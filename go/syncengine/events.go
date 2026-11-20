package syncengine

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"time"

	"vaultshare/go/gdrive"
	"vaultshare/go/settings"
	"vaultshare/go/syncstate"
)

const (
	echoTTL              = 15 * time.Second
	renameCorrelateDelay = 1500 * time.Millisecond
)

type echoEntry struct {
	expiresAt time.Time
	md5Hex    string
	mtimeMs   int64
}

type debounceEntry struct {
	timer     *time.Timer
	firstSeen time.Time
}

type pendingDeleteEntry struct {
	path      string
	size      int64
	md5Hex    string
	sha256Hex string
	timer     *time.Timer
}

type pendingCreateEntry struct {
	path      string
	size      int64
	md5Hex    string
	sha256Hex string
	createdAt time.Time
}

type changeBatch struct {
	id            uint64
	newStartToken string
	pending       map[string]bool
	aborted       bool
}

func normalizeRelPath(p string) string {
	cleaned := strings.ReplaceAll(strings.TrimSpace(p), `\`, `/`)
	cleaned = path.Clean(cleaned)
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	return strings.Trim(cleaned, "/")
}

func isExcludedPath(relPath, configDir string) bool {
	p := normalizeRelPath(relPath)
	if p == "" {
		return true
	}
	if p == ".trash" || strings.HasPrefix(p, ".trash/") {
		return true
	}
	cfg := normalizeRelPath(configDir)
	if cfg != "" && (p == cfg || strings.HasPrefix(p, cfg+"/")) {
		return true
	}
	return false
}

func splitParentAndName(relPath string) (parentDir, name string) {
	p := normalizeRelPath(relPath)
	idx := strings.LastIndexByte(p, '/')
	if idx < 0 {
		return "", p
	}
	return p[:idx], p[idx+1:]
}

func computeHashes(data []byte) (sha256Hex, md5Hex string) {
	s := sha256.Sum256(data)
	m := md5.Sum(data)
	return hex.EncodeToString(s[:]), hex.EncodeToString(m[:])
}

// beginLocalWrite registers an active Vault write/rename BEFORE invoking Obsidian's
// Vault API so synchronous/microtask watcher echoes are immediately suppressed.
// Call the returned finisher after the write completes.
func (e *Engine) beginLocalWrite(relPath string) func(ok bool, md5Hex string, mtimeMs int64) {
	e.echoMu.Lock()
	e.inFlightLocalWrites[relPath]++
	e.echoMu.Unlock()

	var finished bool
	return func(ok bool, md5Hex string, mtimeMs int64) {
		e.echoMu.Lock()
		if finished {
			e.echoMu.Unlock()
			return
		}
		finished = true
		if cur := e.inFlightLocalWrites[relPath] - 1; cur <= 0 {
			delete(e.inFlightLocalWrites, relPath)
		} else {
			e.inFlightLocalWrites[relPath] = cur
		}
		if ok {
			e.recentLocalWrites[relPath] = echoEntry{
				expiresAt: e.now().Add(echoTTL),
				md5Hex:    md5Hex,
				mtimeMs:   mtimeMs,
			}
		}
		e.echoMu.Unlock()
	}
}

// recordLocalEcho suppresses watcher echo events triggered by our own Vault writes.
func (e *Engine) recordLocalEcho(relPath, md5Hex string, mtimeMs int64) {
	e.echoMu.Lock()
	e.recentLocalWrites[relPath] = echoEntry{
		expiresAt: e.now().Add(echoTTL),
		md5Hex:    md5Hex,
		mtimeMs:   mtimeMs,
	}
	e.echoMu.Unlock()
}

func (e *Engine) consumeLocalEcho(relPath string, mtimeMs int64) bool {
	now := e.now()
	e.echoMu.Lock()
	defer e.echoMu.Unlock()
	if e.inFlightLocalWrites[relPath] > 0 {
		return true
	}
	ent, ok := e.recentLocalWrites[relPath]
	if !ok {
		return false
	}
	if now.After(ent.expiresAt) {
		delete(e.recentLocalWrites, relPath)
		return false
	}
	if ent.mtimeMs == 0 || mtimeMs == 0 || ent.mtimeMs == mtimeMs {
		delete(e.recentLocalWrites, relPath)
		return true
	}
	return false
}

func (e *Engine) recordDriveEcho(fileID, md5Hex string) {
	if fileID == "" {
		return
	}
	e.echoMu.Lock()
	e.recentDriveWrites[fileID] = echoEntry{
		expiresAt: e.now().Add(echoTTL),
		md5Hex:    md5Hex,
	}
	e.echoMu.Unlock()
}

func (e *Engine) consumeDriveEcho(fileID, md5Hex string) bool {
	now := e.now()
	e.echoMu.Lock()
	defer e.echoMu.Unlock()
	ent, ok := e.recentDriveWrites[fileID]
	if !ok {
		return false
	}
	if now.After(ent.expiresAt) {
		delete(e.recentDriveWrites, fileID)
		return false
	}
	if ent.md5Hex == "" || md5Hex == "" || strings.EqualFold(ent.md5Hex, md5Hex) {
		delete(e.recentDriveWrites, fileID)
		return true
	}
	return false
}

// OnVaultEvent ingests a local Obsidian Vault or Workspace event in strict FIFO
// order without executing Drive network I/O on the event RPC goroutine.
func (e *Engine) OnVaultEvent(ctx context.Context, ev settings.VaultEventPayload) error {
	e.epochMu.Lock()
	routerActive := e.epochCancel != nil
	e.epochMu.Unlock()

	if routerActive {
		done := make(chan error, 1)
		req := vaultEventReq{ctx: ctx, ev: ev, done: done}
		select {
		case e.eventCh <- req:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	e.eventIngestMu.Lock()
	defer e.eventIngestMu.Unlock()
	return e.processVaultEventOrdered(ctx, ev)
}

func (e *Engine) processVaultEventOrdered(ctx context.Context, ev settings.VaultEventPayload) error {
	relPath := normalizeRelPath(ev.Path)
	if isExcludedPath(relPath, e.vault.ConfigDir()) {
		return nil
	}

	switch ev.Kind {
	case "open":
		if ev.IsFolder {
			return nil
		}
		e.cancelDebounce(relPath)
		e.queue.Enqueue(relPath, PriorityHigh, "open", e.now(), 0)
		return nil

	case "rename":
		oldPath := normalizeRelPath(ev.OldPath)
		if oldPath == "" || oldPath == relPath || isExcludedPath(oldPath, e.vault.ConfigDir()) {
			return nil
		}
		if e.consumeLocalEcho(relPath, ev.MTimeMs) {
			return nil
		}
		if ev.IsFolder {
			e.queue.EnqueueAction(SyncTask{
				Kind:       TaskLocalFolderRename,
				OldPath:    oldPath,
				Path:       relPath,
				Source:     "folder-rename",
				EnqueuedAt: e.now(),
			})
			return nil
		}
		e.cancelDebounce(oldPath)
		e.cancelDebounce(relPath)
		e.consumeInSessionDelete(oldPath)
		e.queue.EnqueueAction(SyncTask{
			Kind:       TaskLocalRename,
			OldPath:    oldPath,
			Path:       relPath,
			Source:     "rename",
			EnqueuedAt: e.now(),
		})
		return nil

	case "create":
		if ev.IsFolder {
			return nil
		}
		if e.consumeLocalEcho(relPath, ev.MTimeMs) {
			return nil
		}
		if e.tryMatchCreateWithPendingDelete(ctx, relPath, ev.Size) {
			return nil
		}
		e.recordPendingCreate(relPath, ev.Size)
		e.scheduleDebouncedEnqueue(relPath, "create")
		return nil

	case "modify":
		if ev.IsFolder {
			return nil
		}
		if e.consumeLocalEcho(relPath, ev.MTimeMs) {
			return nil
		}
		e.scheduleDebouncedEnqueue(relPath, "modify")
		return nil

	case "delete":
		if ev.IsFolder {
			descendants := e.cache.ListDescendantFiles(relPath)
			for _, rec := range descendants {
				e.markInSessionDelete(rec.Path)
				e.queue.Enqueue(rec.Path, PriorityHigh, "folder-delete", e.now(), 0)
			}
			if len(descendants) == 0 {
				e.queue.EnqueueAction(SyncTask{
					Kind:       TaskTrashEmptyFolder,
					Path:       relPath,
					Source:     "folder-delete",
					EnqueuedAt: e.now(),
				})
			}
			return nil
		}
		e.cancelDebounce(relPath)
		e.markInSessionDelete(relPath)
		if e.tryMatchDeleteWithPendingCreate(ctx, relPath) {
			return nil
		}
		e.holdPendingDelete(relPath)
		return nil
	}
	return nil
}

func (e *Engine) trashDriveFolderIfEmpty(ctx context.Context, relDir string) {
	if e.IsPaused() || e.NeedsLogin() {
		return
	}
	relDir = normalizeRelPath(relDir)
	if relDir == "" || isExcludedPath(relDir, e.vault.ConfigDir()) {
		return
	}
	if _, exists, err := e.vault.Stat(ctx, relDir); err != nil || exists {
		return
	}
	if len(e.cache.ListDescendantFiles(relDir)) > 0 {
		return
	}
	for _, fld := range e.cache.GetFoldersByPath(relDir) {
		subFolders, subFiles, err := e.drive.ListChildren(ctx, fld.DriveFolderID)
		if err != nil || len(subFolders) > 0 || len(subFiles) > 0 {
			continue
		}
		e.recordDriveEcho(fld.DriveFolderID, "")
		if err := e.drive.TrashFile(ctx, fld.DriveFolderID); err == nil {
			_ = e.cache.DeleteFolder(ctx, fld.DriveFolderID)
		}
	}
}

func (e *Engine) markInSessionDelete(relPath string) {
	e.echoMu.Lock()
	e.inSessionDeletes[relPath] = e.now().Add(5 * time.Minute)
	e.echoMu.Unlock()
}

func (e *Engine) consumeInSessionDelete(relPath string) bool {
	now := e.now()
	e.echoMu.Lock()
	defer e.echoMu.Unlock()
	exp, ok := e.inSessionDeletes[relPath]
	if !ok {
		return false
	}
	delete(e.inSessionDeletes, relPath)
	return !now.After(exp)
}

func (e *Engine) scheduleDebouncedEnqueue(relPath, source string) {
	cfg := e.getSettings()
	debounce := time.Duration(cfg.LocalDebounceMs) * time.Millisecond
	maxWait := time.Duration(cfg.LocalMaxWaitMs) * time.Millisecond
	now := e.now()

	e.debounceMu.Lock()
	defer e.debounceMu.Unlock()

	if ent, exists := e.debounces[relPath]; exists {
		if ent.timer != nil {
			ent.timer.Stop()
		}
		remainingMax := maxWait - now.Sub(ent.firstSeen)
		wait := min(debounce, max(0, remainingMax))
		ent.timer = time.AfterFunc(wait, func() {
			e.fireDebounced(relPath, source)
		})
		return
	}

	ent := &debounceEntry{firstSeen: now}
	ent.timer = time.AfterFunc(debounce, func() {
		e.fireDebounced(relPath, source)
	})
	e.debounces[relPath] = ent
}

func (e *Engine) fireDebounced(relPath, source string) {
	e.debounceMu.Lock()
	delete(e.debounces, relPath)
	e.debounceMu.Unlock()
	e.queue.Enqueue(relPath, PriorityHigh, source, e.now(), 0)
}

func (e *Engine) cancelDebounce(relPath string) {
	e.debounceMu.Lock()
	if ent, exists := e.debounces[relPath]; exists {
		if ent.timer != nil {
			ent.timer.Stop()
		}
		delete(e.debounces, relPath)
	}
	e.debounceMu.Unlock()
}

// FlushDebounces cancels all pending local debounce timers and enqueues them immediately.
func (e *Engine) FlushDebounces() int {
	e.debounceMu.Lock()
	paths := make([]string, 0, len(e.debounces))
	for p, ent := range e.debounces {
		if ent.timer != nil {
			ent.timer.Stop()
		}
		paths = append(paths, p)
	}
	clear(e.debounces)
	e.debounceMu.Unlock()

	now := e.now()
	for _, p := range paths {
		e.queue.Enqueue(p, PriorityHigh, "flush", now, 0)
	}
	return len(paths)
}

// FlushPendingDeletes releases any held rename-correlation deletes onto the
// High-priority queue immediately (used before app suspend/sleep).
func (e *Engine) FlushPendingDeletes() int {
	e.renameMu.Lock()
	paths := make([]string, 0, len(e.pendingDeletes))
	for p, del := range e.pendingDeletes {
		if del.timer != nil {
			del.timer.Stop()
		}
		paths = append(paths, p)
	}
	clear(e.pendingDeletes)
	e.renameMu.Unlock()

	now := e.now()
	for _, p := range paths {
		e.queue.Enqueue(p, PriorityHigh, "flush-delete", now, 0)
	}
	return len(paths)
}

func (e *Engine) recordPendingCreate(relPath string, size int64) {
	e.renameMu.Lock()
	e.pendingCreates[relPath] = &pendingCreateEntry{
		path:      relPath,
		size:      size,
		createdAt: e.now(),
	}
	// Prune expired entries older than 10 seconds.
	cutoff := e.now().Add(-10 * time.Second)
	for k, v := range e.pendingCreates {
		if v.createdAt.Before(cutoff) {
			delete(e.pendingCreates, k)
		}
	}
	e.renameMu.Unlock()
}

func (e *Engine) holdPendingDelete(relPath string) {
	rec, ok := e.cache.GetFile(relPath)
	if !ok || rec.Local == nil {
		e.queue.Enqueue(relPath, PriorityHigh, "delete", e.now(), 0)
		return
	}

	e.renameMu.Lock()
	if old, exists := e.pendingDeletes[relPath]; exists && old.timer != nil {
		old.timer.Stop()
	}
	ent := &pendingDeleteEntry{
		path:      relPath,
		size:      rec.Local.Size,
		md5Hex:    rec.Local.MD5,
		sha256Hex: rec.Local.SHA256,
	}
	ent.timer = time.AfterFunc(renameCorrelateDelay, func() {
		e.renameMu.Lock()
		cur, stillThere := e.pendingDeletes[relPath]
		if stillThere && cur == ent {
			delete(e.pendingDeletes, relPath)
		} else {
			stillThere = false
		}
		e.renameMu.Unlock()
		if stillThere {
			e.queue.Enqueue(relPath, PriorityHigh, "delete", e.now(), 0)
		}
	})
	e.pendingDeletes[relPath] = ent
	e.renameMu.Unlock()
}

func (e *Engine) tryMatchCreateWithPendingDelete(ctx context.Context, newPath string, size int64) bool {
	e.renameMu.Lock()
	var candidates []*pendingDeleteEntry
	for _, d := range e.pendingDeletes {
		if d.size == size || size == 0 {
			candidates = append(candidates, d)
		}
	}
	e.renameMu.Unlock()
	if len(candidates) == 0 {
		return false
	}

	data, _, err := e.vault.ReadBytes(ctx, newPath)
	if err != nil {
		return false
	}
	shaHex, md5Hex := computeHashes(data)

	e.renameMu.Lock()
	var matched *pendingDeleteEntry
	for _, d := range candidates {
		if cur, ok := e.pendingDeletes[d.path]; ok && cur == d && hashesMatch(cur.sha256Hex, cur.md5Hex, shaHex, md5Hex) {
			matched = cur
			delete(e.pendingDeletes, d.path)
			if cur.timer != nil {
				cur.timer.Stop()
			}
			break
		}
	}
	e.renameMu.Unlock()
	if matched == nil {
		return false
	}

	e.cancelDebounce(newPath)
	e.consumeInSessionDelete(matched.path)
	e.queue.EnqueueAction(SyncTask{
		Kind:       TaskLocalRename,
		OldPath:    matched.path,
		Path:       newPath,
		Source:     "correlated-rename",
		EnqueuedAt: e.now(),
	})
	return true
}

func (e *Engine) tryMatchDeleteWithPendingCreate(ctx context.Context, oldPath string) bool {
	rec, ok := e.cache.GetFile(oldPath)
	if !ok || rec.Local == nil {
		return false
	}
	e.renameMu.Lock()
	var candidates []*pendingCreateEntry
	for _, c := range e.pendingCreates {
		if c.size == rec.Local.Size || c.size == 0 {
			candidates = append(candidates, c)
		}
	}
	e.renameMu.Unlock()
	if len(candidates) == 0 {
		return false
	}

	for _, cand := range candidates {
		data, _, err := e.vault.ReadBytes(ctx, cand.path)
		if err != nil {
			continue
		}
		shaHex, md5Hex := computeHashes(data)
		if !hashesMatch(rec.Local.SHA256, rec.Local.MD5, shaHex, md5Hex) {
			continue
		}
		e.renameMu.Lock()
		delete(e.pendingCreates, cand.path)
		e.renameMu.Unlock()
		e.cancelDebounce(cand.path)
		e.consumeInSessionDelete(oldPath)
		e.queue.EnqueueAction(SyncTask{
			Kind:       TaskLocalRename,
			OldPath:    oldPath,
			Path:       cand.path,
			Source:     "correlated-rename",
			EnqueuedAt: e.now(),
		})
		return true
	}
	return false
}

func hashesMatch(shaA, md5A, shaB, md5B string) bool {
	if shaA != "" && shaB != "" {
		return strings.EqualFold(shaA, shaB)
	}
	if md5A != "" && md5B != "" {
		return strings.EqualFold(md5A, md5B)
	}
	return false
}

func (e *Engine) handleLocalFileRename(ctx context.Context, oldPath, newPath string) error {
	e.cancelDebounce(oldPath)
	e.cancelDebounce(newPath)
	e.consumeInSessionDelete(oldPath)

	return e.withPathLocks(ctx, []string{oldPath, newPath}, func() error {
		return e.handleLocalFileRenameLocked(ctx, oldPath, newPath)
	})
}

func (e *Engine) handleLocalFileRenameLocked(ctx context.Context, oldPath, newPath string) error {
	rec, ok := e.cache.GetFile(oldPath)
	if !ok || rec.LastSynced == nil || rec.LastSynced.DriveFileID == "" || e.IsPaused() || e.NeedsLogin() {
		e.queue.Enqueue(oldPath, PriorityHigh, "rename-old", e.now(), 0)
		e.queue.Enqueue(newPath, PriorityHigh, "rename-new", e.now(), 0)
		return nil
	}

	rootID := e.cache.SyncRoot().RootFolderID
	if rootID == "" {
		e.queue.Enqueue(oldPath, PriorityHigh, "rename-old", e.now(), 0)
		e.queue.Enqueue(newPath, PriorityHigh, "rename-new", e.now(), 0)
		return nil
	}

	newDir, newName := splitParentAndName(newPath)
	newParentID, createdFolders, err := e.drive.EnsureFolderPath(ctx, rootID, newDir)
	if err != nil {
		e.handleIOError(err, "EnsureFolderPath on local rename")
		e.queue.Enqueue(oldPath, PriorityHigh, "rename-fallback", e.now(), 0)
		e.queue.Enqueue(newPath, PriorityHigh, "rename-fallback", e.now(), 0)
		return err
	}
	if len(createdFolders) > 0 {
		_ = e.cache.PutFolders(ctx, createdFolders)
	}

	oldParentID := ""
	if len(rec.DriveVersions) > 0 {
		oldParentID = rec.DriveVersions[0].ParentID
	}
	e.recordDriveEcho(rec.LastSynced.DriveFileID, rec.LastSynced.MD5)
	moved, err := e.drive.MoveRenameFile(ctx, rec.LastSynced.DriveFileID, newParentID, oldParentID, newName)
	if err != nil {
		e.handleIOError(err, "MoveRenameFile on local rename")
		e.queue.Enqueue(oldPath, PriorityHigh, "rename-fallback", e.now(), 0)
		e.queue.Enqueue(newPath, PriorityHigh, "rename-fallback", e.now(), 0)
		return err
	}

	rec.DriveVersions = []syncstate.DriveVersionState{driveFileToState(moved)}
	rec.UpdatedAtMs = e.now().UnixMilli()
	if err := e.cache.MigrateFilePath(ctx, oldPath, newPath, rec); err != nil {
		return err
	}
	e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Renamed++ })
	e.log(settings.LogInfo, "Mirrored local rename to Drive", oldPath+" -> "+newPath)
	return nil
}

func (e *Engine) handleLocalFolderRename(ctx context.Context, oldDir, newDir string) error {
	descendants := e.cache.ListDescendantFiles(oldDir)
	for _, rec := range descendants {
		suffix := strings.TrimPrefix(rec.Path, oldDir)
		newPath := normalizeRelPath(newDir + suffix)
		if err := e.handleLocalFileRename(ctx, rec.Path, newPath); err != nil {
			e.log(settings.LogWarning, "Folder rename child fallback queued", rec.Path)
		}
	}
	return nil
}

// StartupLocalDiff compares Obsidian's in-memory vault.getFiles() stats against
// Cache on startup, matching offline renames and queuing changed paths at High priority.
func (e *Engine) StartupLocalDiff(ctx context.Context) error {
	stats, err := e.vault.ListAllStats(ctx)
	if err != nil {
		return err
	}
	cfgDir := e.vault.ConfigDir()
	present := make(map[string]LocalStat, len(stats))
	for _, st := range stats {
		if st.IsFolder {
			continue
		}
		p := normalizeRelPath(st.Path)
		if isExcludedPath(p, cfgDir) {
			continue
		}
		st.Path = p
		present[p] = st
	}

	cached := e.cache.ListFiles()
	cachedMap := make(map[string]syncstate.FileRecord, len(cached))
	var vanished []syncstate.FileRecord
	for _, rec := range cached {
		cachedMap[rec.Path] = rec
		if _, ok := present[rec.Path]; !ok && rec.Local != nil {
			vanished = append(vanished, rec)
		}
	}

	var newFiles []LocalStat
	for p, st := range present {
		rec, ok := cachedMap[p]
		if !ok || rec.Local == nil {
			newFiles = append(newFiles, st)
			continue
		}
		if rec.Local.Size != st.Size || rec.Local.MtimeMs != st.MTimeMs {
			e.queue.Enqueue(p, PriorityHigh, "startup-diff", e.now(), 0)
		}
	}

	// Offline rename correlation between vanished cached files and newly seen files.
	matchedVanished := make(map[string]bool)
	matchedNew := make(map[string]bool)
	if len(vanished) > 0 && len(newFiles) > 0 {
		for _, nf := range newFiles {
			var sizeMatches []syncstate.FileRecord
			for _, v := range vanished {
				if !matchedVanished[v.Path] && v.Local != nil && v.Local.Size == nf.Size {
					sizeMatches = append(sizeMatches, v)
				}
			}
			if len(sizeMatches) == 0 {
				continue
			}
			data, _, readErr := e.vault.ReadBytes(ctx, nf.Path)
			if readErr != nil {
				continue
			}
			shaHex, md5Hex := computeHashes(data)
			for _, v := range sizeMatches {
				if hashesMatch(v.Local.SHA256, v.Local.MD5, shaHex, md5Hex) {
					matchedVanished[v.Path] = true
					matchedNew[nf.Path] = true
					_ = e.handleLocalFileRename(ctx, v.Path, nf.Path)
					break
				}
			}
		}
	}

	now := e.now()
	for _, nf := range newFiles {
		if !matchedNew[nf.Path] {
			e.queue.Enqueue(nf.Path, PriorityHigh, "startup-new", now, 0)
		}
	}
	for _, v := range vanished {
		if !matchedVanished[v.Path] {
			e.queue.Enqueue(v.Path, PriorityHigh, "startup-missing", now, 0)
		}
	}
	return nil
}

// pollDriveChangesOnce fetches Drive changes since meta["changeToken"], cascades
// folder updates, enqueues affected file paths at High priority, and commits
// newStartPageToken only once all non-errored paths in the batch complete.
func (e *Engine) pollDriveChangesOnce(ctx context.Context) error {
	if e.IsPaused() || e.NeedsLogin() {
		return nil
	}
	e.batchMu.Lock()
	hasActiveBatch := e.activeBatch != nil && len(e.activeBatch.pending) > 0
	e.batchMu.Unlock()
	if hasActiveBatch {
		return nil
	}

	token := e.cache.ChangeToken().StartPageToken
	if token == "" {
		t, err := e.drive.GetStartPageToken(ctx)
		if err != nil {
			e.handleIOError(err, "GetStartPageToken")
			return err
		}
		if err := e.cache.PutChangeToken(ctx, syncstate.ChangeTokenMeta{
			StartPageToken: t,
			UpdatedAtMs:    e.now().UnixMilli(),
		}); err != nil {
			return err
		}
		return nil
	}

	page, err := e.drive.ListChanges(ctx, token)
	if err != nil {
		e.handleIOError(err, "ListChanges")
		return err
	}

	rootID := e.cache.SyncRoot().RootFolderID
	cfgDir := e.vault.ConfigDir()
	affectedPaths := make(map[string]bool)

	// Pass 1: Process folder transitions first (check foldersByID before change.File
	// because removed: true omits change.File).
	for _, ch := range page.Changes {
		if prevFolder, knownFolder := e.cache.GetFolder(ch.FileID); knownFolder {
			if ch.Removed || ch.File == nil || ch.File.Trashed {
				prevFolder.Trashed = true
				_ = e.cache.PutFolders(ctx, []syncstate.FolderRecord{prevFolder})
				for _, f := range e.cache.ListDescendantFiles(prevFolder.Path) {
					affectedPaths[f.Path] = true
				}
				continue
			}
			e.handleKnownDriveFolderChange(ctx, prevFolder, *ch.File, affectedPaths)
			continue
		}

		if ch.File != nil && ch.File.MimeType == gdrive.FolderMimeType && !ch.File.Trashed && !ch.Removed {
			parentRel, inScope := e.cache.ResolveFolderPath(ch.File.FirstParent())
			if !inScope {
				continue
			}
			newPath := ch.File.Name
			if parentRel != "" {
				newPath = parentRel + "/" + ch.File.Name
			}
			if isExcludedPath(newPath, cfgDir) {
				continue
			}
			fr := syncstate.FolderRecord{
				DriveFolderID: ch.File.ID,
				ParentID:      ch.File.FirstParent(),
				Name:          ch.File.Name,
				Path:          newPath,
				ModifiedTime:  ch.File.ModifiedTime,
			}
			_ = e.cache.PutFolders(ctx, []syncstate.FolderRecord{fr})
			// Folder may have been moved into sync root with existing children.
			e.collectDriveSubtreePaths(ctx, fr.DriveFolderID, fr.Path, affectedPaths)
		}
	}

	// Pass 2: Process file changes.
	for _, ch := range page.Changes {
		if _, isFolder := e.cache.GetFolder(ch.FileID); isFolder || ch.FileID == rootID {
			continue
		}
		if ch.File != nil && ch.File.MimeType == gdrive.FolderMimeType {
			continue
		}
		if ch.File != nil && gdrive.IsGoogleNativeDoc(ch.File.MimeType) {
			e.logOnceNativeDoc(ch.File.ID, ch.File.Name)
			continue
		}

		md5Hex := ""
		if ch.File != nil {
			md5Hex = ch.File.MD5Checksum
		}
		if e.consumeDriveEcho(ch.FileID, md5Hex) {
			continue
		}

		if prevRec, known := e.cache.GetFileByDriveID(ch.FileID); known {
			if ch.Removed || ch.File == nil || ch.File.Trashed {
				affectedPaths[prevRec.Path] = true
				continue
			}
			parentRel, inScope := e.cache.ResolveFolderPath(ch.File.FirstParent())
			if !inScope {
				// Moved out of sync root.
				affectedPaths[prevRec.Path] = true
				continue
			}
			newPath := ch.File.Name
			if parentRel != "" {
				newPath = parentRel + "/" + ch.File.Name
			}
			if newPath != prevRec.Path && !isExcludedPath(newPath, cfgDir) {
				// Drive-side rename/move detected.
				if err := e.mirrorDriveRenameLocally(ctx, prevRec, *ch.File, newPath); err == nil {
					continue
				}
			}
			affectedPaths[prevRec.Path] = true
			continue
		}

		if ch.Removed || ch.File == nil || ch.File.Trashed {
			continue
		}
		parentRel, inScope := e.cache.ResolveFolderPath(ch.File.FirstParent())
		if !inScope {
			continue
		}
		relPath := ch.File.Name
		if parentRel != "" {
			relPath = parentRel + "/" + ch.File.Name
		}
		if !isExcludedPath(relPath, cfgDir) {
			affectedPaths[relPath] = true
		}
	}

	if len(affectedPaths) == 0 {
		if page.NewStartPageToken != "" && page.NewStartPageToken != token {
			return e.cache.PutChangeToken(ctx, syncstate.ChangeTokenMeta{
				StartPageToken: page.NewStartPageToken,
				UpdatedAtMs:    e.now().UnixMilli(),
			})
		}
		return nil
	}

	e.batchMu.Lock()
	e.nextBatchID++
	bid := e.nextBatchID
	pending := make(map[string]bool, len(affectedPaths))
	for p := range affectedPaths {
		pending[p] = true
	}
	e.activeBatch = &changeBatch{
		id:            bid,
		newStartToken: page.NewStartPageToken,
		pending:       pending,
	}
	e.batchMu.Unlock()

	now := e.now()
	for p := range affectedPaths {
		e.queue.Enqueue(p, PriorityHigh, "drive-change", now, bid)
	}
	return nil
}

func (e *Engine) handleKnownDriveFolderChange(
	ctx context.Context,
	prev syncstate.FolderRecord,
	cur gdrive.DriveFile,
	affectedPaths map[string]bool,
) {
	parentRel, inScope := e.cache.ResolveFolderPath(cur.FirstParent())
	if !inScope {
		// Moved out of sync root: mark trashed in cache and queue descendants.
		prev.Trashed = true
		_ = e.cache.PutFolders(ctx, []syncstate.FolderRecord{prev})
		for _, f := range e.cache.ListDescendantFiles(prev.Path) {
			affectedPaths[f.Path] = true
		}
		return
	}

	newPath := cur.Name
	if parentRel != "" {
		newPath = parentRel + "/" + cur.Name
	}
	oldPath := prev.Path
	prev.ParentID = cur.FirstParent()
	prev.Name = cur.Name
	prev.Path = newPath
	prev.ModifiedTime = cur.ModifiedTime
	prev.Trashed = false

	updatedFolders := []syncstate.FolderRecord{prev}
	if newPath != oldPath {
		for _, f := range e.cache.ListFolders() {
			if f.DriveFolderID == prev.DriveFolderID {
				continue
			}
			if strings.HasPrefix(f.Path, oldPath+"/") {
				f.Path = newPath + strings.TrimPrefix(f.Path, oldPath)
				updatedFolders = append(updatedFolders, f)
			}
		}
	}
	_ = e.cache.PutFolders(ctx, updatedFolders)

	if newPath != oldPath {
		for _, rec := range e.cache.ListDescendantFiles(oldPath) {
			suffix := strings.TrimPrefix(rec.Path, oldPath)
			dstPath := normalizeRelPath(newPath + suffix)
			oldChildPath := rec.Path
			err := e.withPathLocks(ctx, []string{oldChildPath, dstPath}, func() error {
				latestRec, ok := e.cache.GetFile(oldChildPath)
				if !ok {
					return nil
				}
				endEcho := e.beginLocalWrite(dstPath)
				if err := e.vault.Rename(ctx, oldChildPath, dstPath); err != nil {
					endEcho(false, "", 0)
					return err
				}
				endEcho(true, "", 0)
				return e.cache.MigrateFilePath(ctx, oldChildPath, dstPath, latestRec)
			})
			if err != nil {
				affectedPaths[oldChildPath] = true
				affectedPaths[dstPath] = true
			}
		}
	}
}

func (e *Engine) collectDriveSubtreePaths(ctx context.Context, folderID, relDir string, out map[string]bool) {
	subfolders, files, err := e.drive.ListChildren(ctx, folderID)
	if err != nil {
		return
	}
	cfgDir := e.vault.ConfigDir()
	var recs []syncstate.FolderRecord
	for _, sf := range subfolders {
		p := sf.Name
		if relDir != "" {
			p = relDir + "/" + sf.Name
		}
		if isExcludedPath(p, cfgDir) {
			continue
		}
		recs = append(recs, syncstate.FolderRecord{
			DriveFolderID: sf.ID,
			ParentID:      folderID,
			Name:          sf.Name,
			Path:          p,
			ModifiedTime:  sf.ModifiedTime,
		})
	}
	if len(recs) > 0 {
		_ = e.cache.PutFolders(ctx, recs)
	}
	for _, f := range files {
		p := f.Name
		if relDir != "" {
			p = relDir + "/" + f.Name
		}
		if !isExcludedPath(p, cfgDir) {
			out[p] = true
		}
	}
	for _, sf := range recs {
		e.collectDriveSubtreePaths(ctx, sf.DriveFolderID, sf.Path, out)
	}
}

func (e *Engine) mirrorDriveRenameLocally(ctx context.Context, prev syncstate.FileRecord, moved gdrive.DriveFile, newPath string) error {
	oldPath := prev.Path
	return e.withPathLocks(ctx, []string{oldPath, newPath}, func() error {
		latest, ok := e.cache.GetFile(oldPath)
		if !ok {
			return nil
		}
		endEcho := e.beginLocalWrite(newPath)
		if err := e.vault.Rename(ctx, oldPath, newPath); err != nil {
			endEcho(false, "", 0)
			return err
		}
		endEcho(true, "", 0)
		latest.DriveVersions = []syncstate.DriveVersionState{driveFileToState(moved)}
		latest.UpdatedAtMs = e.now().UnixMilli()
		if err := e.cache.MigrateFilePath(ctx, oldPath, newPath, latest); err != nil {
			return err
		}
		e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Renamed++ })
		e.log(settings.LogInfo, "Mirrored Drive rename locally", oldPath+" -> "+newPath)
		return nil
	})
}

func (e *Engine) completeBatchTask(ctx context.Context, path string, batchIDs []uint64, taskErr error) {
	if len(batchIDs) == 0 {
		return
	}
	var tokenToPersist string
	e.batchMu.Lock()
	b := e.activeBatch
	if b != nil {
		matched := false
		for _, id := range batchIDs {
			if id == b.id {
				matched = true
				break
			}
		}
		if matched {
			if taskErr != nil {
				b.aborted = true
				e.activeBatch = nil
			} else {
				delete(b.pending, path)
				if len(b.pending) == 0 {
					tokenToPersist = b.newStartToken
					e.activeBatch = nil
				}
			}
		}
	}
	e.batchMu.Unlock()

	if tokenToPersist != "" {
		_ = e.cache.PutChangeToken(ctx, syncstate.ChangeTokenMeta{
			StartPageToken: tokenToPersist,
			UpdatedAtMs:    e.now().UnixMilli(),
		})
	}
}

func (e *Engine) logOnceNativeDoc(fileID, name string) {
	e.echoMu.Lock()
	seen := e.warnedNativeDocs[fileID]
	if !seen {
		e.warnedNativeDocs[fileID] = true
	}
	e.echoMu.Unlock()
	if !seen {
		e.log(settings.LogWarning, "Skipped unsupported Google Docs native item", name)
	}
}

func driveFileToState(f gdrive.DriveFile) syncstate.DriveVersionState {
	return syncstate.DriveVersionState{
		FileID:         f.ID,
		ParentID:       f.FirstParent(),
		Name:           f.Name,
		MimeType:       f.MimeType,
		ModifiedTime:   f.ModifiedTime,
		Size:           f.ByteSize(),
		MD5:            strings.ToLower(strings.TrimSpace(f.MD5Checksum)),
		SHA256:         strings.ToLower(strings.TrimSpace(f.SHA256Checksum)),
		HeadRevisionID: f.HeadRevisionID,
		Trashed:        f.Trashed,
	}
}

package syncengine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"

	"vaultshare/go/gdrive"
	"vaultshare/go/merge"
	"vaultshare/go/settings"
	"vaultshare/go/syncstate"
)

type scanFolderItem struct {
	folderID string
	relDir   string
}

// runFullScan walks the entire Drive sync root and local vault paths in randomized
// order inside coordinatorLoop (serialized with pollDriveChangesOnce), refreshes
// FolderRecords, and enqueues diverged or unknown paths at PriorityBulk.
//
// Full Scan is strictly a discovery pass: it never emits unlocked per-file state
// discrepancy warnings; resolvePath (holding the exclusive per-path lock with
// fresh Vault, Drive, and Cache state) is the sole authority for file judgments.
func (e *Engine) runFullScan(ctx context.Context) error {
	if e.IsPaused() || e.NeedsLogin() {
		return nil
	}

	e.fullScanMu.Lock()
	if e.fullScanRunning {
		e.fullScanMu.Unlock()
		return nil
	}
	e.fullScanRunning = true
	e.fullScanMu.Unlock()
	defer func() {
		e.fullScanMu.Lock()
		e.fullScanRunning = false
		e.fullScanMu.Unlock()
		e.metrics.FinishFullScan(e.now())
		e.emitGlobalStatusIfChanged()
	}()

	rootID, err := e.ensureSyncRootID(ctx)
	if err != nil {
		e.handleIOError(err, "EnsureSyncRoot")
		return err
	}

	e.metrics.StartFullScan(e.now())
	meta := e.cache.FullScan()
	meta.LastStartedAtMs = e.now().UnixMilli()
	_ = e.cache.PutFullScan(ctx, meta)
	e.log(settings.LogInfo, "Full Scan started", "")
	e.emitGlobalStatusIfChanged()

	stats, err := e.vault.ListAllStats(ctx)
	if err != nil {
		e.log(settings.LogError, "Full Scan failed to list vault files", err.Error())
		return err
	}
	cfgDir := e.vault.ConfigDir()
	// Track only path presence so local-only files absent on Drive are discovered
	// without keeping a stale Go-side copy of LocalStat metadata.
	localPaths := make(map[string]bool, len(stats))
	for _, st := range stats {
		if st.IsFolder {
			continue
		}
		p := normalizeRelPath(st.Path)
		if p == "" || isExcludedPath(p, cfgDir) {
			continue
		}
		localPaths[p] = true
	}

	totalEstimate := len(localPaths)
	scanned := 0
	e.metrics.UpdateFullScan(scanned, totalEstimate, e.now())

	seenDrivePaths := make(map[string]bool, len(localPaths))
	stack := []scanFolderItem{{folderID: rootID, relDir: ""}}
	visitedFolders := make(map[string]bool)

	for len(stack) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.IsPaused() || e.NeedsLogin() {
			e.log(settings.LogInfo, "Full Scan paused before completion", "")
			return nil
		}

		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visitedFolders[cur.folderID] {
			continue
		}
		visitedFolders[cur.folderID] = true

		childFolders, childFiles, err := e.drive.ListChildren(ctx, cur.folderID)
		if err != nil {
			e.handleIOError(err, "FullScan ListChildren "+cur.relDir)
			return err
		}

		if err := e.processScanFolders(ctx, cur, childFolders, cfgDir, &stack); err != nil {
			return err
		}

		e.processScanFiles(ctx, cur, childFiles, cfgDir, localPaths, seenDrivePaths, &scanned, &totalEstimate)
		e.metrics.UpdateFullScan(scanned, totalEstimate, e.now())
	}

	// Enqueue local-only files absent on Drive and orphan DB records.
	localOnly := make([]string, 0)
	for relPath := range localPaths {
		if !seenDrivePaths[relPath] {
			localOnly = append(localOnly, relPath)
		}
	}
	rand.Shuffle(len(localOnly), func(i, j int) {
		localOnly[i], localOnly[j] = localOnly[j], localOnly[i]
	})
	for _, relPath := range localOnly {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.IsPaused() || e.NeedsLogin() {
			e.log(settings.LogInfo, "Full Scan paused before completion", "")
			return nil
		}
		scanned++
		if e.isPathBusy(relPath) {
			continue
		}
		if _, liveFound, statErr := e.vault.Stat(ctx, relPath); statErr != nil || !liveFound {
			continue
		}
		e.queue.Enqueue(relPath, PriorityBulk, "fullscan", e.now(), 0)
		e.metrics.UpdateFullScan(scanned, max(totalEstimate, scanned), e.now())
	}

	for _, rec := range e.cache.ListFiles() {
		if !seenDrivePaths[rec.Path] && !localPaths[rec.Path] && !e.isPathBusy(rec.Path) {
			e.queue.Enqueue(rec.Path, PriorityBulk, "fullscan", e.now(), 0)
		}
	}

	meta = e.cache.FullScan()
	meta.Initialized = true
	meta.LastCompletedAtMs = e.now().UnixMilli()
	if err := e.cache.PutFullScan(ctx, meta); err != nil {
		return err
	}
	e.log(settings.LogInfo, "Full Scan completed", fmt.Sprintf("scanned=%d", scanned))
	return nil
}

func (e *Engine) processScanFolders(
	ctx context.Context,
	cur scanFolderItem,
	childFolders []gdrive.DriveFile,
	cfgDir string,
	stack *[]scanFolderItem,
) error {
	byName := make(map[string][]gdrive.DriveFile, len(childFolders))
	for _, fld := range childFolders {
		if fld.Trashed {
			continue
		}
		relChild := fld.Name
		if cur.relDir != "" {
			relChild = cur.relDir + "/" + fld.Name
		}
		relChild = normalizeRelPath(relChild)
		if relChild == "" || isExcludedPath(relChild, cfgDir) {
			continue
		}
		byName[relChild] = append(byName[relChild], fld)
	}

	folderRecs := make([]syncstate.FolderRecord, 0, len(byName))
	next := make([]scanFolderItem, 0, len(childFolders))
	for relChild, group := range byName {
		if len(group) > 1 {
			sort.Slice(group, func(i, j int) bool {
				return group[i].ID < group[j].ID
			})
			e.log(settings.LogWarning, "Duplicate Drive folder detected", relChild)
		}
		primary := group[0]
		folderRecs = append(folderRecs, syncstate.FolderRecord{
			DriveFolderID: primary.ID,
			ParentID:      cur.folderID,
			Name:          primary.Name,
			Path:          relChild,
			ModifiedTime:  primary.ModifiedTime,
		})
		for _, fld := range group {
			next = append(next, scanFolderItem{
				folderID: fld.ID,
				relDir:   relChild,
			})
		}
	}
	if len(folderRecs) > 0 {
		if err := e.cache.PutFolders(ctx, folderRecs); err != nil {
			return err
		}
	}

	rand.Shuffle(len(next), func(i, j int) {
		next[i], next[j] = next[j], next[i]
	})
	*stack = append(*stack, next...)
	return nil
}

func (e *Engine) processScanFiles(
	ctx context.Context,
	cur scanFolderItem,
	childFiles []gdrive.DriveFile,
	cfgDir string,
	localPaths map[string]bool,
	seenDrivePaths map[string]bool,
	scanned *int,
	totalEstimate *int,
) {
	byPath := make(map[string][]gdrive.DriveFile, len(childFiles))
	paths := make([]string, 0, len(childFiles))
	for _, f := range childFiles {
		if f.Trashed {
			continue
		}
		relPath := f.Name
		if cur.relDir != "" {
			relPath = cur.relDir + "/" + f.Name
		}
		relPath = normalizeRelPath(relPath)
		if relPath == "" || isExcludedPath(relPath, cfgDir) {
			continue
		}
		if _, exists := byPath[relPath]; !exists {
			paths = append(paths, relPath)
		}
		byPath[relPath] = append(byPath[relPath], f)
	}

	rand.Shuffle(len(paths), func(i, j int) {
		paths[i], paths[j] = paths[j], paths[i]
	})

	for _, relPath := range paths {
		group := byPath[relPath]
		seenDrivePaths[relPath] = true
		if !localPaths[relPath] {
			*totalEstimate++
		}
		*scanned++

		if e.isPathBusy(relPath) {
			continue
		}

		// Quick filter only: if Sync State DB, live Vault stat, and Drive entry
		// match LastSynced cleanly, skip; otherwise enqueue for locked resolvePath.
		rec, hasRec := e.cache.GetFile(relPath)
		localStat, localFound, statErr := e.vault.Stat(ctx, relPath)
		if statErr != nil {
			continue
		}

		if len(group) == 1 && hasRec && rec.LastSynced != nil && rec.Conflict == nil && localFound {
			df := group[0]
			if df.ID == rec.LastSynced.DriveFileID &&
				df.HeadRevisionID == rec.LastSynced.LastSyncedRevisionID &&
				df.ModifiedTime == rec.LastSynced.DriveModifiedTime &&
				localStat.Size == rec.LastSynced.Size &&
				localStat.MTimeMs == rec.LastSynced.LocalMtimeMs {
				if merge.IsTextMergePath(relPath) {
					e.auditUnflaggedConflictMarkers(ctx, relPath)
				}
				continue
			}
		}

		e.queue.Enqueue(relPath, PriorityBulk, "fullscan", e.now(), 0)
	}
}

// auditUnflaggedConflictMarkers checks an otherwise clean text file during Full
// Scan for unresolved conflict markers and flags ConflictMarkers under pathLock.
func (e *Engine) auditUnflaggedConflictMarkers(ctx context.Context, relPath string) {
	data, _, err := e.vault.ReadBytes(ctx, relPath)
	if err != nil || !merge.HasConflictMarkersWith(string(data), merge.StyleForPath(relPath)) {
		return
	}
	_ = e.withPathLock(ctx, relPath, func() error {
		rec, ok := e.cache.GetFile(relPath)
		if !ok || rec.Conflict != nil {
			return nil
		}
		freshData, _, readErr := e.vault.ReadBytes(ctx, relPath)
		if readErr != nil {
			return nil
		}
		mc := e.detectMarkerConflict(relPath, freshData)
		if mc == nil {
			return nil
		}
		rec.Conflict = mc
		rec.UpdatedAtMs = e.now().UnixMilli()
		if err := e.cache.PutFile(ctx, rec); err != nil {
			return err
		}
		e.emitGlobalStatusIfChanged()
		return nil
	})
}

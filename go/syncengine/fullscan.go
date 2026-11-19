package syncengine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"

	"vaultshare/go/gdrive"
	"vaultshare/go/settings"
	"vaultshare/go/syncstate"
)

type scanFolderItem struct {
	folderID string
	relDir   string
}

// runFullScan walks the entire Drive sync root and local vault snapshot in
// randomized folder/file order, refreshes the FolderRecord cache, logs state
// anomalies, and enqueues any diverged paths at PriorityBulk.
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
	localByPath := make(map[string]LocalStat, len(stats))
	for _, st := range stats {
		if st.IsFolder {
			continue
		}
		p := normalizeRelPath(st.Path)
		if p == "" || isExcludedPath(p, cfgDir) {
			continue
		}
		localByPath[p] = LocalStat{
			Path:     p,
			Size:     st.Size,
			MTimeMs:  st.MTimeMs,
			IsFolder: false,
		}
	}

	totalEstimate := len(localByPath)
	scanned := 0
	e.metrics.UpdateFullScan(scanned, totalEstimate, e.now())

	seenDrivePaths := make(map[string]bool, len(localByPath))
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

		e.processScanFiles(cur, childFiles, cfgDir, localByPath, seenDrivePaths, &scanned, &totalEstimate)
		e.metrics.UpdateFullScan(scanned, totalEstimate, e.now())
	}

	// Compare local-only files (absent on Drive) and orphan DB records.
	localOnly := make([]string, 0)
	for relPath := range localByPath {
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
		st := localByPath[relPath]
		rec, hasRec := e.cache.GetFile(relPath)
		if hasRec && rec.LastSynced != nil && st.MTimeMs < rec.LastSynced.LocalMtimeMs {
			e.log(settings.LogWarning, "Local file older than last synced state", relPath)
		} else {
			e.log(settings.LogInfo, "Local file absent on Drive", relPath)
		}
		e.queue.Enqueue(relPath, PriorityBulk, "fullscan", e.now(), 0)
		e.metrics.UpdateFullScan(scanned, max(totalEstimate, scanned), e.now())
	}

	for _, rec := range e.cache.ListFiles() {
		if !seenDrivePaths[rec.Path] {
			if _, ok := localByPath[rec.Path]; !ok {
				e.queue.Enqueue(rec.Path, PriorityBulk, "fullscan", e.now(), 0)
			}
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
	cur scanFolderItem,
	childFiles []gdrive.DriveFile,
	cfgDir string,
	localByPath map[string]LocalStat,
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
		localStat, localFound := localByPath[relPath]
		if !localFound {
			*totalEstimate++
		}
		*scanned++

		rec, hasRec := e.cache.GetFile(relPath)
		needsEnqueue := false

		if len(group) > 1 {
			e.log(settings.LogWarning, "Duplicate Drive files at path", relPath)
			needsEnqueue = true
		}

		df := group[0]
		if !hasRec || rec.LastSynced == nil {
			e.log(settings.LogInfo, "Unknown Drive file discovered", relPath)
			needsEnqueue = true
		} else {
			if rec.Conflict != nil {
				needsEnqueue = true
			}
			if df.ID != rec.LastSynced.DriveFileID ||
				df.HeadRevisionID != rec.LastSynced.LastSyncedRevisionID ||
				df.ModifiedTime != rec.LastSynced.DriveModifiedTime {
				if df.ModifiedTime > rec.LastSynced.DriveModifiedTime {
					e.log(settings.LogInfo, "Drive file newer than last synced", relPath)
				}
				needsEnqueue = true
			}
			if !localFound ||
				localStat.Size != rec.LastSynced.Size ||
				localStat.MTimeMs != rec.LastSynced.LocalMtimeMs {
				if localFound && localStat.MTimeMs < rec.LastSynced.LocalMtimeMs {
					e.log(settings.LogWarning, "Local file older than last synced state", relPath)
				}
				needsEnqueue = true
			}
		}

		if needsEnqueue {
			e.queue.Enqueue(relPath, PriorityBulk, "fullscan", e.now(), 0)
		}
	}
}

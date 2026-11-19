package syncengine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"vaultshare/go/gdrive"
	"vaultshare/go/merge"
	"vaultshare/go/settings"
	"vaultshare/go/syncstate"
)

func (e *Engine) resolvePath(ctx context.Context, relPath string) error {
	// TODO(slow-binary-create): Open Question #1 — add size/mtime stability probe
	// before uploading newly created non-text files.
	relPath = normalizeRelPath(relPath)
	if isExcludedPath(relPath, e.vault.ConfigDir()) {
		return nil
	}

	rootID := e.cache.SyncRoot().RootFolderID
	if rootID == "" {
		return errors.New("sync root folder not initialized")
	}

	// Phase 1: Parallel metadata gather (zero open-file lock).
	var (
		localStat  LocalStat
		localFound bool
		localErr   error
		driveFiles []gdrive.DriveFile
		driveErr   error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		localStat, localFound, localErr = e.vault.Stat(ctx, relPath)
	}()
	go func() {
		defer wg.Done()
		driveFiles, driveErr = e.findDriveFilesAtPath(ctx, rootID, relPath)
	}()
	wg.Wait()

	if localErr != nil {
		return localErr
	}
	if driveErr != nil {
		e.handleIOError(driveErr, "FindByPath "+relPath)
		return driveErr
	}

	rec, hasRec := e.cache.GetFile(relPath)
	if !hasRec {
		rec = syncstate.FileRecord{Path: relPath}
	}

	// Fast-path exit when both sides match LastSynced.
	if rec.LastSynced != nil && localFound && len(driveFiles) == 1 {
		df := driveFiles[0]
		if !df.Trashed &&
			localStat.Size == rec.LastSynced.Size &&
			localStat.MTimeMs == rec.LastSynced.LocalMtimeMs &&
			df.ID == rec.LastSynced.DriveFileID &&
			df.HeadRevisionID == rec.LastSynced.LastSyncedRevisionID &&
			df.ModifiedTime == rec.LastSynced.DriveModifiedTime &&
			rec.Conflict == nil {
			return nil
		}
	}

	var localData []byte
	var localSHA, localMD5 string
	if localFound {
		var err error
		localData, localStat, err = e.vault.ReadBytes(ctx, relPath)
		if err != nil {
			return err
		}
		localSHA, localMD5 = computeHashes(localData)
	}

	// Phase 2: Presence & change classification.
	driveStates := make([]syncstate.DriveVersionState, 0, len(driveFiles))
	for _, df := range driveFiles {
		driveStates = append(driveStates, driveFileToState(df))
	}

	// Case 1: Absent on both sides -> clean up stale record.
	if !localFound && len(driveFiles) == 0 {
		if hasRec {
			return e.cache.DeleteFile(ctx, relPath)
		}
		return nil
	}

	// Case 2: Only local exists.
	if localFound && len(driveFiles) == 0 {
		if rec.LastSynced == nil {
			return e.pushNewLocalFile(ctx, rootID, relPath, localStat, localData, localSHA, localMD5)
		}
		// Previously synced Drive file is missing: verify whether Drive trashed it or local was also edited.
		localChanged := !localMatchesBaseline(localStat, localSHA, localMD5, rec.LastSynced)
		driveTrashed, trashErr := e.isDriveFileTrashed(ctx, rec.LastSynced.DriveFileID)
		if trashErr != nil {
			e.handleIOError(trashErr, "check Drive trash "+relPath)
			return trashErr
		}
		if driveTrashed && !localChanged {
			if err := e.vault.Trash(ctx, relPath); err != nil {
				return err
			}
			_ = e.cache.DeleteFile(ctx, relPath)
			e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Trashed++ })
			e.log(settings.LogInfo, "Mirrored Drive trash locally", relPath)
			return nil
		}
		return e.flagMissingConflict(ctx, &rec, &localStat, localSHA, localMD5, nil, "Drive file missing or modify-vs-delete conflict")
	}

	// Case 3: Only Drive exists (1 or N versions).
	if !localFound && len(driveFiles) > 0 {
		if rec.LastSynced == nil {
			if len(driveFiles) == 1 {
				return e.pullNewDriveFile(ctx, relPath, driveFiles[0])
			}
			// Duplicate Drive files exist with no local copy: run N-version resolution.
			return e.resolveConflictVersions(ctx, rootID, relPath, &rec, false, LocalStat{}, nil, "", "", driveFiles)
		}
		driveChanged := len(driveFiles) > 1 || !driveMatchesBaseline(driveFiles[0], rec.LastSynced)
		localConfirmedTrash := e.consumeInSessionDelete(relPath)
		if !localConfirmedTrash {
			inTrash, err := e.vault.ExistsInTrash(ctx, relPath, rec.LastSynced.Size)
			if err == nil && inTrash {
				localConfirmedTrash = true
			}
		}
		if localConfirmedTrash && !driveChanged {
			if err := e.drive.TrashFile(ctx, driveFiles[0].ID); err != nil {
				e.handleIOError(err, "TrashFile "+relPath)
				return err
			}
			e.recordDriveEcho(driveFiles[0].ID, "")
			_ = e.cache.DeleteFile(ctx, relPath)
			e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Trashed++ })
			e.log(settings.LogInfo, "Mirrored local trash to Drive", relPath)
			if parentDir, _ := splitParentAndName(relPath); parentDir != "" {
				e.trashDriveFolderIfEmpty(ctx, parentDir)
			}
			return nil
		}
		return e.flagMissingConflict(ctx, &rec, nil, "", "", driveStates, "Local file missing or modify-vs-delete conflict")
	}

	// Case 4: Both sides exist. Check if all versions are identical.
	if len(driveFiles) == 1 && versionsIdentical(localStat, localSHA, localMD5, driveFiles[0]) {
		return e.saveSyncedRecord(ctx, relPath, localStat, localSHA, localMD5, driveFiles[0])
	}

	// Single-sided change fast paths when LastSynced exists and there are no Drive duplicates.
	if rec.LastSynced != nil && len(driveFiles) == 1 {
		localChanged := !localMatchesBaseline(localStat, localSHA, localMD5, rec.LastSynced)
		driveChanged := !driveMatchesBaseline(driveFiles[0], rec.LastSynced)
		switch {
		case localChanged && !driveChanged:
			return e.pushExistingLocalFile(ctx, relPath, driveFiles[0].ID, localStat, localData, localSHA, localMD5)
		case !localChanged && driveChanged:
			return e.pullExistingDriveFile(ctx, relPath, driveFiles[0])
		}
	}

	return e.resolveConflictVersions(ctx, rootID, relPath, &rec, true, localStat, localData, localSHA, localMD5, driveFiles)
}

func (e *Engine) findDriveFilesAtPath(ctx context.Context, rootID, relPath string) ([]gdrive.DriveFile, error) {
	dir, name := splitParentAndName(relPath)
	var parentIDs []string
	if dir == "" {
		parentIDs = []string{rootID}
	} else {
		folders := e.cache.GetFoldersByPath(dir)
		for _, f := range folders {
			if !f.Trashed {
				parentIDs = append(parentIDs, f.DriveFolderID)
			}
		}
		if len(parentIDs) == 0 {
			return nil, nil
		}
	}
	return e.drive.FindInParents(ctx, parentIDs, name)
}

func (e *Engine) isDriveFileTrashed(ctx context.Context, driveFileID string) (bool, error) {
	if driveFileID == "" {
		return false, nil
	}
	meta, found, err := e.drive.GetFileMeta(ctx, driveFileID)
	if err != nil {
		return false, err
	}
	if !found || meta.Trashed {
		return true, nil
	}
	rootID := e.cache.SyncRoot().RootFolderID
	parentID := meta.FirstParent()
	for hops := 0; hops < 64 && parentID != "" && parentID != rootID; hops++ {
		if fld, ok := e.cache.GetFolder(parentID); ok {
			if fld.Trashed {
				return true, nil
			}
			if fld.ParentID != "" {
				parentID = fld.ParentID
				continue
			}
		}
		pm, pFound, pErr := e.drive.GetFileMeta(ctx, parentID)
		if pErr != nil {
			return false, pErr
		}
		if !pFound || pm.Trashed {
			return true, nil
		}
		parentID = pm.FirstParent()
	}
	return false, nil
}

func (e *Engine) pushNewLocalFile(
	ctx context.Context,
	rootID, relPath string,
	st LocalStat,
	data []byte,
	shaHex, md5Hex string,
) error {
	dir, name := splitParentAndName(relPath)
	parentID := rootID
	if dir != "" {
		var created []syncstate.FolderRecord
		var err error
		parentID, created, err = e.drive.EnsureFolderPath(ctx, rootID, dir)
		if err != nil {
			e.handleIOError(err, "EnsureFolderPath "+dir)
			return err
		}
		if len(created) > 0 {
			_ = e.cache.PutFolders(ctx, created)
		}
	}

	mimeType := gdrive.ContentMimeType(name)
	created, err := e.drive.CreateFile(ctx, parentID, name, mimeType, st.MTimeMs, data)
	if err != nil {
		e.handleIOError(err, "CreateFile "+relPath)
		return err
	}
	e.recordDriveEcho(created.ID, md5Hex)
	e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Pushed++ })
	e.log(settings.LogInfo, "Uploaded new file to Drive", relPath)
	return e.saveSyncedRecord(ctx, relPath, st, shaHex, md5Hex, created)
}

func (e *Engine) pushExistingLocalFile(
	ctx context.Context,
	relPath, driveFileID string,
	st LocalStat,
	data []byte,
	shaHex, md5Hex string,
) error {
	mimeType := gdrive.ContentMimeType(relPath)
	updated, err := e.drive.UpdateFile(ctx, driveFileID, mimeType, st.MTimeMs, data)
	if err != nil {
		e.handleIOError(err, "UpdateFile "+relPath)
		return err
	}
	e.recordDriveEcho(updated.ID, md5Hex)
	e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Pushed++ })
	e.log(settings.LogInfo, "Pushed local edit to Drive", relPath)
	return e.saveSyncedRecord(ctx, relPath, st, shaHex, md5Hex, updated)
}

func (e *Engine) pullNewDriveFile(ctx context.Context, relPath string, df gdrive.DriveFile) error {
	return e.pullDriveFile(ctx, relPath, df, true)
}

func (e *Engine) pullExistingDriveFile(ctx context.Context, relPath string, df gdrive.DriveFile) error {
	return e.pullDriveFile(ctx, relPath, df, false)
}

func (e *Engine) pullDriveFile(ctx context.Context, relPath string, df gdrive.DriveFile, isNew bool) error {
	// Download Drive bytes BEFORE acquiring open-file lock.
	data, err := e.drive.DownloadFile(ctx, df.ID)
	if err != nil {
		e.handleIOError(err, "DownloadFile "+relPath)
		return err
	}
	shaHex, md5Hex := computeHashes(data)

	var newStat LocalStat
	err = e.vault.WithOpenFileLock(ctx, relPath, func(lockCtx context.Context) error {
		var writeErr error
		newStat, writeErr = e.writeLocalContent(lockCtx, relPath, data)
		return writeErr
	})
	if err != nil {
		return err
	}
	e.recordLocalEcho(relPath, md5Hex, newStat.MTimeMs)
	e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Pulled++ })
	if isNew {
		e.log(settings.LogInfo, "Pulled new file from Drive", relPath)
	} else {
		e.log(settings.LogInfo, "Pulled remote update from Drive", relPath)
	}
	return e.saveSyncedRecord(ctx, relPath, newStat, shaHex, md5Hex, df)
}

func (e *Engine) writeLocalContent(ctx context.Context, relPath string, data []byte) (LocalStat, error) {
	if merge.IsTextMergePath(relPath) {
		return e.vault.WriteText(ctx, relPath, string(data))
	}
	return e.vault.WriteBytes(ctx, relPath, data)
}

func (e *Engine) resolveConflictVersions(
	ctx context.Context,
	rootID, relPath string,
	rec *syncstate.FileRecord,
	hasLocal bool,
	localStat LocalStat,
	localData []byte,
	localSHA, localMD5 string,
	driveFiles []gdrive.DriveFile,
) error {
	cfg := e.getSettings()
	rules := settings.NormalizeSyncResolution(cfg.SyncResolution)

	for _, rule := range rules {
		switch rule {
		case settings.ResolutionMerge:
			if cfg.DisableTextMerge || !merge.IsTextMergePath(relPath) {
				continue
			}
			handled, err := e.tryMergeConflict(ctx, rootID, relPath, rec, hasLocal, localStat, localData, driveFiles)
			if err != nil {
				return err
			}
			if handled {
				return nil
			}

		case settings.ResolutionKeepNewest, settings.ResolutionKeepOldest:
			keepNewest := rule == settings.ResolutionKeepNewest
			return e.resolveByTimestamp(ctx, relPath, hasLocal, localStat, localData, localSHA, localMD5, driveFiles, keepNewest)

		case settings.ResolutionKeepLocal:
			if !hasLocal {
				continue
			}
			return e.resolveKeepLocal(ctx, rootID, relPath, localStat, localData, localSHA, localMD5, driveFiles)

		case settings.ResolutionKeepDrive:
			// Keep Drive fails/falls through when multiple Drive versions exist.
			if len(driveFiles) != 1 {
				continue
			}
			if err := e.pullExistingDriveFile(ctx, relPath, driveFiles[0]); err != nil {
				return err
			}
			e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.KeptDrive++ })
			return nil

		case settings.ResolutionManual:
			return e.flagVersionsConflict(ctx, rec, hasLocal, localStat, localSHA, localMD5, driveFiles)
		}
	}
	return e.flagVersionsConflict(ctx, rec, hasLocal, localStat, localSHA, localMD5, driveFiles)
}

func (e *Engine) tryMergeConflict(
	ctx context.Context,
	rootID, relPath string,
	rec *syncstate.FileRecord,
	hasLocal bool,
	localStat LocalStat,
	localData []byte,
	driveFiles []gdrive.DriveFile,
) (bool, error) {
	style := merge.StyleForPath(relPath)

	// Pre-fetch all Drive contents + optional base revision BEFORE locking the open file.
	driveContents := make([]string, len(driveFiles))
	for i, df := range driveFiles {
		b, err := e.drive.DownloadFile(ctx, df.ID)
		if err != nil {
			e.handleIOError(err, "DownloadFile for merge "+relPath)
			return false, err
		}
		driveContents[i] = string(b)
	}

	var baseText string
	var hasBase bool
	if rec.LastSynced != nil && rec.LastSynced.LastSyncedRevisionID != "" {
		baseBytes, ok, err := e.drive.DownloadRevision(ctx, rec.LastSynced.DriveFileID, rec.LastSynced.LastSyncedRevisionID)
		if err != nil {
			e.handleIOError(err, "DownloadRevision for merge "+relPath)
			return false, err
		}
		if ok {
			baseText = string(baseBytes)
			hasBase = true
		}
	}

	var mergedRes merge.ReconcileResult
	var mergedBytes []byte
	var mergedSHA, mergedMD5 string
	var postWriteStat LocalStat

	err := e.vault.WithOpenFileLock(ctx, relPath, func(lockCtx context.Context) error {
		curLocal := string(localData)
		if hasLocal {
			freshBytes, _, readErr := e.vault.ReadBytes(lockCtx, relPath)
			if readErr == nil {
				curLocal = string(freshBytes)
			}
		}

		versions := make([]string, 0, len(driveContents)+1)
		if hasLocal {
			versions = append(versions, curLocal)
		}
		versions = append(versions, driveContents...)

		mergedRes = merge.ReconcileVersions(baseText, hasBase, versions, style)
		if mergedRes.Kind == "keepBoth" {
			return nil
		}
		mergedBytes = []byte(mergedRes.Content)
		mergedSHA, mergedMD5 = computeHashes(mergedBytes)
		if !hasLocal || curLocal != mergedRes.Content {
			var writeErr error
			postWriteStat, writeErr = e.vault.WriteText(lockCtx, relPath, mergedRes.Content)
			return writeErr
		}
		postWriteStat = localStat
		return nil
	})
	if err != nil {
		return false, err
	}
	if mergedRes.Kind == "keepBoth" {
		return false, nil
	}

	e.recordLocalEcho(relPath, mergedMD5, postWriteStat.MTimeMs)

	// Re-check primary Drive headRevisionId right before UpdateFile to avoid silent overwrite.
	primary := pickPrimaryDriveFile(driveFiles)
	latestMeta, found, metaErr := e.drive.GetFileMeta(ctx, primary.ID)
	if metaErr != nil {
		e.handleIOError(metaErr, "GetFileMeta pre-upload "+relPath)
		return false, metaErr
	}
	if found && latestMeta.HeadRevisionID != primary.HeadRevisionID {
		// Remote moved while merging; re-enqueue so fresh remote revision merges cleanly.
		e.queue.Enqueue(relPath, PriorityHigh, "merge-retry", e.now(), 0)
		return true, nil
	}

	updatedDrive, err := e.drive.UpdateFile(ctx, primary.ID, gdrive.ContentMimeType(relPath), postWriteStat.MTimeMs, mergedBytes)
	if err != nil {
		e.handleIOError(err, "UpdateFile post-merge "+relPath)
		return false, err
	}
	e.recordDriveEcho(updatedDrive.ID, mergedMD5)

	// Trash any extra Drive duplicates folded into the merged result.
	for _, df := range driveFiles {
		if df.ID != primary.ID {
			_ = e.drive.TrashFile(ctx, df.ID)
			e.recordDriveEcho(df.ID, "")
		}
	}

	var conflict *syncstate.ConflictState
	if merge.HasConflictMarkersWith(mergedRes.Content, style) {
		conflict = &syncstate.ConflictState{
			Kind:         syncstate.ConflictMarkers,
			DetectedAtMs: e.now().UnixMilli(),
			Summary:      "Merged text contains unresolved conflict markers",
		}
		e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.ManualFlagged++ })
	}
	e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.Merged++ })
	e.log(settings.LogInfo, "Merged conflicting text versions", relPath)

	_ = rootID
	return true, e.saveRecordWithConflict(ctx, relPath, postWriteStat, mergedSHA, mergedMD5, updatedDrive, conflict)
}

func (e *Engine) resolveByTimestamp(
	ctx context.Context,
	relPath string,
	hasLocal bool,
	localStat LocalStat,
	localData []byte,
	localSHA, localMD5 string,
	driveFiles []gdrive.DriveFile,
	keepNewest bool,
) error {
	bestDrive := driveFiles[0]
	bestDriveMs := parseDriveTimeMs(bestDrive.ModifiedTime)
	for _, df := range driveFiles[1:] {
		ms := parseDriveTimeMs(df.ModifiedTime)
		if (keepNewest && ms > bestDriveMs) || (!keepNewest && ms < bestDriveMs) {
			bestDrive = df
			bestDriveMs = ms
		}
	}

	localWins := false
	if hasLocal {
		if (keepNewest && localStat.MTimeMs >= bestDriveMs) || (!keepNewest && localStat.MTimeMs <= bestDriveMs) {
			localWins = true
		}
	}

	if localWins {
		primary := pickPrimaryDriveFile(driveFiles)
		if err := e.pushExistingLocalFile(ctx, relPath, primary.ID, localStat, localData, localSHA, localMD5); err != nil {
			return err
		}
		for _, df := range driveFiles {
			if df.ID != primary.ID {
				_ = e.drive.TrashFile(ctx, df.ID)
				e.recordDriveEcho(df.ID, "")
			}
		}
	} else {
		if err := e.pullExistingDriveFile(ctx, relPath, bestDrive); err != nil {
			return err
		}
		for _, df := range driveFiles {
			if df.ID != bestDrive.ID {
				_ = e.drive.TrashFile(ctx, df.ID)
				e.recordDriveEcho(df.ID, "")
			}
		}
	}

	e.metrics.IncResolution(func(c *settings.ResolutionCounters) {
		if keepNewest {
			c.KeptNewer++
		} else {
			c.KeptOlder++
		}
	})
	return nil
}

func (e *Engine) resolveKeepLocal(
	ctx context.Context,
	rootID, relPath string,
	localStat LocalStat,
	localData []byte,
	localSHA, localMD5 string,
	driveFiles []gdrive.DriveFile,
) error {
	if len(driveFiles) == 0 {
		if err := e.pushNewLocalFile(ctx, rootID, relPath, localStat, localData, localSHA, localMD5); err != nil {
			return err
		}
	} else {
		primary := pickPrimaryDriveFile(driveFiles)
		if err := e.pushExistingLocalFile(ctx, relPath, primary.ID, localStat, localData, localSHA, localMD5); err != nil {
			return err
		}
		for _, df := range driveFiles {
			if df.ID != primary.ID {
				_ = e.drive.TrashFile(ctx, df.ID)
				e.recordDriveEcho(df.ID, "")
			}
		}
	}
	e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.KeptLocal++ })
	return nil
}

func (e *Engine) flagMissingConflict(
	ctx context.Context,
	rec *syncstate.FileRecord,
	localStat *LocalStat,
	localSHA, localMD5 string,
	driveStates []syncstate.DriveVersionState,
	summary string,
) error {
	if localStat != nil {
		rec.Local = &syncstate.LocalFileState{
			Size:    localStat.Size,
			MtimeMs: localStat.MTimeMs,
			MD5:     localMD5,
			SHA256:  localSHA,
		}
	} else {
		rec.Local = nil
	}
	rec.DriveVersions = driveStates
	rec.Conflict = &syncstate.ConflictState{
		Kind:         syncstate.ConflictMissing,
		DetectedAtMs: e.now().UnixMilli(),
		Summary:      summary,
	}
	rec.UpdatedAtMs = e.now().UnixMilli()
	if err := e.cache.PutFile(ctx, *rec); err != nil {
		return err
	}
	e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.ManualFlagged++ })
	e.emitGlobalStatusIfChanged()
	return nil
}

func (e *Engine) flagVersionsConflict(
	ctx context.Context,
	rec *syncstate.FileRecord,
	hasLocal bool,
	localStat LocalStat,
	localSHA, localMD5 string,
	driveFiles []gdrive.DriveFile,
) error {
	if hasLocal {
		rec.Local = &syncstate.LocalFileState{
			Size:    localStat.Size,
			MtimeMs: localStat.MTimeMs,
			MD5:     localMD5,
			SHA256:  localSHA,
		}
	} else {
		rec.Local = nil
	}
	rec.DriveVersions = make([]syncstate.DriveVersionState, 0, len(driveFiles))
	for _, df := range driveFiles {
		rec.DriveVersions = append(rec.DriveVersions, driveFileToState(df))
	}
	rec.Conflict = &syncstate.ConflictState{
		Kind:         syncstate.ConflictVersions,
		DetectedAtMs: e.now().UnixMilli(),
		Summary:      fmt.Sprintf("%d Drive version(s) in conflict", len(driveFiles)),
	}
	rec.UpdatedAtMs = e.now().UnixMilli()
	if err := e.cache.PutFile(ctx, *rec); err != nil {
		return err
	}
	e.metrics.IncResolution(func(c *settings.ResolutionCounters) { c.ManualFlagged++ })
	e.emitGlobalStatusIfChanged()
	return nil
}

func (e *Engine) saveSyncedRecord(
	ctx context.Context,
	relPath string,
	st LocalStat,
	shaHex, md5Hex string,
	df gdrive.DriveFile,
) error {
	return e.saveRecordWithConflict(ctx, relPath, st, shaHex, md5Hex, df, nil)
}

func (e *Engine) saveRecordWithConflict(
	ctx context.Context,
	relPath string,
	st LocalStat,
	shaHex, md5Hex string,
	df gdrive.DriveFile,
	conflict *syncstate.ConflictState,
) error {
	nowMs := e.now().UnixMilli()
	rec := syncstate.FileRecord{
		Path:     relPath,
		MimeType: df.MimeType,
		LastSynced: &syncstate.SyncedBaseline{
			DriveFileID:          df.ID,
			LastSyncedRevisionID: df.HeadRevisionID,
			MD5:                  md5Hex,
			SHA256:               shaHex,
			Size:                 st.Size,
			LocalMtimeMs:         st.MTimeMs,
			DriveModifiedTime:    df.ModifiedTime,
			SyncedAtMs:           nowMs,
		},
		Local: &syncstate.LocalFileState{
			Size:    st.Size,
			MtimeMs: st.MTimeMs,
			MD5:     md5Hex,
			SHA256:  shaHex,
		},
		DriveVersions: []syncstate.DriveVersionState{driveFileToState(df)},
		Conflict:      conflict,
		UpdatedAtMs:   nowMs,
	}
	if err := e.cache.PutFile(ctx, rec); err != nil {
		return err
	}
	e.emitGlobalStatusIfChanged()
	return nil
}

func pickPrimaryDriveFile(files []gdrive.DriveFile) gdrive.DriveFile {
	copied := slices.Clone(files)
	sort.Slice(copied, func(i, j int) bool {
		if copied[i].ModifiedTime != copied[j].ModifiedTime {
			return copied[i].ModifiedTime > copied[j].ModifiedTime
		}
		return copied[i].ID < copied[j].ID
	})
	return copied[0]
}

func versionsIdentical(st LocalStat, shaHex, md5Hex string, df gdrive.DriveFile) bool {
	if df.SHA256Checksum != "" && shaHex != "" {
		return strings.EqualFold(df.SHA256Checksum, shaHex)
	}
	if df.MD5Checksum != "" && md5Hex != "" {
		return strings.EqualFold(df.MD5Checksum, md5Hex)
	}
	return st.Size == df.ByteSize() && parseDriveTimeMs(df.ModifiedTime) == st.MTimeMs
}

func localMatchesBaseline(st LocalStat, shaHex, md5Hex string, b *syncstate.SyncedBaseline) bool {
	if b.SHA256 != "" && shaHex != "" {
		return strings.EqualFold(b.SHA256, shaHex)
	}
	if b.MD5 != "" && md5Hex != "" {
		return strings.EqualFold(b.MD5, md5Hex)
	}
	return st.Size == b.Size && st.MTimeMs == b.LocalMtimeMs
}

func driveMatchesBaseline(df gdrive.DriveFile, b *syncstate.SyncedBaseline) bool {
	if df.HeadRevisionID != "" && b.LastSyncedRevisionID != "" {
		return df.ID == b.DriveFileID && df.HeadRevisionID == b.LastSyncedRevisionID
	}
	if df.MD5Checksum != "" && b.MD5 != "" {
		return df.ID == b.DriveFileID && strings.EqualFold(df.MD5Checksum, b.MD5)
	}
	return df.ID == b.DriveFileID && df.ModifiedTime == b.DriveModifiedTime && df.ByteSize() == b.Size
}

func parseDriveTimeMs(rfc string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, rfc); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, rfc); err == nil {
		return t.UnixMilli()
	}
	return 0
}

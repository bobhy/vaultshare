package syncstate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"vaultshare/go/settings"
)

const maxFolderWalkDepth = 128

// Cache maintains an in-memory write-through projection of all 3 IndexedDB stores
// and secondary lookup indexes. All Store+Cache writes serialize through writeGate
// (a context-aware channel semaphore, never holding sync.Mutex across Store I/O)
// so IndexedDB and memory never diverge under concurrent callers.
type Cache struct {
	store     Store
	writeGate chan struct{}

	mu            sync.RWMutex
	filesByPath   map[string]FileRecord
	driveIDToPath map[string]string
	foldersByID   map[string]FolderRecord
	foldersByPath map[string][]FolderRecord
	conflictPaths map[string]string

	syncRoot    SyncRootMeta
	changeToken ChangeTokenMeta
	engineState EngineStateMeta
	clientID    ClientIDMeta
	fullScan    FullScanMeta
}

// NewCache creates an empty Cache backed by store. Call LoadAll before use.
func NewCache(store Store) *Cache {
	return &Cache{
		store:         store,
		writeGate:     make(chan struct{}, 1),
		filesByPath:   make(map[string]FileRecord),
		driveIDToPath: make(map[string]string),
		foldersByID:   make(map[string]FolderRecord),
		foldersByPath: make(map[string][]FolderRecord),
		conflictPaths: make(map[string]string),
	}
}

func (c *Cache) withWriteGate(ctx context.Context, fn func() error) error {
	select {
	case c.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.writeGate }()
	return fn()
}

// LoadAll reads all rows from Store and populates in-memory indexes.
func (c *Cache) LoadAll(ctx context.Context) error {
	return c.withWriteGate(ctx, func() error {
		return c.loadAllLocked(ctx)
	})
}

func (c *Cache) loadAllLocked(ctx context.Context) error {
	snap, err := c.store.LoadAll(ctx)
	if err != nil {
		return err
	}
	if snap == nil {
		snap = &Snapshot{}
	}

	filesByPath := make(map[string]FileRecord, len(snap.Files))
	driveIDToPath := make(map[string]string, len(snap.Files))
	conflictPaths := make(map[string]string)

	for _, rec := range snap.Files {
		NormalizeFileRecord(&rec)
		filesByPath[rec.Path] = cloneFileRecord(rec)
		for _, id := range rec.DriveFileIDs {
			driveIDToPath[id] = rec.Path
		}
		if rec.Conflict != nil && rec.Conflict.Kind != "" {
			conflictPaths[rec.Path] = rec.Conflict.Kind
		}
	}

	foldersByID := make(map[string]FolderRecord, len(snap.Folders))
	for _, f := range snap.Folders {
		foldersByID[f.DriveFolderID] = f
	}
	foldersByPath := rebuildFoldersByPath(foldersByID)

	var syncRoot SyncRootMeta
	var changeToken ChangeTokenMeta
	var engineState EngineStateMeta
	var clientID ClientIDMeta
	var fullScan FullScanMeta

	if err := unmarshalMeta(snap.Meta, MetaKeySyncRoot, &syncRoot); err != nil {
		return err
	}
	if err := unmarshalMeta(snap.Meta, MetaKeyChangeToken, &changeToken); err != nil {
		return err
	}
	if err := unmarshalMeta(snap.Meta, MetaKeyEngineState, &engineState); err != nil {
		return err
	}
	if err := unmarshalMeta(snap.Meta, MetaKeyClientID, &clientID); err != nil {
		return err
	}
	if err := unmarshalMeta(snap.Meta, MetaKeyFullScan, &fullScan); err != nil {
		return err
	}

	c.mu.Lock()
	c.filesByPath = filesByPath
	c.driveIDToPath = driveIDToPath
	c.foldersByID = foldersByID
	c.foldersByPath = foldersByPath
	c.conflictPaths = conflictPaths
	c.syncRoot = syncRoot
	c.changeToken = changeToken
	c.engineState = engineState
	c.clientID = clientID
	c.fullScan = fullScan
	c.mu.Unlock()
	return nil
}

func unmarshalMeta(meta map[string]json.RawMessage, key string, dst any) error {
	raw, ok := meta[key]
	if !ok || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode meta[%q]: %w", key, err)
	}
	return nil
}

func rebuildFoldersByPath(byID map[string]FolderRecord) map[string][]FolderRecord {
	byPath := make(map[string][]FolderRecord)
	for _, f := range byID {
		if f.Trashed {
			continue
		}
		byPath[f.Path] = append(byPath[f.Path], f)
	}
	for p, slice := range byPath {
		sort.Slice(slice, func(i, j int) bool {
			return slice[i].DriveFolderID < slice[j].DriveFolderID
		})
		byPath[p] = slice
	}
	return byPath
}

// GetFile returns a cloned FileRecord by vault path.
func (c *Cache) GetFile(path string) (FileRecord, bool) {
	c.mu.RLock()
	rec, ok := c.filesByPath[path]
	c.mu.RUnlock()
	if !ok {
		return FileRecord{}, false
	}
	return cloneFileRecord(rec), true
}

// GetFileByDriveID resolves a Drive file ID via the in-memory secondary index.
func (c *Cache) GetFileByDriveID(driveFileID string) (FileRecord, bool) {
	c.mu.RLock()
	path, ok := c.driveIDToPath[driveFileID]
	var rec FileRecord
	if ok {
		rec, ok = c.filesByPath[path]
	}
	c.mu.RUnlock()
	if !ok {
		return FileRecord{}, false
	}
	return cloneFileRecord(rec), true
}

// ListFiles returns all cached FileRecords sorted by Path.
func (c *Cache) ListFiles() []FileRecord {
	c.mu.RLock()
	out := make([]FileRecord, 0, len(c.filesByPath))
	for _, rec := range c.filesByPath {
		out = append(out, cloneFileRecord(rec))
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// ListDescendantFiles returns all cached FileRecords under dirPath ("" matches all).
func (c *Cache) ListDescendantFiles(dirPath string) []FileRecord {
	prefix := strings.Trim(dirPath, "/")
	c.mu.RLock()
	var out []FileRecord
	for p, rec := range c.filesByPath {
		if prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/") {
			out = append(out, cloneFileRecord(rec))
		}
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// PutFile persists rec to Store first, then updates in-memory maps and indexes.
func (c *Cache) PutFile(ctx context.Context, rec FileRecord) error {
	return c.withWriteGate(ctx, func() error {
		NormalizeFileRecord(&rec)
		if err := c.store.PutFile(ctx, rec); err != nil {
			return err
		}

		c.mu.Lock()
		if old, exists := c.filesByPath[rec.Path]; exists {
			c.removeFileIndexesLocked(old)
		}
		c.filesByPath[rec.Path] = cloneFileRecord(rec)
		c.addFileIndexesLocked(rec)
		c.mu.Unlock()
		return nil
	})
}

// DeleteFile deletes path from Store first, then updates in-memory maps and indexes.
func (c *Cache) DeleteFile(ctx context.Context, path string) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.DeleteFile(ctx, path); err != nil {
			return err
		}

		c.mu.Lock()
		if old, exists := c.filesByPath[path]; exists {
			c.removeFileIndexesLocked(old)
			delete(c.filesByPath, path)
		}
		c.mu.Unlock()
		return nil
	})
}

// MigrateFilePath atomically moves oldPath to newPath in Store first, then updates
// filesByPath, driveIDToPath, and conflictPaths in memory.
func (c *Cache) MigrateFilePath(ctx context.Context, oldPath, newPath string, updated FileRecord) error {
	return c.withWriteGate(ctx, func() error {
		updated.Path = newPath
		NormalizeFileRecord(&updated)
		if err := c.store.MigrateFilePath(ctx, oldPath, newPath, updated); err != nil {
			return err
		}

		c.mu.Lock()
		if old, exists := c.filesByPath[oldPath]; exists {
			c.removeFileIndexesLocked(old)
			delete(c.filesByPath, oldPath)
		}
		if prevTarget, exists := c.filesByPath[newPath]; exists {
			c.removeFileIndexesLocked(prevTarget)
		}
		c.filesByPath[newPath] = cloneFileRecord(updated)
		c.addFileIndexesLocked(updated)
		c.mu.Unlock()
		return nil
	})
}

func (c *Cache) addFileIndexesLocked(rec FileRecord) {
	for _, id := range rec.DriveFileIDs {
		c.driveIDToPath[id] = rec.Path
	}
	if rec.Conflict != nil && rec.Conflict.Kind != "" {
		c.conflictPaths[rec.Path] = rec.Conflict.Kind
	}
}

func (c *Cache) removeFileIndexesLocked(rec FileRecord) {
	for _, id := range rec.DriveFileIDs {
		if c.driveIDToPath[id] == rec.Path {
			delete(c.driveIDToPath, id)
		}
	}
	delete(c.conflictPaths, rec.Path)
}

// ConflictCounts returns O(1) pending conflict counts grouped by kind.
func (c *Cache) ConflictCounts() settings.ConflictCounts {
	var counts settings.ConflictCounts
	c.mu.RLock()
	for _, kind := range c.conflictPaths {
		switch kind {
		case ConflictMarkers:
			counts.Markers++
		case ConflictVersions:
			counts.Versions++
		case ConflictMissing:
			counts.Missing++
		}
		counts.Total++
	}
	c.mu.RUnlock()
	return counts
}

// ListConflicts returns all FileRecords currently carrying a non-nil Conflict, sorted by Path.
func (c *Cache) ListConflicts() []FileRecord {
	c.mu.RLock()
	out := make([]FileRecord, 0, len(c.conflictPaths))
	for p := range c.conflictPaths {
		if rec, ok := c.filesByPath[p]; ok && rec.Conflict != nil {
			out = append(out, cloneFileRecord(rec))
		}
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

// GetFolder returns a FolderRecord by Drive folder ID.
func (c *Cache) GetFolder(driveFolderID string) (FolderRecord, bool) {
	c.mu.RLock()
	f, ok := c.foldersByID[driveFolderID]
	c.mu.RUnlock()
	return f, ok
}

// GetFoldersByPath returns all non-trashed FolderRecords registered at relPath.
func (c *Cache) GetFoldersByPath(relPath string) []FolderRecord {
	c.mu.RLock()
	list := slices.Clone(c.foldersByPath[relPath])
	c.mu.RUnlock()
	return list
}

// ListFolders returns all cached FolderRecords sorted by Path then DriveFolderID.
func (c *Cache) ListFolders() []FolderRecord {
	c.mu.RLock()
	out := make([]FolderRecord, 0, len(c.foldersByID))
	for _, f := range c.foldersByID {
		out = append(out, f)
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].DriveFolderID < out[j].DriveFolderID
	})
	return out
}

// PutFolders persists folders in one Store transaction and updates folder indexes.
func (c *Cache) PutFolders(ctx context.Context, folders []FolderRecord) error {
	if len(folders) == 0 {
		return nil
	}
	return c.withWriteGate(ctx, func() error {
		if err := c.store.PutFolders(ctx, folders); err != nil {
			return err
		}

		c.mu.Lock()
		for _, f := range folders {
			c.foldersByID[f.DriveFolderID] = f
		}
		c.foldersByPath = rebuildFoldersByPath(c.foldersByID)
		c.mu.Unlock()
		return nil
	})
}

// DeleteFolder removes driveFolderID from Store and updates folder indexes.
func (c *Cache) DeleteFolder(ctx context.Context, driveFolderID string) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.DeleteFolder(ctx, driveFolderID); err != nil {
			return err
		}

		c.mu.Lock()
		delete(c.foldersByID, driveFolderID)
		c.foldersByPath = rebuildFoldersByPath(c.foldersByID)
		c.mu.Unlock()
		return nil
	})
}

// ResolveFolderPath walks ParentID links in memory from folderID up to the
// configured syncRoot.RootFolderID, returning the vault-relative path and
// whether folderID is inside the non-trashed sync root hierarchy.
func (c *Cache) ResolveFolderPath(folderID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	rootID := c.syncRoot.RootFolderID
	if rootID == "" {
		return "", false
	}
	if folderID == rootID {
		return "", true
	}

	var segments []string
	cur := folderID
	for range maxFolderWalkDepth {
		if cur == rootID {
			slices.Reverse(segments)
			return strings.Join(segments, "/"), true
		}
		f, ok := c.foldersByID[cur]
		if !ok || f.Trashed {
			return "", false
		}
		segments = append(segments, f.Name)
		cur = f.ParentID
	}
	return "", false
}

// SyncRoot returns the cached SyncRootMeta.
func (c *Cache) SyncRoot() SyncRootMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.syncRoot
}

// PutSyncRoot persists SyncRootMeta and updates the cache.
func (c *Cache) PutSyncRoot(ctx context.Context, meta SyncRootMeta) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.PutMeta(ctx, MetaKeySyncRoot, meta); err != nil {
			return err
		}
		c.mu.Lock()
		c.syncRoot = meta
		c.mu.Unlock()
		return nil
	})
}

// ChangeToken returns the cached ChangeTokenMeta.
func (c *Cache) ChangeToken() ChangeTokenMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.changeToken
}

// PutChangeToken persists ChangeTokenMeta and updates the cache.
func (c *Cache) PutChangeToken(ctx context.Context, meta ChangeTokenMeta) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.PutMeta(ctx, MetaKeyChangeToken, meta); err != nil {
			return err
		}
		c.mu.Lock()
		c.changeToken = meta
		c.mu.Unlock()
		return nil
	})
}

// EngineState returns the cached EngineStateMeta.
func (c *Cache) EngineState() EngineStateMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.engineState
}

// PutEngineState persists EngineStateMeta and updates the cache.
func (c *Cache) PutEngineState(ctx context.Context, meta EngineStateMeta) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.PutMeta(ctx, MetaKeyEngineState, meta); err != nil {
			return err
		}
		c.mu.Lock()
		c.engineState = meta
		c.mu.Unlock()
		return nil
	})
}

// ClientID returns the cached ClientIDMeta.
func (c *Cache) ClientID() ClientIDMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientID
}

// PutClientID persists ClientIDMeta and updates the cache.
func (c *Cache) PutClientID(ctx context.Context, meta ClientIDMeta) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.PutMeta(ctx, MetaKeyClientID, meta); err != nil {
			return err
		}
		c.mu.Lock()
		c.clientID = meta
		c.mu.Unlock()
		return nil
	})
}

// FullScan returns the cached FullScanMeta.
func (c *Cache) FullScan() FullScanMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fullScan
}

// PutFullScan persists FullScanMeta and updates the cache.
func (c *Cache) PutFullScan(ctx context.Context, meta FullScanMeta) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.PutMeta(ctx, MetaKeyFullScan, meta); err != nil {
			return err
		}
		c.mu.Lock()
		c.fullScan = meta
		c.mu.Unlock()
		return nil
	})
}

// ResetSyncScope clears files, folders, syncRoot, changeToken, and fullScan
// in Store and Cache while preserving clientId and engineState.
func (c *Cache) ResetSyncScope(ctx context.Context) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.ResetSyncScope(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		c.filesByPath = make(map[string]FileRecord)
		c.driveIDToPath = make(map[string]string)
		c.foldersByID = make(map[string]FolderRecord)
		c.foldersByPath = make(map[string][]FolderRecord)
		c.conflictPaths = make(map[string]string)
		c.syncRoot = SyncRootMeta{}
		c.changeToken = ChangeTokenMeta{}
		c.fullScan = FullScanMeta{}
		c.mu.Unlock()
		return nil
	})
}

// ClearAll clears all 3 stores and resets all in-memory maps and metadata.
func (c *Cache) ClearAll(ctx context.Context) error {
	return c.withWriteGate(ctx, func() error {
		if err := c.store.ClearAll(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		c.filesByPath = make(map[string]FileRecord)
		c.driveIDToPath = make(map[string]string)
		c.foldersByID = make(map[string]FolderRecord)
		c.foldersByPath = make(map[string][]FolderRecord)
		c.conflictPaths = make(map[string]string)
		c.syncRoot = SyncRootMeta{}
		c.changeToken = ChangeTokenMeta{}
		c.engineState = EngineStateMeta{}
		c.clientID = ClientIDMeta{}
		c.fullScan = FullScanMeta{}
		c.mu.Unlock()
		return nil
	})
}

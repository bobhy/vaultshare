package syncstate

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"sync"
)

// MemoryStore implements Store in memory for pure-Go unit tests without IndexedDB.
type MemoryStore struct {
	mu      sync.Mutex
	files   map[string]FileRecord
	folders map[string]FolderRecord
	meta    map[string]json.RawMessage
}

// NewMemoryStore constructs an initialized empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		files:   make(map[string]FileRecord),
		folders: make(map[string]FolderRecord),
		meta:    make(map[string]json.RawMessage),
	}
}

// LoadAll returns a deep copy of all stored rows across the 3 stores.
func (m *MemoryStore) LoadAll(_ context.Context) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	files := make([]FileRecord, 0, len(m.files))
	for _, rec := range m.files {
		files = append(files, cloneFileRecord(rec))
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	folders := make([]FolderRecord, 0, len(m.folders))
	for _, f := range m.folders {
		folders = append(folders, f)
	}
	sort.Slice(folders, func(i, j int) bool {
		return folders[i].DriveFolderID < folders[j].DriveFolderID
	})

	meta := make(map[string]json.RawMessage, len(m.meta))
	for k, v := range m.meta {
		meta[k] = slices.Clone(v)
	}

	return &Snapshot{
		Files:   files,
		Folders: folders,
		Meta:    meta,
	}, nil
}

// PutFile stores a normalized clone of rec under rec.Path.
func (m *MemoryStore) PutFile(_ context.Context, rec FileRecord) error {
	NormalizeFileRecord(&rec)
	m.mu.Lock()
	m.files[rec.Path] = cloneFileRecord(rec)
	m.mu.Unlock()
	return nil
}

// DeleteFile removes the FileRecord for path if present.
func (m *MemoryStore) DeleteFile(_ context.Context, path string) error {
	m.mu.Lock()
	delete(m.files, path)
	m.mu.Unlock()
	return nil
}

// MigrateFilePath atomically deletes oldPath and writes updated under newPath.
func (m *MemoryStore) MigrateFilePath(_ context.Context, oldPath, newPath string, updated FileRecord) error {
	updated.Path = newPath
	NormalizeFileRecord(&updated)
	m.mu.Lock()
	delete(m.files, oldPath)
	m.files[newPath] = cloneFileRecord(updated)
	m.mu.Unlock()
	return nil
}

// PutFolders stores all supplied FolderRecords atomically.
func (m *MemoryStore) PutFolders(_ context.Context, folders []FolderRecord) error {
	m.mu.Lock()
	for _, f := range folders {
		m.folders[f.DriveFolderID] = f
	}
	m.mu.Unlock()
	return nil
}

// DeleteFolder removes driveFolderID if present.
func (m *MemoryStore) DeleteFolder(_ context.Context, driveFolderID string) error {
	m.mu.Lock()
	delete(m.folders, driveFolderID)
	m.mu.Unlock()
	return nil
}

// PutMeta JSON-encodes and stores value under key.
func (m *MemoryStore) PutMeta(_ context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.meta[key] = raw
	m.mu.Unlock()
	return nil
}

// ResetSyncScope clears files, folders, syncRoot, changeToken, and fullScan
// while retaining clientId and engineState.
func (m *MemoryStore) ResetSyncScope(_ context.Context) error {
	m.mu.Lock()
	m.files = make(map[string]FileRecord)
	m.folders = make(map[string]FolderRecord)
	delete(m.meta, MetaKeySyncRoot)
	delete(m.meta, MetaKeyChangeToken)
	delete(m.meta, MetaKeyFullScan)
	m.mu.Unlock()
	return nil
}

// ClearAll wipes all 3 stores.
func (m *MemoryStore) ClearAll(_ context.Context) error {
	m.mu.Lock()
	clear(m.files)
	clear(m.folders)
	maps.DeleteFunc(m.meta, func(_ string, _ json.RawMessage) bool { return true })
	m.mu.Unlock()
	return nil
}

// Close is a no-op for MemoryStore.
func (m *MemoryStore) Close() error {
	return nil
}

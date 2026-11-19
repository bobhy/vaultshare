package syncstate

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

// Conflict kind identifiers persisted on ConflictState.Kind.
const (
	ConflictMarkers  = "markers"
	ConflictVersions = "versions"
	ConflictMissing  = "missing"
)

// Singleton keys stored in the `meta` object store.
const (
	MetaKeySyncRoot    = "syncRoot"
	MetaKeyChangeToken = "changeToken"
	MetaKeyEngineState = "engineState"
	MetaKeyClientID    = "clientId"
	MetaKeyFullScan    = "fullScan"
)

// FileRecord represents one logical Obsidian file, its last-synced baseline,
// observed local and Drive versions, and any active manual conflict flag.
type FileRecord struct {
	Path          string              `json:"path"`
	MimeType      string              `json:"mimeType"`
	LastSynced    *SyncedBaseline     `json:"lastSynced,omitempty"`
	Local         *LocalFileState     `json:"local,omitempty"`
	DriveVersions []DriveVersionState `json:"driveVersions,omitempty"`
	DriveFileIDs  []string            `json:"driveFileIds,omitempty"`
	Conflict      *ConflictState      `json:"conflict,omitempty"`
	UpdatedAtMs   int64               `json:"updatedAtMs"`
}

// SyncedBaseline captures the last cleanly synchronized state between local and Drive.
type SyncedBaseline struct {
	DriveFileID          string `json:"driveFileId"`
	LastSyncedRevisionID string `json:"lastSyncedRevisionId"`
	MD5                  string `json:"md5"`
	SHA256               string `json:"sha256"`
	Size                 int64  `json:"size"`
	LocalMtimeMs         int64  `json:"localMtimeMs"`
	DriveModifiedTime    string `json:"driveModifiedTime"`
	SyncedAtMs           int64  `json:"syncedAtMs"`
}

// LocalFileState caches the last observed local vault stat and checksums.
type LocalFileState struct {
	Size        int64  `json:"size"`
	MtimeMs     int64  `json:"mtimeMs"`
	MD5         string `json:"md5"`
	SHA256      string `json:"sha256"`
	DeletedAtMs int64  `json:"deletedAtMs,omitempty"`
}

// DriveVersionState captures metadata for one Google Drive file version at Path.
// Multiple entries model duplicate-named Drive files sharing the same path.
type DriveVersionState struct {
	FileID         string `json:"fileId"`
	ParentID       string `json:"parentId"`
	Name           string `json:"name"`
	MimeType       string `json:"mimeType"`
	ModifiedTime   string `json:"modifiedTime"`
	Size           int64  `json:"size"`
	MD5            string `json:"md5"`
	SHA256         string `json:"sha256"`
	HeadRevisionID string `json:"headRevisionId"`
	Trashed        bool   `json:"trashed,omitempty"`
}

// ConflictState flags a file requiring manual user resolution in ReconcileView.
type ConflictState struct {
	Kind         string `json:"kind"` // ConflictMarkers | ConflictVersions | ConflictMissing
	DetectedAtMs int64  `json:"detectedAtMs"`
	Summary      string `json:"summary"`
}

// FolderRecord maps a Google Drive folder ID to its parent and vault-relative path.
type FolderRecord struct {
	DriveFolderID string `json:"driveFolderId"`
	ParentID      string `json:"parentId"`
	Name          string `json:"name"`
	Path          string `json:"path"` // "" for configured sync root; e.g. "Notes/Work"
	Trashed       bool   `json:"trashed,omitempty"`
	ModifiedTime  string `json:"modifiedTime"`
}

// SyncRootMeta records the resolved Google Drive VaultShare root folder IDs.
type SyncRootMeta struct {
	VaultShareFolderID string `json:"vaultShareFolderId"`
	RootFolderID       string `json:"rootFolderId"`
	RootFolderName     string `json:"rootFolderName"`
}

// ChangeTokenMeta records the persisted Drive changes.list cursor.
type ChangeTokenMeta struct {
	StartPageToken string `json:"startPageToken"`
	UpdatedAtMs    int64  `json:"updatedAtMs"`
}

// EngineStateMeta persists whether conflict resolution is paused by the user.
type EngineStateMeta struct {
	Paused bool `json:"paused"`
}

// ClientIDMeta persists the per-vault client identifier used in diagnostics.
type ClientIDMeta struct {
	ID string `json:"id"`
}

// FullScanMeta tracks initialization and timestamps of the Full Scan walker.
type FullScanMeta struct {
	Initialized       bool  `json:"initialized"`
	LastStartedAtMs   int64 `json:"lastStartedAtMs"`
	LastCompletedAtMs int64 `json:"lastCompletedAtMs"`
}

// Snapshot holds all persisted rows loaded at startup from the 3 object stores.
type Snapshot struct {
	Files   []FileRecord
	Folders []FolderRecord
	Meta    map[string]json.RawMessage
}

// Store abstracts atomic persistence across the `files`, `folders`, and `meta` stores.
type Store interface {
	LoadAll(ctx context.Context) (*Snapshot, error)
	PutFile(ctx context.Context, rec FileRecord) error
	DeleteFile(ctx context.Context, path string) error
	MigrateFilePath(ctx context.Context, oldPath, newPath string, updated FileRecord) error
	PutFolders(ctx context.Context, folders []FolderRecord) error
	DeleteFolder(ctx context.Context, driveFolderID string) error
	PutMeta(ctx context.Context, key string, value any) error
	ResetSyncScope(ctx context.Context) error
	ClearAll(ctx context.Context) error
	Close() error
}

// NormalizeFileRecord recomputes DriveFileIDs as the sorted unique union of
// LastSynced.DriveFileID and every DriveVersions[i].FileID.
func NormalizeFileRecord(rec *FileRecord) {
	if rec == nil {
		return
	}
	seen := make(map[string]bool, len(rec.DriveVersions)+1)
	if rec.LastSynced != nil && strings.TrimSpace(rec.LastSynced.DriveFileID) != "" {
		seen[rec.LastSynced.DriveFileID] = true
	}
	for _, dv := range rec.DriveVersions {
		if strings.TrimSpace(dv.FileID) != "" {
			seen[dv.FileID] = true
		}
	}
	if len(seen) == 0 {
		rec.DriveFileIDs = nil
		return
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rec.DriveFileIDs = ids
}

func cloneFileRecord(rec FileRecord) FileRecord {
	out := rec
	if rec.LastSynced != nil {
		b := *rec.LastSynced
		out.LastSynced = &b
	}
	if rec.Local != nil {
		l := *rec.Local
		out.Local = &l
	}
	if rec.Conflict != nil {
		c := *rec.Conflict
		out.Conflict = &c
	}
	if rec.DriveVersions != nil {
		out.DriveVersions = slices.Clone(rec.DriveVersions)
	}
	if rec.DriveFileIDs != nil {
		out.DriveFileIDs = slices.Clone(rec.DriveFileIDs)
	}
	return out
}

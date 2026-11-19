package syncengine

import (
	"context"

	"vaultshare/go/gdrive"
	"vaultshare/go/settings"
	"vaultshare/go/syncstate"
)

// LocalStat describes lightweight file or folder metadata from Obsidian's Vault.
type LocalStat struct {
	Path     string
	Size     int64
	MTimeMs  int64
	IsFolder bool
}

// VaultPort abstracts Obsidian Vault file I/O and editor locking for pure-Go testing.
type VaultPort interface {
	ConfigDir() string
	Stat(ctx context.Context, path string) (LocalStat, bool, error)
	ListAllStats(ctx context.Context) ([]LocalStat, error)
	ReadBytes(ctx context.Context, path string) ([]byte, LocalStat, error)
	WriteText(ctx context.Context, path string, text string) (LocalStat, error)
	WriteBytes(ctx context.Context, path string, data []byte) (LocalStat, error)
	Rename(ctx context.Context, oldPath, newPath string) error
	Trash(ctx context.Context, path string) error
	ExistsInTrash(ctx context.Context, path string, size int64) (bool, error)
	WithOpenFileLock(ctx context.Context, path string, fn func(context.Context) error) error
}

// DrivePort abstracts Google Drive v3 operations (satisfied directly by *gdrive.Client).
type DrivePort interface {
	EnsureSyncRoot(ctx context.Context, syncRootFolder string) (gdrive.SyncRootIDs, error)
	ListChildren(ctx context.Context, folderID string) (folders []gdrive.DriveFile, files []gdrive.DriveFile, err error)
	FindInParents(ctx context.Context, parentIDs []string, name string) ([]gdrive.DriveFile, error)
	GetFileMeta(ctx context.Context, fileID string) (gdrive.DriveFile, bool, error)
	DownloadFile(ctx context.Context, fileID string) ([]byte, error)
	DownloadRevision(ctx context.Context, fileID, revisionID string) ([]byte, bool, error)
	CreateFile(ctx context.Context, parentID, name, mimeType string, mtimeMs int64, data []byte) (gdrive.DriveFile, error)
	UpdateFile(ctx context.Context, fileID, mimeType string, mtimeMs int64, data []byte) (gdrive.DriveFile, error)
	MoveRenameFile(ctx context.Context, fileID, newParentID, oldParentID, newName string) (gdrive.DriveFile, error)
	TrashFile(ctx context.Context, fileID string) error
	EnsureFolderPath(ctx context.Context, rootID, relDir string) (folderID string, created []syncstate.FolderRecord, err error)
	GetStartPageToken(ctx context.Context) (string, error)
	ListChanges(ctx context.Context, pageToken string) (gdrive.ChangePage, error)
}

// AuthPort abstracts OAuth token management (satisfied directly by *gdrive.TokenManager).
type AuthPort interface {
	LoadFromSecrets(ctx context.Context) (bool, error)
	HasRefreshToken() bool
	SetRefreshToken(ctx context.Context, refreshToken string) error
	Logout(ctx context.Context) error
}

// EventSink delivers UI updates and persistent Notices from Go to TypeScript.
type EventSink interface {
	EmitGlobalStatus(ev settings.GlobalStatusEvent)
	EmitLog(entry settings.LogEntry)
	EmitNotice(message string)
}

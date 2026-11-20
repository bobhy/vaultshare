# Sync State DB Design (Agent Spec)

Compact architectural reference for the VaultShare V2 IndexedDB persistence layer and in-memory cache.
Human-readable design docs live in Go `doc.go`/package godoc under `go/syncstate/` and `go/cmd/plugin/`.

## 1. Package Ownership & Runtime

- **Pure-Go core (`go/syncstate/`)**: Domain structs, `Store` interface, write-through `Cache`, and `MemoryStore` for native `go test ./go/...`.
  Zero `syscall/js` imports.
- **IndexedDB adapter (`go/cmd/plugin/idb_store.go`, `//go:build js && wasm`)**: Implements `syncstate.Store` over `window.indexedDB` via
  `github.com/bobhy/go-indexeddb` (`replace github.com/hack-pad/go-indexeddb => github.com/bobhy/go-indexeddb` in `go.mod`).
- **Boundary serialization**: Marshal/unmarshal one JSON blob per record (`JSON.parse` / `JSON.stringify`), never unpack fields via `syscall/js`.
- **DB naming & isolation**: `vaultshare-<sanitizedVaultName>` (lowercase alphanumeric + `-` slug, `"default"` fallback; distinct from v1
  `vault-share-<name>`).
- **Schema versioning**: `DB_VERSION = 1`. Pre-1.0 upgrades cold-start (drop all object stores and recreate). This is lossless because no base
  content blobs are stored locally; Full Scan rebuilds state.

## 2. Write-Through Cache & Secondary Indexes

`Cache` (`go/syncstate/`) loads all 3 stores once at startup via `Store.LoadAll(ctx)` (`getAll()` per store).

- **Write-through invariant**: All Store+Cache writes serialize through a context-aware channel `writeGate` (never holding `sync.Mutex` across
  IDB `Await`), persisting to `Store` (IDB) **first** and mutating in-memory maps **only after** IDB succeeds so Store and Cache never invert.
- **Read performance**: All hot lookups (`changes.list` parent-chain scope checks, conflict count badges, 1 Hz Sync Status snapshots, rename
  correlation) read O(1) from memory with zero IDB transactions.
- **In-memory indexes** maintained by `Cache`:
  - `filesByPath map[string]*FileRecord`
  - `driveIDToPath map[string]string` (every `fileId` in `LastSynced.DriveFileID` and `DriveVersions[*].FileID` → vault `path`)
  - `foldersByID map[string]*FolderRecord`
  - `foldersByPath map[string][]*FolderRecord` (slice tolerates duplicate-named Drive folders at the same path)
  - `conflictPaths map[string]string` (`path` → `ConflictState.Kind` for O(1) counts and Reconcile iteration)

## 3. Object Stores (3 Stores)

No `bases` blob store (revisions live in Drive via `lastSyncedRevisionId`) and no `pending_tasks` store (work queue lives in memory).

### Store 1: `files`
- **Primary key**: `path` (normalized vault-relative POSIX path, e.g. `"Notes/Ideas.md"`, no leading `/`).
- **Index**: `byDriveFileId` on `driveFileIds` (`unique: false`, `multiEntry: true`).

```go
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

type SyncedBaseline struct {
	DriveFileID          string `json:"driveFileId"`
	LastSyncedRevisionID string `json:"lastSyncedRevisionId"`
	MD5                  string `json:"md5"`
	SHA256               string `json:"sha256"`
	Size                 int64  `json:"size"`
	LocalMtimeMs         int64  `json:"localMtimeMs"`
	DriveModifiedTime    string `json:"driveModifiedTime"` // RFC3339
	SyncedAtMs           int64  `json:"syncedAtMs"`
}

type LocalFileState struct {
	Size        int64  `json:"size"`
	MtimeMs     int64  `json:"mtimeMs"`
	MD5         string `json:"md5"`
	SHA256      string `json:"sha256"`
	DeletedAtMs int64  `json:"deletedAtMs,omitempty"` // short retention for delete+create rename matching
}

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

type ConflictState struct {
	Kind         string `json:"kind"` // "markers" | "versions" | "missing"
	DetectedAtMs int64  `json:"detectedAtMs"`
	Summary      string `json:"summary"`
}
```

- **Invariants**:
  - `DriveFileIDs` must be recomputed before every write as the sorted unique union of `LastSynced.DriveFileID` (if non-empty) and every
    `DriveVersions[i].FileID`.
  - `DriveVersions` slice natively captures 0, 1, or N duplicate-named Drive files at `path` for N-way merge and Reconcile UI listing.

### Store 2: `folders`
- **Primary key**: `driveFolderId`.
- **Index**: `byPath` on `path` (`unique: false`).

```go
type FolderRecord struct {
	DriveFolderID string `json:"driveFolderId"`
	ParentID      string `json:"parentId"`
	Name          string `json:"name"`
	Path          string `json:"path"` // "" for sync root; e.g. "Notes/Work"
	Trashed       bool   `json:"trashed,omitempty"`
	ModifiedTime  string `json:"modifiedTime"`
}
```

- **Invariants**:
  - The configured sync root folder (`My Drive / VaultShare / <syncRootFolder>`) has `Path: ""`.
  - Subfolders use vault-relative POSIX paths (`"Folder/Sub"`).
  - `changes.list` resolves scope and path by walking `ParentID` in `foldersByID` (O(depth) in memory, zero API calls).

### Store 3: `meta`
- **Primary key**: `key` (stored as `{ "key": "<name>", "value": <payload> }`).

| Key | Go Struct | Fields & Semantics |
| :--- | :--- | :--- |
| `"syncRoot"` | `SyncRootMeta` | `VaultShareFolderID`, `RootFolderID`, `RootFolderName` (detects sync root setting changes) |
| `"changeToken"` | `ChangeTokenMeta` | `StartPageToken`, `UpdatedAtMs` |
| `"engineState"` | `EngineStateMeta` | `Paused bool` — persisted across restarts; if `true` on load, show persistent Notice (`duration = 0`) |
| `"clientId"` | `ClientIDMeta` | `ID string` — per-vault UUID for diagnostics/logging |
| `"fullScan"` | `FullScanMeta` | `Initialized bool`, `LastStartedAtMs`, `LastCompletedAtMs` |

- **`syncRootFolder` change handling**: When `settings.syncRootFolder` differs from `meta["syncRoot"].RootFolderName`, confirm with the user,
  clear `files`, `folders`, and `changeToken` (preserving `clientId` and `engineState`), resolve/create the new `RootFolderID`, capture a fresh
  `GetStartPageToken()`, and run `Full Scan`.

## 4. Crash-Safety & Recovery Invariants (No `pending_tasks` Store)

1. **Drive `changes.list` batching**: Capture `GetStartPageToken()` **before** startup Full Scan starts. Track each poll batch via a
   `pendingBatchPaths` set; persist `meta["changeToken"]` once non-errored batch paths finish (errored/offline paths move to retry rather than
   starving future polls). If killed mid-batch, next startup replays that delta idempotently.
2. **Local edits on startup**: Immediately after `LoadAll(ctx)`, diff Obsidian's in-memory `vault.getFiles()` (`stat.mtime` + `stat.size`, zero file
   content reads) against `filesByPath` and enqueue changed/created/deleted local files at High priority (also catches edits made while closed).
3. **Startup Full Scan**: Runs with randomized starting position / traversal order to audit Drive duplicates and recover any unseen remote shifts.

## 5. `syncstate.Store` Interface & Transactions

```go
type Snapshot struct {
	Files   []FileRecord
	Folders []FolderRecord
	Meta    map[string]json.RawMessage
}

type Store interface {
	LoadAll(ctx context.Context) (*Snapshot, error)
	PutFile(ctx context.Context, rec FileRecord) error
	DeleteFile(ctx context.Context, path string) error
	MigrateFilePath(ctx context.Context, oldPath, newPath string, updated FileRecord) error
	PutFolders(ctx context.Context, folders []FolderRecord) error
	DeleteFolder(ctx context.Context, driveFolderID string) error
	PutMeta(ctx context.Context, key string, value any) error
	ClearAll(ctx context.Context) error
	Close() error
}
```

- `MigrateFilePath` runs `Delete(oldPath)` + `Put(updated)` inside **one `readwrite` transaction** on `files` so renames atomically preserve
  `LastSynced.LastSyncedRevisionID` and `driveIDToPath`.
- `PutFolders` writes all supplied `FolderRecord`s in **one `readwrite` transaction** on `folders` (used for folder discovery and subtree path
  rewrites after folder renames/moves).
- **IDB safety invariants**:
  - `openDB` must single-flight `factory.Open` under a mutex/promise so concurrent startup callers never open duplicate connections (`onblocked`).
  - Inside any open IDB transaction, queue all `Put`/`Delete` requests synchronously before calling `tx.Await(ctx)`—never call `awaitPromise` or
    yield on Go channels mid-transaction (which causes IDB auto-commit and `TransactionInactiveError`).

## 6. Testing Contract (`go test ./go/syncstate/...`)

Run a shared behavioral test suite against `MemoryStore` + `Cache` covering:
- Recomputing `DriveFileIDs` and maintaining `driveIDToPath` across `PutFile`, `DeleteFile`, and `MigrateFilePath` (with duplicate Drive IDs).
- Duplicate-named Drive folders stored at the same `Path` in `foldersByPath` without overwriting each other.
- Subtree path updates via `PutFolders` + `MigrateFilePath` on folder rename/move.
- `conflictPaths` secondary index consistency across setting/clearing `FileRecord.Conflict`.
- Meta persistence round-trip for all 5 keys (`syncRoot`, `changeToken`, `engineState`, `clientId`, `fullScan`).

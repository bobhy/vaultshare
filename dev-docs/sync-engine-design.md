# Go Sync Engine Design (Agent Spec)

Compact architectural reference for the VaultShare V2 sync engine (`go/syncengine/`) and N-way text merge (`go/merge/`).
Human-readable design docs live in package/symbol godoc under `go/syncengine/` and `go/merge/`.

## 1. Pure-Go Packages & Port Interfaces (Zero `syscall/js`)

All orchestration, scheduling, conflict resolution, and metrics live in pure Go so `go test ./go/...` runs natively without Electron/Wasm.

### Port Interfaces Injected into `syncengine.Engine`

```go
type VaultPort interface {
	Stat(ctx context.Context, path string) (LocalStat, bool, error)
	ListAllStats(ctx context.Context) ([]LocalStat, error) // reads Obsidian in-memory vault.getFiles()
	ReadBytes(ctx context.Context, path string) ([]byte, LocalStat, error)
	WriteBytes(ctx context.Context, path string, data []byte) (LocalStat, error)
	Rename(ctx context.Context, oldPath, newPath string) error
	Trash(ctx context.Context, path string) error
	ExistsInTrash(ctx context.Context, path string) (bool, error)
	// Shows "Synchronizing..." overlay + read-only editor guard only if path is open in a view.
	WithOpenFileLock(ctx context.Context, path string, fn func(context.Context) error) error
}

type DrivePort interface {
	EnsureSyncRoot(ctx context.Context, syncRootFolder string) (SyncRootIDs, error)
	ListChildren(ctx context.Context, folderID string) ([]DriveEntry, error)
	FindByPath(ctx context.Context, rootID, relPath string) ([]DriveFile, error) // [] supports duplicates
	GetFileMeta(ctx context.Context, fileID string) (DriveFile, bool, error)
	DownloadFile(ctx context.Context, fileID string) ([]byte, DriveFile, error)
	DownloadRevision(ctx context.Context, fileID, revisionID string) ([]byte, bool, error)
	CreateFile(ctx context.Context, parentID, name, mimeType string, mtimeMs int64, data []byte) (DriveFile, error)
	UpdateFile(ctx context.Context, fileID string, mtimeMs int64, data []byte) (DriveFile, error)
	MoveRenameFile(ctx context.Context, fileID, newParentID, newName string) (DriveFile, error)
	TrashFile(ctx context.Context, fileID string) error
	EnsureFolderPath(ctx context.Context, rootID, relDir string) (folderID string, created []FolderRecord, err error)
	GetStartPageToken(ctx context.Context) (string, error)
	ListChanges(ctx context.Context, pageToken string) (ChangePage, error)
}

type EventSink interface {
	EmitGlobalStatus(ev settings.GlobalStatusEvent) // {Paused, NeedsLogin, ConflictsTotal}
	EmitLog(entry settings.LogEntry)
	EmitNotice(message string) // TS renders persistent Notice (duration = 0)
}
```

- **Upload timestamps**: `CreateFile` and `UpdateFile` pass the local file's `mtimeMs` as Drive `modifiedTime` so `Keep newest` / `Keep oldest`
  compare real edit timestamps instead of upload times.

### `go/merge/` Extensions (Ported from `../test-gdoc/go/merge`)
- Add `MarkerStyle`: `MarkerStyleBacktick` for `.md` (`\`<<<<< conflict\``, `\`===== base\``, `\`>>>>> conflict\``) and `MarkerStylePlain`
  (`<<<<< conflict`, `===== base`, `>>>>> conflict`) for other text files.
- Keep whole-line diff3 conflict regions and add iterative N-way folding across `N >= 2` versions (local + 1..K Drive duplicates) against the
  fetched `LastSyncedRevisionID` base (or 2-way N-way fallback when pruned/unseeded).

## 2. Event Ingestion, Debouncing, Echo Suppression & Rename Handling

### Local Vault Events (`src/main.ts` → Go)
- **`OnLocalOpen(path)`** (`workspace.on('file-open')`): **never debounced**; enqueues immediately at `PriorityHigh`.
- **`OnLocalRename(oldPath, newPath)`** (`vault.on('rename')`): authoritative in-Obsidian rename (for `TFolder`, walks all tracked descendant paths
  in `Cache`). Suppresses follow-up echo, mirrors via `DrivePort.MoveRenameFile` + `Cache.MigrateFilePath` (preserving `LastSyncedRevisionID`),
  or falls back to conflict check if missing on Drive.
- **`OnLocalModify(path)` / `OnLocalCreate(path)`**:
  1. Check `recentLocalWrites` / `LastSynced` checksum+mtime to ignore self-echoes.
  2. For `Create`, check `pendingDeletes` first (size + hash match → symmetric rename correlation).
  3. Otherwise start/reset per-path trailing hold-down debounce timer with max-wait cap (Advanced settings) → enqueue at `PriorityHigh`.
- **`OnLocalDelete(path)`**:
  1. If `TFolder`, expands to all tracked descendant paths in `Cache`.
  2. Held in `pendingDeletes` (1–2 s) alongside cached `{size, md5, sha256}` from `Cache`.
  3. Symmetrically checks `pendingCreates` (handling external renames where `create` arrives before `delete`).
  4. On match: converts to a single rename operation; on expiry without match: enqueues `PriorityHigh` delete/sync check.
- **Scope & exclusions**: Exclude `app.vault.configDir` (`.obsidian/`) and `.trash/` across local events, `changes.list`, and `Full Scan` so
  Obsidian config/trash directories are never synced even if touched on Drive by v1.
- **Startup local stat diff**: Immediately after `Cache.LoadAll(ctx)`, compare `VaultPort.ListAllStats` against `Cache.Files` in memory:
  - Correlate vanished vs. new local files by size + SHA-256/MD5 hash (offline renames while Obsidian was closed).
  - Enqueue all modified/created/deleted/renamed local files at `PriorityHigh`.
- **TODO (slow non-text file creation)**: Open Question #1 (`functional spec.md`) remains deferred — add a size/mtime stability probe before
  uploading newly created non-text files.

### Google Drive Poller (`changes.list`)
- Capture `GetStartPageToken()` **before** startup `Full Scan` begins. Poll on active/idle cadence (Advanced settings); skip while `Paused`.
- Processes `ChangePage` entries:
  1. Update `folders` cache first, handling all folder transitions: rename in place, move within sync root, move into sync root (walk subtree),
     move out of sync root, and `trashed`/`removed` (cascade path migrations or local trash/conflict checks across all descendant `folders`
     and `files`, updating `driveIDToPath`).
  2. Filter scope in O(depth) memory via `foldersByID` parent chain (ignore items outside `syncRoot.RootFolderID` or in excluded paths).
  3. Resolve path (via `driveIDToPath[fileId]` for existing/renamed/trashed files, or `folder.Path + "/" + name` for new files).
  4. Filter echoes via `recentDriveWrites` and `LastSynced` revision/checksum match; enqueue affected paths at `PriorityHigh`.
- **Crash-safe token commit**: Track batch completion via a `pendingBatchPaths` set; persist `meta["changeToken"]` once non-errored batch paths
  finish (errored/offline paths move to retry so they cannot starve Drive polling).

### Dual Echo Suppression
- `recentLocalWrites[path]` and `recentDriveWrites[fileID]` store `{expiresAtMs, md5, mtimeMs}` (~15 s TTL) to drop immediate watcher/poll echoes.
- Secondary check against `FileRecord.LastSynced` handles delayed polls or expired TTLs deterministically.

## 3. Two-Lane Priority Queue, Worker Pool & Lifecycle

- **Strict priority lanes**: `PriorityHigh` (live Vault/Drive events, `file-open`, startup local stat diff) drains completely before any
  `PriorityBulk` (Full Scan) item is popped.
- **Queue deduplication & per-path execution lock**:
  - `queued map[string]*Task` indexes items currently waiting in either lane (`High` or `Bulk`).
  - `inFlight map[string]bool` tracks paths currently executing in a worker; tasks are removed from `queued` upon worker pickup.
  - Enqueue deduplicates only against `queued[path]` (upgrading `Bulk → High` if needed). If an event arrives while `inFlight[path]` is true,
    it naturally enqueues once in `queued` without a separate `rerun` flag.
  - Dispatcher skips queued items whose `path` is currently `inFlight` until that worker finishes.
- **Concurrency & Parallel I/O**:
  - Worker pool of `N` goroutines (default 4; outbound Drive HTTP concurrency bounded by `MaxPendingRequests`).
  - Inside each task, Vault `Stat`/`Read` and Drive metadata/download requests run concurrently in child goroutines via `syscall/js` `Await`.
  - **Deadlock invariant**: Never hold any Go `sync.Mutex` (`queueMu`, `cacheMu`, etc.) across `awaitPromise`, `VaultPort`, `DrivePort`,
    `syncstate.Store`, or `EventSink` calls, so JS→Go re-entry cannot deadlock waiting goroutines.
- **Pause / Resume**:
  - `Pause()` sets and persists `meta["engineState"].Paused = true`; active workers finish cleanly, no new tasks dequeue, and **Full Scan pauses**.
  - On startup, if `engineState.Paused == true`, keep workers paused and call `EventSink.EmitNotice(...)` (persistent Notice, `duration = 0`).
  - `Resume()` sets `Paused = false`, persists `meta["engineState"]`, wakes workers, `changes.list` poller, and paused Full Scan.
- **`FlushHighPriority(ctx)`**: Cancels/fires all active local debounce timers immediately and blocks until `PriorityHigh` queue and in-flight
  High tasks drain to 0 (called best-effort on `visibilitychange` and ready for a future explicit flush button).

## 4. Self-Contained Task Execution & Resolution Pipeline

Every task ignores stale trigger payloads and evaluates fresh state in two phases:

### Phase 1: Parallel Metadata Gather (No Open-File Lock)
1. Concurrently run `VaultPort.Stat(path)` and `DrivePort.FindByPath(rootID, path)` (plus `Cache.GetFile(path)`).
2. **Fast-path skip**: If local `(size, mtimeMs)` matches `LastSynced` and Drive `(size, md5, modifiedTime, count==1)` matches `LastSynced`, exit.
3. Lazily compute local SHA-256/MD5 only when local `(size, mtimeMs)` changed or `LastSynced == nil` with matching size (rebaseline check).

### Phase 2: Classify & Execute
1. **Identical versions** (equal checksum, or equal `size + mtime + mime` when checksums absent) → update `FileRecord.LastSynced` and return.
2. **Single-sided change**:
   - Local modified/created, Drive unchanged/absent → upload (`UpdateFile` / `CreateFile` with local `mtimeMs`).
   - Drive modified/created, local unchanged/absent → `DownloadFile` **before** `WithOpenFileLock` → write local inside lock.
   - One side deleted, other unchanged → check trash confirmation (`DriveFile.Trashed` or `VaultPort.ExistsInTrash` in `.trash`); if confirmed,
     mirror trash (`TrashFile` / `VaultPort.Trash`) and clear `FileRecord`. If unconfirmed, flag `Conflict{Kind: "missing"}`.
3. **Modify-vs-delete conflict** (one side modified, other side trashed/missing) → flag directly as `Conflict{Kind: "missing"}` for manual
   resolution (no v1 placeholder side-files).
4. **Content / duplicate conflict (`N >= 2` distinct versions)** → walk `PluginSettings.SyncResolution` in configured order:
   - **`Merge (text only)`**: Skip if `DisableTextMerge` or non-text extension. Pre-fetch all Drive version contents + `DownloadRevision(fileID,
     LastSyncedRevisionID)` **before** locking. Inside `WithOpenFileLock`, re-read local, run N-way merge (clean text uses 3-way `diff3`; any
     marker-bearing text uses `nway` `ReconcileText` union, never raw `diff3`), write local. Release lock, re-check `GetFileMeta(fileID).
     headRevisionId` immediately before `UpdateFile` (re-run merge if Drive changed in the meantime), upload merged result to primary Drive file,
     trash extra Drive duplicates, and always advance `FileRecord.LastSynced` to the uploaded `headRevisionId` + checksum. If conflict markers
     remain, also set `Conflict{Kind: "markers"}`; if merge cannot apply cleanly (binary/malformed), fall through to next rule.
   - **`Keep newest` / `Keep oldest`**: Compare `mtimeMs` / `modifiedTime` across all N versions. If a Drive version wins, download **before**
     `WithOpenFileLock`, write local, and trash remaining Drive duplicates; if local wins, upload local and trash extra Drive duplicates.
   - **`Keep local`**: Upload local to primary Drive file (or create) and trash extra Drive duplicates.
   - **`Keep Drive`**: If `>1` distinct Drive versions exist, **fail/skip** and fall through to the next rule. Otherwise download **before**
     `WithOpenFileLock`, write local, update `LastSynced`.
   - **`Manual resolution`** (always last): Persist `Local` + `DriveVersions` on `FileRecord` and set `Conflict{Kind: "versions"}`.

## 5. Full Scan Walker

- **Triggers**: Startup (after fast local stat diff), periodic audit timer (Advanced settings), mobile wake, and manual `Full Scan` command.
- **Pause coupling**: Checks `engine.IsPaused()` before each directory/chunk step and parks on a resume signal while paused (zero Drive API calls
  while paused).
- **Mobile-safe randomized restart**: On wake/start, always starts from the beginning (never skips folders based on stale checkpoints) and picks a
  randomized starting index / shuffled directory traversal order per run so partial sessions balance coverage across the vault.
- **Audit & duplicate detection**:
  - Walks local tree + Drive tree (`ListChildren`), updating `folders` cache and discovering duplicate-named Drive files (`len(DriveVersions) > 1`).
  - Logs `INFO` for unknown or newer files relative to `Cache`, and `WARN` for unexpected discrepancies (e.g., disk file older than DB record).
  - Enqueues paths needing sync/resolution at `PriorityBulk`.

## 6. Metrics, UI Status Delivery & Testing Matrix

### `SyncStatusSnapshot` & Instrumentation
- `Paused bool`, `FullScan`: `{Active bool, Scanned int, Total int, FilesPerSec float64}`.
- `Queued`: `{High int, Bulk int}`, `InFlight int`.
- `HighPriLatencyMs`: `{EWMA float64, Last float64, Max float64}` (`EWMA` with `alpha = 0.2`, measured enqueue → task completion).
- `SessionResolutions`: counters reset at plugin load (`Merged`, `KeptNewer`, `KeptOlder`, `KeptLocal`, `KeptDrive`, `Pushed`, `Pulled`, `Trashed`,
  `Renamed`, `ManualFlagged`).
- `Conflicts`: `{Markers int, Versions int, Missing int, Total int}` (from `Cache.conflictPaths`).
- **Global indicator push (`EventSink.EmitGlobalStatus`)**: Emits `GlobalStatusEvent{Paused, NeedsLogin, ConflictsTotal}` immediately on any
  `Paused`, `NeedsLogin`, or `Conflicts.Total` transition **plus a 1-minute heartbeat** so the global indicator cannot go stale.
- **Sync Status pull (`Engine.GetStatusSnapshot`)**: No Go-side 1 Hz ticker. While the Sync Status window is open, its autorefresh countdown
  timer (≥ 1 s) and Refresh button pull `SyncStatusSnapshot` O(1) from Go memory; zero snapshot traffic occurs when closed.
- **Logging stream**: Go retains an in-memory ring buffer for hydration on open and pushes `EmitLog` only while a Logging view is subscribed.

### Native Go Test Suite (`go test ./go/syncengine/...`)
- Strict lane ordering (`High` preempts `Bulk` queue during active Full Scan; `file-open` skips debounce).
- In-flight re-enqueue + per-path mutex (second event during execution queues once without `rerun` flag; concurrent workers never collide on the
  same path).
- Symmetric external rename correlation (`Delete→Create` and `Create→Delete`), in-app `OnLocalRename`, and startup offline rename diff.
- Open-file lock ordering (verify `DownloadFile` / `DownloadRevision` complete *before* `WithOpenFileLock` enters).
- N-version resolution: duplicate Drive files folded by `Merge`, skipped by `Keep Drive` to fall through to `Manual resolution`.
- Modify-vs-delete and missing-without-trash → `Conflict{Kind: "missing"}`.
- Full Scan pausing cleanly when conflict resolution pauses, and randomized traversal visiting 100% of vault/Drive entries.
- `changeToken` persisted only after all tasks in a `changes.list` batch finish (remains unadvanced if paused/canceled mid-batch).

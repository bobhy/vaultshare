# Wasm Bridge & UI Views Design (Agent Spec)

Compact reference for the TypeScript shell (`src/`), Wasm RPC/host bridge (`src/wasm/`, `go/cmd/plugin/`), codegen (`go/cmd/gents/`), and UI.
Human-readable docs live in module TSDoc (`src/**/*.ts`) and package godoc (`go/cmd/plugin/`, `go/settings/`).
Note: `manifest.json` has `minAppVersion: "1.13.0"` and `isDesktopOnly: false` (Pattern 1 `syscall/js` `Await` works on Desktop, Android, iOS).

## 1. Startup, Lifecycle & Mutual Exclusion (`src/main.ts`)

1. `onload()`:
   - Run `isLegacyVaultShareEnabled(app)` (see `src/auth/oauth.ts`). If legacy `vault-share` is enabled, display a persistent
     Notice (`new Notice(msg, 0)`) and abort initialization (remain dormant: no protocol handler, no Vault listeners, no Wasm load).
   - Load settings merged with `DEFAULT_SETTINGS`, register `vault-share-auth` protocol handler, CM6 extensions (`conflictHighlightExtension` and
     `lockCompartment.of(EditorView.editable.of(true))`), views (`SyncStatusView`, `LogView`, `ReconcileView`), commands, global indicator, and
     declarative `PluginSettingTab`.
   - Defer Wasm instantiation until `app.workspace.onLayoutReady(...)` so Obsidian startup is never blocked.
2. `onLayoutReady()`:
   - Initialize `GoWasmBridge`, inject `window.__vaultshareHost`, call `bridge.start(settings)` (loads DB, secret token, starts engine).
   - Register `vault` events (`create`, `modify`, `delete`, `rename`), `workspace.on('file-open')`, and `document` `visibilitychange` (calls
     `bridge.flushHighPriority()` when hidden).
3. `onunload()`:
   - Call `bridge.unload()` (`__vaultshare.shutdown()`), closing IDB, canceling Go goroutines, releasing `js.Func` callbacks, deleting globals.

## 2. Go ↔ TypeScript Bridge Contract

### TS → Go RPC (`window.__vaultshare.invoke(method, payloadJson): Promise<string>`)
Every call returns a JS `Promise` (Go spawns a goroutine via `newJSPromise` in `go/cmd/plugin/main.go`, never blocking the JS thread).
Runtime validators in `src/wasm/validators.ts` narrow parsed JSON without `any`.

| Method | Payload → Result | Notes |
| :--- | :--- | :--- |
| `start` | `PluginSettings` → `GlobalStatusEvent` | Loads IDB + refresh token; runs startup stat diff; starts engine/poller. |
| `updateSettings` | `PluginSettings` → `{}` | Reconfigures timers, `MaxPendingRequests`, `SyncResolution` live. |
| `setRefreshToken` | `{token}` → `GlobalStatusEvent` | Stores in `SecretStorage`, clears `NeedsLogin`, triggers immediate sync. |
| `logout` | `{}` → `GlobalStatusEvent` | Clears local SecretStorage secret (no Google `/revoke`), enters `NeedsLogin`. |
| `onVaultEvent` | `VaultEventPayload` → `{}` | `kind`: `"open"\|"create"\|"modify"\|"delete"\|"rename"`. |
| `flushHighPriority` | `{}` → `{drained: bool}` | Cancels local debounces and waits for High-pri queue + in-flight High. |
| `pause` / `resume` | `{}` → `GlobalStatusEvent` | Persists `engineState.Paused`; pauses/resumes workers and Full Scan. |
| `startFullScan` | `{}` → `{}` | Manually triggers Full Scan walker (`PriorityBulk`). |
| `getStatusSnapshot` | `{}` → `SyncStatusSnapshot` | O(1) read from Go memory; polled by `SyncStatusView` only while open. |
| `getLogs` / `setLogStream` | `{minSeverity}` / `{enabled}` | Hydrates ring buffer on `LogView` open; toggles live `logEntry` pushes. |
| `listConflicts` | `{}` → `ConflictItem[]` | Lists all flagged `markers`, `versions`, and `missing` files. |
| `getConflictContent` | `{path, source, fileId?}` → `{text?, binary?, mime, size}` | Lazy preview for non-modal `ReconcileView`. |
| `resolveVersionConflict` | `{path, winner, driveFileId?}` → `{}` | Keeps chosen local/Drive version (or restores/deletes `missing`). |
| `clearMarkerConflict` | `{path}` → `{}` | Verifies no conflict markers remain and clears `Conflict` flag. |

### Go → TS Single-Root Host (`window.__vaultshareHost`)
Injected once before `bridge.start(...)` (zero per-API JSON fat thunks):
- `app`: `this.app` (Go accesses `app.secretStorage`, `app.vault`, `.trash` listing via opaque `js.Value` + `awaitPromise`; `WriteText` uses
  `vault.modify`/`create` for text notes, `WriteBytes` uses `vault.modifyBinary`/`createBinary` with freshly allocated `ArrayBuffer`
  `byteOffset === 0` via `js.CopyBytesToJS`/`Go`, and `Rename` uses `vault.rename` to avoid cross-note wikilink rewrites).
- `obsidian`: `{ Notice, requestUrl }` (used by `roundtripper.go` and persistent Notices `new Notice(msg, 0)`).
- `withOpenFileLock(path: string, fn: () => Promise<void>): Promise<void>`: acquires open-file UI lock only if `path` is open in a view.
- `emitEvent(kind: string, jsonPayload: string): void`: routes `globalStatus`, `logEntry`, and `notice` to Obsidian `Events` / `Notice`.

### Codegen (`go/cmd/gents/main.go` → `src/settings.generated.ts`)
Extend `gents` (currently primitive-only) in Milestone 1 to emit recursive structs, slices, pointers (`omitempty` → `?:`), and maps for
`PluginSettings`, `SyncStatusSnapshot`, `GlobalStatusEvent`, `LogEntry`, `ConflictItem`, and `DEFAULT_SETTINGS`. Never hand-edit generated TS.

## 3. Open-File Locking Overlay (`src/ui/open-file-lock.ts`)

1. Go completes all Drive downloads/revision fetches **before** calling `host.withOpenFileLock(path, fn)`.
2. TS collects **all** open leaves (`app.workspace.getLeavesOfType('markdown')` plus matching `TextFileView`s for split panes) matching
   `file.path === path`:
   - If none open: invoke `await fn()` immediately with zero UI overhead.
   - If open: **first** dispatch `lockCompartment.reconfigure(EditorView.editable.of(false))` and attach `"Synchronizing..."` overlay
     (`pointer-events: all`, spinner + label) on every matching leaf's `containerEl`, **then** call `await view.save()` on each so Obsidian's
     2 s `requestSave` buffer flushes before Go reads (keystrokes during the lock are blocked, not buffered).
3. Suppress local `modify` echoes during the lock, run `await fn()` (`WriteText`/`WriteBytes`), then restore `EditorView.editable.of(true)` and
   remove overlays in `finally`.

## 4. Global Conflict Indicator (`src/ui/global-status-bar.ts`)

- **Placement**: Single `addStatusBarItem()` on desktop; mirrored via a ribbon icon when `Platform.isMobile` (mobile hides the status bar).
- **Two-part layout**:
  - **Engine icon (left)**: shows **paused**, **running**, or **needs login** (Full Scan omitted to keep it clean). Click opens/focuses
    `SyncStatusView` (or Settings login when `NeedsLogin`).
  - **Conflict badge (right, hidden when `conflictsTotal === 0`)**: high-contrast pill `⚠ <N>`. Click runs `Reconcile conflicts`.
- **Freshness**: Driven by `GlobalStatusEvent` (edge-triggered on `Paused`/`NeedsLogin`/`ConflictsTotal` + 1-minute Go heartbeat).

## 5. Workspace Views (`ItemView` in `src/ui/`)

### `SyncStatusView` (`src/ui/sync-status-view.ts`)
- **Controls**: **Pause / Resume** button (or **Log in** CTA on `NeedsLogin`), **Scan Now** button, **Flush & Sync Now** button (cancels local
  debounces, calls `flushHighPriority()`, displays `"Synced — safe to switch"` confirmation).
- **Autorefresh**: Local `registerInterval` (≥ 1 s, e.g., 3 s default) drives a countdown label on the **Refresh** button and calls
  `bridge.getStatusSnapshot()` while open; stops in `onClose()`.
- **Displays**: Full Scan status + progress bar + `files/s`; High & Bulk queue counts + In-flight count; High-pri latency (`EWMA`, `Last`,
  `Max` tooltip); session resolution counters; conflict counts by kind (`Markers`, `Versions`, `Missing`).

### `LogView` (`src/ui/log-view.ts`)
- Ported from `../vault-share` log UI + severity filter (`DEBUG` / `INFO` / `WARNING` / `ERROR`), **Clear**, and synced **Pause / Resume**.
- `onOpen()` fetches ring buffer via `getLogs` and enables `setLogStream(true)`; `onClose()` disables `setLogStream(false)`.

### `ReconcileView` & Editor Marker Extension (`src/ui/reconcile-view.ts`, `src/ui/conflict-marker-navigator.ts`)
- **Primary command (`Reconcile conflicts`)**: Opens/focuses a **non-modal** main-panel tab (`workspace.getLeaf('tab')`) stepping through
  flagged conflicts (`Conflict 1 of N`, `Previous`, `Next`, `Skip`).
- **Multi-version / missing conflicts**: Lists candidate versions (`Local` vs. `Drive`, `DriveFileID` for duplicates, `ModifiedTime`, `Size`,
  trashed/missing flag), non-modal **View** preview pane (lazy-fetched via `getConflictContent`), and **Keep this version** (no rename control).
- **Text marker conflicts**: For `.md` notes, opens in an editor tab and jumps to the first marker via `Next conflict marker`. For non-`.md`
  text files (which Obsidian does not open natively in `MarkdownView`), `ReconcileView` renders an inline segment chooser for plain markers.
- **CM6 extension** (ported from `../vault-share/src/ui/conflict-marker-navigator.ts`): Highlights conflict regions and floats a CM6 panel (top
  on desktop, bottom on mobile) with per-segment buttons (`base`, `A1`, `A2`, ..., `Next`, `Prev`). Commands: **Reconcile conflicts**,
  **Next conflict marker**, **Previous conflict marker**.

## 6. Declarative Settings Tab (`src/settings.ts`)

Path A (`minAppVersion: "1.13.0"`): implement `getSettingDefinitions()` only (no `display()`). Sentence case on all UI strings.
- **Account**: Auth status + **Log in** / **Log out** button (`render`); visible `drive.file` limitation note.
- **Sync**: `syncRootFolder` (default sanitized vault name → `My Drive/VaultShare/<syncRootFolder>`; changing resets state after confirmation),
  `disableTextMerge` toggle, and `syncResolution` (`Merge (text only)` toggle pinned first + up/down controls for up to 2 middle rules +
  pinned `Manual resolution` always last).
- **Trash warning**: Visible warning notice row when Obsidian's trash setting is not `.trash`. Because `Vault.getConfig` and `trashOption` are not
  public in `obsidian.d.ts`, defensively read `${app.vault.configDir}/app.json` via `vault.adapter.read` (treating missing `trashOption` as
  system trash).
- **Advanced (Timers & Concurrency)**: `localDebounceMs`, `localMaxWaitMs`, `drivePollActiveMs`, `drivePollIdleMs`, `fullScanIntervalMin`,
  `maxPendingRequests` (1–10 slider), `logMinSeverity`, `logHorizon`.
- Mutating settings calls `saveSettings()` + `bridge.updateSettings(settings)`.

## 7. Testing (`npm test` via Vitest + `npm run build && npm run lint`)

- **`src/wasm/validators.test.ts` & `bridge.test.ts`**: Mocked `__vaultshare` validating all RPC payloads, `NeedsLogin` / `GlobalStatusEvent`,
  and clean `unload()`.
- **`src/auth/oauth.test.ts`**: State CSRF verification, protocol callback, `isLegacyVaultShareEnabled`.
- **`src/ui/` tests**: `OpenFileLock` (non-open fast path, open-file CM6 `editable=false` + overlay cleanup on error), `GlobalStatusBar`,
  `ConflictMarkerNavigator` (backtick + plain markers), and `SyncStatusView` (countdown refresh stops on `onClose`).

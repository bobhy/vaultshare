# VaultShare V2 — Review Remediation Plan

Tracks the multi-batch remediation of all findings from the whole-codebase review and design/functional spec audit.

## Workflow & Verification Rules

- Work through one batch at a time.
- After implementing a batch, run the full verification suite:
  ```sh
  GOCACHE=/tmp/go-build go test -race -count=1 ./go/... && npm test && GOCACHE=/tmp/go-build npm run build && npm run lint
  ```
- Mark completed items `[x]` in this file as they are verified.
- Stop at the end of each batch for manual testing and git commit.

---

## Batch 1: Go Merge Engine (`go/merge/`)

- [x] **B1.1 (`C1`) Fix `Diff3Merge` overlap boundary (`diff3.go`):**
  - Fix `hunksOverlap` so an empty insertion hunk `[s, s)` at the exact start of a non-empty edit/delete hunk `[s, e)` (`s <= hStart && hStart < e`) is recognized as overlapping.
  - Ensure `basePos` is strictly monotonic, `aStart`/`bStart` never go out of bounds (preventing Wasm slice panics), and `Diff3Merge(a, o, b)` is symmetric with `Diff3Merge(b, o, a)`.
- [x] **B1.2 (`C2`, `H9`) Fix `localizeClean` 3+ version drops and general N-way folding (`nway.go`):**
  - Fix `localizeClean` pre-region loop (`blocks[bi].Buf1Start+blocks[bi].Buf1Len < r.start`) so insertions at `r.start` and empty-base regions (`r.start == r.end`, including 2-way `mintTwoWay` and 3-way pure insertions) are not skipped before computing `cStart`.
  - Support N-way (`N >= 3`) folding when version 3+ makes disjoint clean edits or opens additional conflict regions outside existing regions (simultaneous multi-version merge / parsed-version expansion fallback), both with a common base (`hasBase == true`) and in 2-way fallback (`hasBase == false`).
- [x] **B1.3 (`C3`) Replace `O(N · M)` 2D `lcsPairs` DP table with linear-space / anchored diff (`diff3.go`):**
  - Intern lines to integer IDs for fast comparisons.
  - Use Patience unique-line anchoring + linear-space Hirschberg LCS (`O(N + M)` memory) with a safe cell budget fallback so large files (5,000–10,000+ lines) never OOM Wasm or freeze the UI thread.
- [x] **B1.4 (`C4`) Fix plain-text `=====` false positives and line-anchor `HasConflictMarkersWith` (`nway.go`):**
  - Tighten `backtickSpec.likeRe` and `plainSpec.likeRe` to match actual VaultShare conflict markers (`<<<<< conflict`, `>>>>> conflict`, `===== (base|A\d+)`) so plain-text section dividers (`=========`) do not break merges.
  - Make `HasConflictMarkersWith` check whole lines rather than unanchored `strings.Contains`, preventing inline prose mentions of `` `<<<<< conflict` `` from flagging permanent phantom conflicts.
- [x] **B1.5 (`H9`, `M7`, `L21`) CRLF normalization, duplicate label validation, and byte-safe keys (`nway.go`):**
  - Normalize `\r\n` (CRLF) in `splitLines` / line comparisons so Windows-saved files and cross-platform LF/CRLF edits parse and merge cleanly.
  - Reject duplicate segment labels (e.g., two `A1` segments in one region) in `validRegion`.
  - Replace `json.Marshal` in `normKey` and `skeletonOf` with length-prefixed raw string encoding so non-UTF-8 byte sequences cannot collide via `\ufffd`.
- [x] **B1.6 (`M15`) Expand unit tests in `go/merge/nway_test.go`:**
  - Add regression tests for `Diff3Merge` insertion+delete/edit at same offset (both argument orders), 3+ version 2-way and pure-insertion merges, 3+ version disjoint clean edits and multi-region conflicts, plain-text `=====` dividers, inline prose `` `<<<<< conflict` `` mentions, CRLF inputs, duplicate labels, non-UTF-8 bytes, and large-file (10,000-line) diff memory/speed.

---

## Batch 2: Go Sync Engine & Google Drive Client (`go/syncengine/`, `go/gdrive/`)

- [ ] **B2.1 (`C5`) Fix `coordinatorLoop` poll timer reset and 1-minute status heartbeat (`engine.go`):**
  - Keep a persistent poll timer across `heartbeat.C` ticks so intervals > 60 s (such as `DrivePollIdleMs` when offline) are never reset by the 1-minute heartbeat.
  - Add a forced `emitGlobalStatus()` for the 1-minute heartbeat so the global status indicator refreshes as specified.
- [ ] **B2.2 (`C11`) Fix resumable upload protocol bugs (`go/gdrive/upload.go`):**
  - Query server offset via `queryResumableOffset` before retrying failed chunk PUTs (including after 5xx/429).
  - Handle `200 OK` / `201 Created` returned by `queryResumableOffset` when the upload already completed on the server.
  - On `308 Resume Incomplete` without a `Range` header, keep `offset` at 0 bytes persisted (with a progress guard against infinite loops) instead of skipping to `end`; bound-check `nextOffset <= total`; allow recovery when `nextOffset == 0`.
- [ ] **B2.3 (`C12`) Preserve & reconcile duplicate Drive folders in Full Scan (`fullscan.go`, `client.go`):**
  - Use consistent canonical folder ordering (oldest `ModifiedTime`, then smallest `ID`).
  - Persist all non-trashed duplicate `FolderRecord`s in `Cache` so `GetFoldersByPath` and `ResolveFolderPath` find files and subfolders inside every duplicate folder.
- [ ] **B2.4 (`H1`) Discover preexisting Drive subfolders before Full Scan & recover on `ErrAlreadyExists` (`resolver.go`):**
  - When `cache.GetFoldersByPath(dir)` is empty in `findDriveFilesAtPath`, resolve non-trashed folder segments on Drive without creating folders (caching discovered `FolderRecord`s) so opening/creating a note in a subfolder before Full Scan merges with any preexisting Drive file.
  - If `CreateFile` returns `gdrive.ErrAlreadyExists`, re-enqueue the path for reconciliation instead of failing the task.
- [ ] **B2.5 (`H2`) Preserve edits across Drive rename+edit and local edit+rename (`events.go`):**
  - After `mirrorDriveRenameLocally` succeeds, check if `ch.File` also differs from `LastSynced` (`headRevisionId` / `md5Checksum`) and add `newPath` to `affectedPaths`; if `mirrorDriveRenameLocally` fails, add both `prevRec.Path` and `newPath` to `affectedPaths`.
  - On local rename `oldPath -> newPath`, after `handleLocalFileRenameLocked` migrates the record, enqueue `newPath` (or check if local content diverged from `LastSynced`) so pending debounced edits made right before rename are uploaded.
- [ ] **B2.6 (`H3`, `H4`) Fix `ResolveVersionConflict` and `resolver.go` races (`engine.go`, `resolver.go`, `events.go`):**
  - In `ResolveVersionConflict(winner="local")`, verify the target Drive file is non-trashed (if trashed or missing, create a new Drive file via `pushNewLocalFile`), and read local content under `WithOpenFileLock`.
  - In `ResolveVersionConflict(winner="delete")`, propagate `TrashFile` / `vault.Trash` errors before deleting the DB record.
  - Have `pushExistingLocalFile` return `(pushed bool, err error)` so `resolveByTimestamp` and `resolveKeepLocal` do not trash extra Drive duplicates when a `push-retry` was scheduled.
  - In `tryMergeConflict`, capture `freshStat` from `ReadBytes` inside `WithOpenFileLock`, and check `!found || latestMeta.Trashed` before `UpdateFile`.
  - In `pullDriveFile` (`!opts.isNew`), treat unexpected `ReadBytes` failure/disappearance inside `WithOpenFileLock` as `errLocalChangedDuringPull`.
  - Suppress local delete echo when mirroring Drive trash locally (`resolver.go`), and guard `trashDriveFolderIfEmpty` against racing with concurrent uploads.
- [ ] **B2.7 (`H5`, `H6`) Queue startup offline renames on `actionLane` & resume interrupted Full Scan (`engine.go`, `events.go`, `fullscan.go`):**
  - In `StartupLocalDiff`, enqueue `TaskLocalRename` onto `queue.EnqueueAction` instead of making synchronous Drive HTTP calls before `ensureSyncRootID`.
  - Track when an active Full Scan is interrupted by `Pause()` and automatically trigger a fresh Full Scan on `Resume()`.
- [ ] **B2.8 (`H7`, `H8`, `M1`–`M6`) Fix `TokenManager` races, Drive client edge cases, settings bounds, and echo map pruning (`go/gdrive/`, `go/settings/`, `go/syncengine/`):**
  - In `TokenManager.AccessToken`, verify `tm.refreshToken == rt` before persisting rotated refresh tokens or stamping `authFailedAt`, and loop if `refreshToken` changed mid-refresh.
  - In `initResumableSession`, track `invalidatedOnce` for `401` retry; in `CreateFile`, check MD5 on `409` and check `GetFileMeta(generatedID)` on final-attempt error; in `createFolder`, pre-generate folder ID via `generateFileID`; validate `splitFolderPath` (no `.`/`..`, no whitespace stripping) and non-empty `fileID` / `syncRootFolder`; fix `MoveRenameFile` when `oldParentID == ""`; implement full jitter in `backoffDelay` and safe integer/clamped `parseRetryAfter`; map all mergeable text extensions in `ContentMimeType`.
  - Enforce minimum timer bounds in `settings.NormalizeSettings`, prune expired entries in `recentLocalWrites`/`recentDriveWrites`/`inSessionDeletes`, and add unit tests across `go/gdrive/` and `go/syncengine/`.

---

## Batch 3: Go Wasm Bridge, IndexedDB & Vault Port (`go/cmd/plugin/`, `src/wasm/`)

- [ ] **B3.1 (`C8`) Fix `awaitPromise` primitive rejection panic (`jsutil.go`):**
  - Check `args[0].Type() == js.TypeObject || args[0].Type() == js.TypeFunction` before calling `.Get("message")`, stringify primitive/object fallbacks via `js.Global().Get("String").Invoke(args[0])`, and guard `promise` thenability.
- [ ] **B3.2 (`C9`, `H14`, `M8`) Fix IndexedDB cancellation, `tx.Await`, `onclose` recovery, `tx.Abort`, and multi-vault DB naming (`idb_store.go`, `jsutil.go`, `secret_store.go`):**
  - Check `ctx.Err()` after `req.Await(ctx)` in `openDBOnce`; in `LoadAll`, await `tx.Await(ctx)` after queuing the three `GetAll()` calls and read `.Result()`.
  - Add `tx.Abort()` on mid-transaction errors in `MigrateFilePath`, `PutFolders`, `ResetSyncScope`, and `ClearAll`.
  - Handle unexpected IDB connection closure (`onclose` / `InvalidStateError`) by clearing `s.db` so `openDB` reconnects (critical on iOS mobile backgrounding); guard `window.indexedDB` before `idb.Global()`.
  - Append a short deterministic hash of the raw vault name to `slugifyVaultName` when non-ASCII or lossy characters are present so distinct vaults never collide on `"default"`.
  - Return an error in `jsSecretPort.SaveRefreshToken` if `secretStorage` is unavailable, and guard `setSecret` calls against JS throws.
- [ ] **B3.3 (`C10`) Fix `syncService.handleStart` premature publication, error rollback, and `shutdown()` race (`service.go`, `engine.go`):**
  - Single-flight `handleStart` and keep `s.engine` unpublished until `eng.Start(...)` succeeds; close `store` and clear state if `Start` fails or `shutdown` occurred during startup; check `ctx.Err()` in `Engine.Start` before launching epoch loops.
- [ ] **B3.4 (`H10`, `H11`, `H12`, `H15`, `M1`) Fix `jsVaultPort` trash API, flat `.trash` lookup, `WithOpenFileLock` callback lifetime, post-write stat, and HTTP transport (`vault_port.go`, `roundtripper.go`, `service.go`):**
  - Use `h.fileManager.Call("trashFile", af)` in `jsVaultPort.Trash` (aligned with Obsidian plugin guidelines).
  - In `ExistsInTrash`, check both Obsidian's flat `.trash/<basename>` (with integer collision suffix `<stem> <N><ext>` and `type == "file"`) and nested `.trash/<relPath>`.
  - In `WithOpenFileLock`, do not release `cb` on `ctx.Done()` before JS invokes/settles it, and wait for `fn` if already running.
  - Refresh file stat after `vault.modify`/`modifyBinary` via `adapter.stat(relPath)` if needed, exclude hidden dotfiles not indexed by Obsidian's `Vault` API, and wrap missing-file errors with `os.ErrNotExist`.
  - Configure a request timeout and `CheckRedirect: http.ErrUseLastResponse` on Drive `http.Client`, set `contentType` and empty-PUT `Content-Length: 0` in `roundtripper.go`, and check `ctx.Err()` before firing `requestUrl`.
- [ ] **B3.5 (`H13`, `M15`) Harden `GoWasmBridge` lifecycle and expand `src/wasm/bridge.test.ts` (`bridge.ts`, `validators.ts`, `bridge.test.ts`):**
  - Wrap `emitEvent` in `try/catch` so JS listener errors never panic Go Wasm.
  - Handle `unload()` during in-flight `init()`, reset `readyPromise` and notify on unexpected `go.run()` exit, normalize `candidates: null` in `validators.ts`, use generated request types in `bridge.ts`, and add unit tests for `GoWasmBridge` lifecycle and RPC methods.

---

## Batch 4: TypeScript UI, Settings, Lifecycle & OAuth (`src/`, `styles.css`, `../vault-share/`)

- [ ] **B4.1 (`C6`, `H18`, `H23`, `M12`) Fix Settings reactivity, `MAX_MIDDLE_RULES = 2`, and settings races (`src/settings.ts`, `src/main.ts`):**
  - Override `setControlValue(key, value)` in `VaultShareSettingTab` (or hook `saveSettings`) with input validation so all 9 declarative `control:` settings propagate live to the running Go engine via `wasmBridge.updateSettings()`.
  - Set `MAX_MIDDLE_RULES = 2` (matching Go's `MaxMiddleResolutions = 2`).
  - Clone `DEFAULT_SETTINGS` deeply in `loadSettings()` (guarding `Array.isArray(syncResolution)`), serialize `saveRules()` in `renderResolutionEditor`, reset `button.removeDestructive()` and disarm on edit in `Sync root folder`, and return a listener cleanup closure from Account `render()`.
- [ ] **B4.2 (`C7`, `H19`) Fix `ConflictMarkerNavigator` live-edit safety, `view.save()` before `clearMarkerConflict`, empty-segment newline, and navigation (`src/ui/conflict-marker-navigator.ts`, `src/main.ts`):**
  - Re-parse or map conflict block ranges on document changes (`tr.docChanged`) and resolve against fresh editor lines so editing while the banner is open never overwrites wrong lines.
  - Flush `await view.save()` before invoking `wasmBridge.clearMarkerConflict(filePath)`.
  - Remove the trailing line break when applying an empty segment (`lines.length === 0`) so resolving an empty `base` or deletion leaves no blank line.
  - Fix line-0 `navigateForward`, inside-block `navigateBackward`, mobile `touchend` scroll/double-fire (use standard `click`), and add an inline plain-marker segment chooser in `ReconcileView` for non-`.md` text files.
- [ ] **B4.3 (`H17`, `M2`, `M10`, `H21`, `H23`) Fix view restoration across restart, startup/unload races, editor save before flush, and mobile wake (`src/main.ts`, `src/ui/log-view.ts`, `src/ui/sync-status-view.ts`, `src/ui/reconcile-view.ts`):**
  - Trigger `globalStatus` listeners after setting `this.engineStarted = true` in `startEngineAndWatchers()` so a restored `LogView` connects immediately on startup.
  - Guard `SyncStatusView` and `ReconcileView` on `this.plugin.isEngineStarted()` before calling `bridge` RPCs, and re-check DOM refs after `await` in `catch` blocks.
  - Register vault watchers and check unload state cleanly across `startEngineAndWatchers()`; save dirty open `TextFileView` buffers before `flushHighPriority()` in `flushAndNotify()` and `visibilitychange` (`hidden`); trigger poll/scan on `visibilitychange` (`visible`).
- [ ] **B4.4 (`H20`, `H22`, `H24`, `M11`, `M12`) Harden `ReconcileView`, `OpenFileLocker`, `LogView`, `GlobalStatusIndicator`, and `styles.css`:**
  - Add two-step confirmation on **Delete everywhere**, clarify trashed Drive candidate actions, add `Skip` button, switch opened `.md` conflict tabs to source mode, and serialize `loadConflicts()` / `loadPreview()`.
  - In `OpenFileLocker`, suppress `view.save()` modify echoes, add `EditorState.readOnly.of(true)` + keyboard event blocking on the locked container, wrap `finally` per-handle cleanup in `try/catch`, and unlock all active handles on `onunload()`.
  - In `LogView`, use `this.plugin.settings.logHorizon` and `logMinSeverity`, append single live log rows incrementally (preserving active text selection), and ref-count active `LogView` leaves before calling `setLogStream(false)`.
  - Fix mobile ribbon priority when `needsLogin && conflictsTotal > 0`, use `plugin.registerDomEvent`, and add missing CSS rules in `styles.css`.
- [ ] **B4.5 (`H16`, `M9`) Upgrade OAuth flow and Cloudflare Worker relay (`src/auth/oauth.ts`, `~/src/obsidian/vault-share`):**
  - Update the Cloudflare Worker in `~/src/obsidian/vault-share` to support V2 ephemeral ECDH P-256 + AES-GCM encrypted refresh-token handoff when `state` includes `pub` while preserving the existing plaintext redirect for V1 clients (retaining the existing Google OAuth Client ID and `client_secret`).
  - Persist pending OAuth state/private key in `sessionStorage`/`localStorage` with a TTL so mobile WebView eviction during browser consent does not break login, and surface `params.error` / `error_description` clearly.

---

## Batch 5: Codegen, CI/Release Workflows, Config & Documentation (`go/cmd/gents/`, `.github/workflows/`, `dev-docs/`, `AGENTS.md`, `README.md`)

- [ ] **B5.1 (`C13`, `H25`) Fix GitHub Actions workflows and `versions.json` (`.github/workflows/lint.yml`, `.github/workflows/release.yml`, `versions.json`):**
  - Add `actions/setup-go` (with Go 1.27 matching `go.mod`) and run `npm run test:go` + `npm test` in CI.
  - Update `versions.json` to include `"2.0.0": "1.13.0"`.
- [ ] **B5.2 (`H25`) Harden `go/cmd/gents/main.go` and emit shared constants:**
  - Skip unexported fields, handle `reflect.Map` in `omitempty`, escape control characters in `tsLiteral`, emit shared settings constants (`MAX_MIDDLE_RESOLUTIONS`, bounds), and add `go/cmd/gents/main_test.go`.
- [ ] **B5.3 (`M14`, `L1`–`L6`) Repo config hygiene (`.gitignore`, `package.json`, `eslint.config.mts`, `version-bump.mjs`, `.npmrc`):**
  - Fix `.gitignore` LF line endings and `.vscode` trailing space; fix `"license": "0BSD"` and remove unused `devDependencies` in `package.json`; clean up obsolete sample-plugin rules in `eslint.config.mts`; ensure trailing newlines in `version-bump.mjs` and `.npmrc`.
- [ ] **B5.4 (`H25`, Spec Section 5) Update `AGENTS.md`, `README.md`, and `dev-docs/*.md`:**
  - Replace legacy sample-plugin references in `AGENTS.md` and `README.md` with VaultShare V2 architecture, commands, `npm test`, `.trash` recommendation, and `drive.file` scope documentation.
  - Synchronize `dev-docs/{functional spec.md,sync-engine-design.md,drive-api-design.md,db-design.md,bridge-ui-design.md}` with the final V2 interfaces and behavior.

# Independent Code Review: VaultShare V2 (2026-11-20)

Whole-project second-opinion review of `go/`, `src/`, build, CI, and docs, judged against
`dev-docs/functional spec.md` and the `*-design.md` specs. Written independently of
`review-remediation-plan.md`. Where a finding overlaps the few parts of that plan the reviewer had
already seen (its batch 5 items and B1.1), the overlap is noted.

Status: **recommendations only**. Nothing here has been fixed. Adopt items into a `*-plan.md` before
working them.

## Evidence legend

- **Reproduced**: demonstrated by a failing test run against the real engine with the existing fakes
  (tests kept outside the repo; see [Reproduction tests](#reproduction-tests)).
- **Verified**: observed directly in a live Obsidian 1.15 test vault.
- **By reading**: traced through the code; not executed.
- **Plausible**: depends on Google Drive behavior not verified here.

## Summary

| ID | Sev | Finding | Evidence |
| :-- | :-- | :-- | :-- |
| H1 | High | Drive polling stops forever after one offline blip (or any poll interval ≥ 60 s) | Reproduced |
| H2 | High | Drive `404` treated as confirmed trash, so the unchanged local file is trashed | Reproduced (account-switch variant: Plausible) |
| H3 | High | Reconcile "Keep local"/"Keep Drive" on a trashed Drive version trashes the local file | Reproduced |
| H4 | High | One persistently failing path pins the `changes.list` cursor forever | By reading (behavior locked in by an existing test) |
| H5 | High | No mass-deletion guard; trashing the Drive sync root empties every device's vault | By reading |
| H6 | High | IndexedDB and refresh-token keys derived from a lossy slug of the vault *name* | By reading |
| M1 | Med | `ExistsInTrash` assumes nested `.trash/`; Obsidian's trash is flat | Verified |
| M2 | Med | Duplicate Drive folders: inconsistent canonical choice, files in "other" duplicates never sync | By reading |
| M3 | Med | Force-overwrite resolutions decide on a pre-lock snapshot and can drop fresh keystrokes | By reading |
| M4 | Med | Rename handling: N-file folder moves, dropped edits after rename, stale content after remote rename+edit | By reading |
| M5 | Med | OAuth: refresh token in `obsidian://` URL; CSRF state lost if mobile OS kills Obsidian | By reading |
| M6 | Med | `main.js` is 18.3 MB; gzip-embedding the Wasm would make it about 4.7 MB | Measured |
| M7 | Med | TS/Go settings drift (middle-rule cap 3 vs 2); no upper bound on poll intervals | By reading (overlaps B5.2) |
| M8 | Med | README lacks the network/relay/account disclosures the community store requires | By reading (overlaps B5.4) |
| M9 | Med | CI runs no tests and doesn't pin Go | By reading (overlaps B5.1) |
| M10 | Med | Invalid/expired change cursor is never reset; polling errors forever | By reading |
| L1–L11 | Low | See [Low-severity findings](#low-severity-findings) | |

## High-severity findings

### H1. Drive polling stops forever after one offline blip

- **Where**: `go/syncengine/engine.go:388` (`coordinatorLoop`), heartbeat case at `:410`; offline is cleared
  only at `:432` (successful poll) and `:511` (`SetRefreshToken`).
- **Mechanism**: each loop iteration creates a fresh poll timer, and the 1-minute heartbeat `case`
  restarts the loop. Any interval ≥ 60 s is reset before it fires. When offline (or paused/needs-login)
  the idle interval (default 600 s) applies, so the engine never polls again, which means it never clears
  `offline`. Periodic Full Scan is driven from `runPollCycle`, so it stops too.
- **Impact**: after a transient network failure (laptop sleep, Wi-Fi drop), Drive→local sync silently
  stops until the user resumes, changes settings, or logs in again. A user-configured active interval
  ≥ 60 s never polls even online.
- **Evidence**: `testing/synctest` over 31 virtual minutes gives 0 polls offline, 0 polls with a 90 s active
  interval, and 51 polls with defaults online.
- **Fix**: keep one long-lived poll timer (reset it only after a poll or wake) and service the heartbeat
  without touching it, or compute the next poll deadline and `select` on `time.Until(deadline)`. Add the
  synctest scenarios as regression tests.

### H2. Drive `404` is treated as "confirmed trashed"

- **Where**: `go/syncengine/resolver.go:246` (`isDriveFileTrashed`: `!found || meta.Trashed` returns `true`),
  consumed by Case 2 at `resolver.go:131-152`. The same pattern appears in the parent walk.
- **Impact**: when Drive returns 404 (file purged, access lost, or a *different Google account*), an
  unchanged local file is moved to trash. The spec says an unlocatable version must become a
  manual-intervention conflict.
- **Account-switch amplifier (Plausible)**: `Logout` (`engine.go:540`) keeps all sync state, and
  `ensureSyncRootID` (`engine.go:966`) trusts the cached root ID when the name matches. Logging in with a
  different Google account makes every `LastSynced` ID 404. Full Scan then queues every local file as
  "local-only", and each is trashed.
- **Evidence**: reproduced with a single file (baseline sync, then delete the Drive node, then resolve; the
  local file is trashed).
- **Fix**: only `meta.Trashed == true` (or a trashed ancestor) confirms deletion; `!found` flags a
  `missing` conflict. Record the Drive account identity (`about.get` → `user.permissionId`) in `meta`
  on login and reset sync scope (with confirmation) when it changes. Verify the cached sync-root ID with
  `GetFileMeta` after login.

### H3. Reconcile choices on a trashed Drive version destroy the local file

- **Where**: `engine.go:758` (`resolveVersionConflictLocked`); `gdrive/upload.go:123` (`UpdateFile` never
  sets `trashed:false`); `src/ui/reconcile-view.ts` sends no `driveFileId` for "Keep local".
- **Scenario A, "Keep local"**: in a `missing` conflict (local edited, Drive trashed), the engine uploads
  into the *trashed* `LastSynced` file and records it as synced. The next resolve (open, edit, scan) finds
  no non-trashed Drive file and treats the trash as confirmed, so it **trashes the local file the user chose
  to keep**.
- **Scenario B, "Keep Drive"**: on a candidate shown as "• Trashed", the `drive` branch (`engine.go:810`)
  trashes the local file. The spec says this is where the user can *restore* a missing version.
- **Evidence**: both reproduced.
- **Fix**: "Keep local" with a trashed or missing target either untrashes (`trashed:false` in the update
  metadata) or creates a new file. "Keep Drive" on a trashed candidate untrashes it and pulls. Keep
  "Delete everywhere" as the only path that deletes. Add confirmation for destructive buttons.

### H4. One failing path pins the change cursor forever

- **Where**: `events.go:1064` (`completeBatchTask`): any task error aborts the batch, and the token is not
  advanced. `TestBatchGatedChangeTokenSkipsCommitOnTaskError` locks this behavior in.
- **Impact**: a *persistent* failure re-lists an ever-growing change backlog on every poll (each 30 s) and
  re-resolves every path in it. That costs API quota and battery, and the cursor never advances. Realistic
  persistent failures: a Drive file whose name is invalid on this OS (`:` `?` `*` on Windows/Android), a
  case-only name collision on a case-insensitive file system, `ErrAlreadyExists` from M2, quota errors.
  The design doc requires errored paths to "move to retry so they cannot starve Drive polling"; that
  isn't implemented.
- **Fix**: on task error, move the path to a retry set with backoff (persisted if needed) and still advance
  the cursor; surface repeatedly failing paths in the Sync Status view and logs.

### H5. No mass-deletion guard

- **Where**: deletion mirroring in `resolver.go` Cases 2/3 and `events.go:800-812`. The spec's Full Scan
  "reasonability checks" aren't implemented.
- **Impact**: trashing `My Drive/VaultShare/<root>` (or a large folder) in the Drive UI trashes every
  descendant locally on every device. Deleting a large local folder, by mistake or because of a moved or
  unmounted vault directory, trashes it on Drive for all devices. H2 is another route to the same outcome.
- **Fix**: count the trash operations a poll batch, scan, or startup diff would perform. Above a threshold
  (for example more than 20 files or 10% of tracked files), auto-pause and show a persistent Notice listing
  the scope, requiring an explicit "Apply deletions" confirmation.

### H6. Per-vault storage keyed by a lossy vault-name slug

- **Where**: `go/cmd/plugin/jsutil.go:138` (`slugifyVaultName`: keeps only `[a-z0-9]`, falls back to `default`),
  used by `idb_store.go:45` (DB name) and `secret_store.go:27` (refresh-token key).
- **Impact**: all vaults share one IndexedDB origin. Vaults with non-Latin names (Japanese, Cyrillic, Greek,
  and so on) all map to `vaultshare-default`; names differing only in case or punctuation collide, as do
  same-named vaults in different folders. Opening vault B loads A's state and sees a different `syncRoot`
  name, so `ResetSyncScope` runs; switching back resets again. The vaults also share a login.
- **Fix**: generate a random per-vault ID once and persist it with `app.saveLocalStorage` (public,
  vault-scoped, since 1.8.7); key both the DB and the secret by it, with a one-time migration from the
  slug-named DB when its `syncRoot.RootFolderName` matches this vault. (`app.appId` is also unique per
  vault but isn't in the public API.) Delete the duplicate TS helper `refreshTokenSecretKey`
  (`src/auth/oauth.ts:62`, used only by tests).

## Medium-severity findings

### M1. Trash confirmation can't find nested files (Verified)

`ExistsInTrash` (`go/cmd/plugin/vault_port.go:195`) looks for `.trash/<relPath>`. In Obsidian 1.15, trashing
`…/sub/probe-note.md` produced `.trash/probe-note.md`: the trash is flat. Only root-level files can be
confirmed. Together with the 5-minute in-session delete TTL (`events.go:342`), deletions made while
paused, needing login, offline for more than 5 minutes, or while Obsidian was closed become `missing`
conflicts instead of mirrored trashes. The test fake (`fakeVault.ExistsInTrash`) encodes the same wrong
assumption. **Fix**: match by basename (with Obsidian's ` N` collision suffix) plus size; keep in-session
deletes until processed rather than for a fixed 5 minutes; correct the fake.

### M2. Duplicate Drive folders are only half-handled

- Full Scan stores only the smallest-ID duplicate (`fullscan.go:163`), while `EnsureFolderPath` writes into
  the oldest (`pickCanonicalFolder`, `gdrive/client.go:285`). `findDriveFilesAtPath` searches only cached
  folders, so on a device that learned folders via Full Scan, files in the other duplicate never sync.
- `createFolder` (`gdrive/client.go:554`) is retried on network errors without a pre-generated ID, so a
  timed-out but successful request creates duplicates.
- `CreateFile` returns `ErrAlreadyExists` (`upload.go:73`), which the engine treats as a plain failure (feeds H4).

**Fix**: one canonical rule shared by scan and writes; cache every duplicate; consolidate (move children,
trash empties) during Full Scan as the design doc says; use `generateIds` for folders; treat
`ErrAlreadyExists` as "re-resolve now".

### M3. Force-overwrite resolutions can drop fresh keystrokes

`pullExistingDriveFile` (`resolver.go:350`), used by Keep newest/oldest, Keep Drive and manual "drive",
passes `forceOverwrite: true`. The choice is made from a local stat taken *before* `WithOpenFileLock`
flushes the editor, so up to ~2 s of typing can be overwritten. This bites when text merge is disabled or
the rule order puts a timestamp rule first. **Fix**: inside the lock, re-hash local content; if it changed
since classification, abort and re-enqueue (as the merge/pull paths already do).

### M4. Rename handling gaps

- Local folder rename moves each descendant file individually (`events.go:659`) and leaves the old Drive
  folder behind. Prefer one `MoveRenameFile` on the folder plus a `PutFolders` subtree path rewrite.
- `handleLocalFileRename` cancels the new path's debounce (`events.go:599-602`) and never re-enqueues it.
  An edit made between the rename event and the rename task's execution is not pushed until the next
  open or scan. **Fix**: enqueue the new path (cheap fast path) after a successful rename.
- After mirroring a Drive rename (`events.go:1040`) or folder move (`handleKnownDriveFolderChange`), the
  new paths aren't re-resolved, so a remote rename+edit leaves stale local content. **Fix**: enqueue them.

### M5. OAuth flow weaknesses

- The relay returns the long-lived refresh token in an `obsidian://vault-share-auth?…&refresh_token=…` URL.
  Custom schemes can be claimed by other apps on mobile and may be logged. Prefer the relay returning a
  short-lived one-time ticket that the plugin redeems over HTTPS (shared with V1, so coordinate).
- `pendingState` lives only in memory (`src/auth/oauth.ts:72`). If iOS/Android kills Obsidian while the
  user is in the browser, the callback fails ("No pending OAuth login flow"). Persist state with an expiry
  (`localStorage` is enough; it's not a secret).

### M6. Bundle size

`build/plugin.wasm` is 13.7 MB and `main.js` 18.3 MB (base64). Gzipped, the Wasm is 3.5 MB, so `main.js` would
be about 4.7 MB if the Wasm were embedded gzipped and inflated at load with `DecompressionStream('gzip')`
(Electron and current iOS/Android WebViews support it). Separately, `net/http` pulls in `crypto/tls`,
`crypto/x509`, and HTTP/2, none of which can run here because every request goes through `requestUrl`.
Making `gdrive` speak `HTTPRequest`/`DoFunc` directly would drop them. Both cut startup time and mobile
memory.

### M7. Settings drift between TS and Go (overlaps B5.2)

`src/settings.ts:41` caps middle rules at 3 (and the UI says "up to 3"); Go caps at 2
(`settings.go:38`) and silently drops the third. The poll interval inputs have no maximum (which interacts
with H1). Emit shared limits from Go via `gents` and remove the TS reimplementation of
`NormalizeSyncResolution`.

### M8. README disclosures (overlaps B5.4)

The user-facing README sections are placeholders. Community-store policy requires stating network use
and third-party services: Google Drive API, the Cloudflare relay (which sees refresh tokens on every
refresh), the need for a Google account, the `drive.file` visibility limitation, and the `.trash`
recommendation.

### M9. CI (overlaps B5.1)

`.github/workflows/lint.yml` and `release.yml` never run `npm run test:go` or `npm test` and don't install a
pinned Go. Release also doesn't check that the tag equals `manifest.json` `version`.

### M10. No recovery from an invalid change cursor

If `changes.list` rejects the stored token (account switch, expiry), `pollDriveChangesOnce`
(`events.go:788`) logs and fails every cycle. On a 400/404 for the token: fetch a new start token and run
a Full Scan.

## Low-severity findings

- **L1** Lost-wakeup race: `workerLoop` (`engine.go:342`) calls `PopNext` and `WakeChan` under separate
  locks. Return the wake channel from `PopNext`. Rarely hit under Wasm's cooperative scheduler.
- **L2** The heartbeat calls `emitGlobalStatusIfChanged` (`engine.go:1017`), so it never re-sends an unchanged
  status. The spec's "cannot go stale" heartbeat is a no-op.
- **L3** Startup Full Scan runs only when the interval has elapsed (`engine.go:209-213`); the spec says once
  per startup.
- **L4** IndexedDB schema bumps drop all stores. That isn't lossless: unsynced deletions resurrect, and
  diverged files lose their merge base, producing 2-way markers. Write real migrations before 1.0.
- **L5** Unvalidated casts of bridge events: `main.ts:92`, `log-view.ts:152,162`, `settings.ts:156`. The bridge
  already validates; give `on()` typed overloads instead of `unknown` plus a cast.
- **L6** Merging normalizes CRLF to LF for the whole file.
- **L7** Performance: Full Scan reads every clean text file to audit markers (`fullscan.go:297`);
  `Cache.PutFolders` rebuilds the whole path index per call (quadratic over a scan).
- **L8** `refreshTokenSecretKey` (TS) duplicates Go slug logic and is dead outside tests (see H6).
- **L9** Every Notice is persistent by project rule, so routine successes ("Resolved X") pile up. Consider
  limiting persistence to warnings and errors.
- **L10** `splitFolderPath` trims segment whitespace and splits on `\`, so folders with leading/trailing
  spaces or backslashes map to the wrong Drive folder.
- **L11** Cross-platform name hazards (case-only collisions, OS-invalid characters) have no handling and
  become H4 poison pills. Detect them and flag a conflict with a clear message.

## What is solid

- **Merge engine** (`go/merge`): passed 20,000 randomized property cases (added lines never lost in
  3-way or N-way mode, every line kept in 2-way mode, self-merge idempotent, identity cases clean).
  This was measured *after* the merge fixes in commit `264b8c4`; the same test panics on the pre-fix
  code.
- **Cache** write-through ordering, the write gate, and the index maintenance are correct.
- **Token manager** (single-flight refresh, rotation persisted before use) and the **transport**
  (semaphore released before backoff, cancellation honored) are well built, apart from one narrow race:
  a refresh still in flight during logout or re-login can write a stale rotated token back to
  SecretStorage.
- **Queue design**: strict lanes, FIFO actions, and per-path in-flight exclusion. The **open-file lock**
  flush-then-recheck is correct for the merge and pull paths.
- Package godoc and TSDoc coverage are thorough; the design docs match the code closely.

## Test gaps worth closing

All of the above lack tests. Priorities: manual reconcile actions for every winner × {trashed, 404,
duplicate}; offline recovery and poll scheduling (with `testing/synctest`); batch cursor progress with one
permanently failing path; flat-trash confirmation; mass-delete threshold; account change; nested folder
rename; rename-then-edit.

## Suggested order

1. **Data safety**: H2, H3, H5, M1, M3.
2. **Liveness**: H1, H4, M10, L1, L2.
3. **Identity and storage**: H6, the account check from H2, L4.
4. **Drive structure**: M2, M4, L10, L11.
5. **Auth and UX**: M5, L9.
6. **Release readiness**: M6, M7, M8, M9.

## Reproduction tests

H1, H2 and H3 were reproduced with tests run through `go test -overlay`, using the existing fakes in
`go/syncengine/engine_test.go` (H1 with `testing/synctest` virtual time). They aren't committed. The
natural first step of batch 1 is to add them as regression tests that fail before the fix.

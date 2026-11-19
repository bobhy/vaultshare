# Google Drive OAuth & API Layer Design (Agent Spec)

Compact reference for VaultShare V2 authentication (`src/auth/`, `go/gdrive/`) and the Drive v3 client behind `syncengine.DrivePort`.
Human-readable docs live in package godoc (`go/gdrive/`) and module TSDoc (`src/auth/`).
Prior art to port: `../test-gdoc/go/gdrive/{auth,transport,client}.go`, `../test-gdoc/src/auth/oauth.ts`,
and `../test-gdoc/go/cmd/plugin/roundtripper.go`.

## 1. Decisions (Settled)

| Decision | Choice | Consequence |
| :--- | :--- | :--- |
| OAuth scope | `https://www.googleapis.com/auth/drive.file` (non-sensitive) | Plugin sees only files created by this OAuth client (any device). |
| OAuth client & relay | Reuse vault-share's client ID + Cloudflare relay | V1-created Drive files visible to V2; no new cloud setup. |
| Auth failure | Distinct `NeedsLogin` engine state (separate from user Pause) | Re-login resumes automatically; persisted `Paused` untouched. |

- **`drive.file` limitation (user-facing, must be documented)**: Files added to the sync folder via Drive web UI, Google Drive desktop, or other
  apps are invisible to `changes.list`, `files.list`, and Full Scan. The full `drive` scope is *restricted* (Google verification + security
  assessment) and is out of scope.
- **Constants** (single source in `src/auth/constants.ts`, mirrored in Go only where needed):
  - `GOOGLE_CLIENT_ID = '535881224719-5qvscbujq78mpvne1rp3im0cg5cdcu4c.apps.googleusercontent.com'`
  - `RELAY_BASE_URL = 'https://vault-share-auth.bob-hyman.workers.dev'` (`/google/callback`, `/google/token/refresh`)
  - `OAUTH_PROTOCOL_ACTION = 'vault-share-auth'` (shared with V1 → V1/V2 mutual exclusion is mandatory, see functional spec)

## 2. Login Flow (TypeScript Shell, `src/auth/oauth.ts`)

1. Settings **Log in** button → `getAuthorizationUrl()`: `response_type=code`, `access_type=offline`, `prompt=consent`, `scope=drive.file`,
   `redirect_uri=${RELAY_BASE_URL}/google/callback`, `state=btoa(JSON.stringify({app:'vault-share', nonce}))` (32-char crypto nonce).
2. Open URL in system browser (`window.open(url, '_blank', 'noopener,noreferrer')`); relay exchanges the code and redirects to
   `obsidian://vault-share-auth?...&refresh_token=...`.
3. `registerObsidianProtocolHandler(OAUTH_PROTOCOL_ACTION, ...)` → verify `state` (one-shot) → pass refresh token to Go via
   `bridge.setRefreshToken(token)`. TS never stores tokens itself.
4. **Log out** → Clear only the local `SecretStorage` key (do **not** call Google `/revoke`, which revokes the OAuth grant across all devices
   sharing that Google account).
- Before registering the protocol handler, `onload()` runs the V1 presence check (`isVaultSharePresent`, from test-gdoc) and stays dormant
  with a persistent Notice if vault-share is installed or enabled.

## 3. Token Management (Go, `go/gdrive/auth.go`)

Port `TokenManager` from test-gdoc unchanged in behavior:
- Access token cached in memory only; proactive refresh 60 s before expiry via `POST ${RELAY_BASE_URL}/google/token/refresh`.
- Single-flight refresh under mutex; refresh requests bypass the Drive concurrency semaphore (unthrottled transport).
- `400`/`401` from relay → `*AuthError`, records failure time, 60 s cooldown before retry.
- Rotated refresh tokens replace the stored one.
- **Persistence (changed from test-gdoc)**: Go owns the refresh token in `app.secretStorage` via the injected `app` handle (Pattern 1):
  key `vaultshare-<vaultSlug>-refresh-token` (slug: lowercase `[a-z0-9-]`, `"default"` fallback). Loaded at bridge init; written on login and on
  rotation; cleared (`""`) on logout. Access tokens and expiry are never persisted.
- A Drive API `401` with a locally valid token → `Invalidate()` once and retry (handles external revocation/re-consent elsewhere).

### `NeedsLogin` State
- Entered when: no refresh token at startup, or `TokenManager` returns `*AuthError` (relay `400`/`401`, e.g. `invalid_grant`).
- Effect: engine stops dispatching tasks and stops the `changes.list` poller and Full Scan; local events still queue in memory (debounced).
  Persisted `engineState.Paused` is **not** modified.
- UI: `GlobalStatusEvent.NeedsLogin = true` (edge-triggered + 1-minute heartbeat) → indicator shows "needs login"; one persistent Notice
  (`duration = 0`) with a **Log in** action, not repeated while state persists.
- Exit: successful `setRefreshToken` + first successful token refresh → clear `NeedsLogin`, resume (unless user `Paused`), and immediately
  run a local stat diff + `changes.list` poll.

## 4. HTTP Transport (Go, `go/gdrive/transport.go` + `go/cmd/plugin/roundtripper.go`)

- `gdrive.Transport` implements `http.RoundTripper` over a `DoFunc`; Wasm `DoFunc` calls Obsidian `requestUrl` (CORS-free, mobile-safe) with
  `throw: false`, bodies as `ArrayBuffer` via `js.CopyBytesToJS` / `js.CopyBytesToGo`.
- Dynamic counting semaphore capped by `PluginSettings.MaxPendingRequests` (clamp 1–10, default 3); only the RoundTrip holds a permit,
  backoff sleeps happen outside it.
- `requestUrl` cannot be aborted: context cancellation abandons the wait only; the response is discarded when it arrives.
- Retry policy (`RetryOptions`): up to 6 attempts, 0.5 s base doubling to 30 s max, full jitter, on network errors, `429`, `5xx`, and
  `403` with reason `rateLimitExceeded` / `userRateLimitExceeded`. Never retry other `4xx`.
- Offline handling: repeated network errors surface as `ErrOffline`; engine re-queues the task (not a conflict) and the poller backs off to
  the idle cadence until a request succeeds.

## 5. Drive v3 Client Operations (`go/gdrive/client.go`)

All requests use explicit `fields` projections. Standard file projection:
`id,name,mimeType,parents,trashed,modifiedTime,size,md5Checksum,sha256Checksum,headRevisionId`.

| `DrivePort` method | Drive call(s) | Notes |
| :--- | :--- | :--- |
| `EnsureSyncRoot` | `files.list` + `files.create` (folder) | Resolves/creates `My Drive/VaultShare/<syncRootFolder>`; caches IDs in `meta`. |
| `EnsureFolderPath` | `files.list` per segment + `files.create` | **Recursive** (test-gdoc was flat); returns created `FolderRecord`s. |
| `ListChildren` | `files.list` `q='<id>' in parents and trashed=false`, `pageSize=1000` | Paged; folders and files. |
| `FindByPath` | Folder walk via cache, then `files.list` `name='<n>' and '<parent>' in parents` | Returns all matches (duplicates). |
| `GetFileMeta` | `files.get` | |
| `DownloadFile` | `files.get?alt=media` | |
| `DownloadRevision` | `revisions.get?alt=media` | `404` → `(nil, false, nil)` (pruned) → engine uses 2-way merge. |
| `CreateFile` | `files.generateIds` + upload with `id` in metadata | Idempotent retry (see below); sets `modifiedTime` from local mtime. |
| `UpdateFile` | `files.update` upload (`uploadType=multipart`/`resumable`) | Sets `modifiedTime` from local mtime. |
| `MoveRenameFile` | `files.update?addParents=&removeParents=` + `name` | `parents` is not writable in the body. |
| `TrashFile` | `files.update` body `{"trashed": true}` | Recoverable; other clients see `trashed` via `changes.list`. |
| `GetStartPageToken` | `changes.getStartPageToken` | |
| `ListChanges` | `changes.list` (see below) | |

- **`changes.list` request**: `pageToken`, `includeRemoved=true`, `spaces=drive`, `pageSize=1000`,
  `fields=nextPageToken,newStartPageToken,changes(fileId,removed,file(<standard projection>))`. Page through `nextPageToken`;
  `newStartPageToken` is returned to the engine, which persists it only after the batch's tasks finish (see sync-engine-design.md).
- **Uploads**: `multipart` when size ≤ 5 MiB; otherwise `resumable` session with chunks that are multiples of 256 KiB (default 4 MiB),
  omitting `Authorization` on chunk `PUT`s (pre-authorized session URI), resuming from server `Range` on failure, and restarting on `404`/`410`.
- **Duplicate prevention**:
  - Call `files.generateIds` (`space=drive`, `type=files`) **once before** the retry loop; a retried create reuses that ID and treats
    `409 Conflict` → `files.get` verification (exists, non-trashed, matching size/md5) as idempotent success.
  - Per-`(parentID, name)` in-process single-flight lock (not folder-wide across 30 s retries) with a `files.list` existence check before create.
    When `foldersByPath` has duplicate Drive folders, `EnsureFolderPath`/creates target one canonical folder (oldest `modifiedTime`/smallest ID)
    while `FindByPath` queries all matching parent IDs. Cross-client duplicates are resolved by Full Scan + N-version resolution.

## 6. Field Semantics Relied Upon (Verified in Drive v3 `File` Reference)

- `modifiedTime`: writable ("Note that setting modifiedTime also updates modifiedByMeTime").
- `headRevisionId`, `md5Checksum`: output only, available for files with binary content (all plugin uploads; not Google Docs editors files).
- `sha256Checksum`: output only, populated for files with content stored in Drive.
- Revisions: non-head revisions are auto-pruned (~30 days or after 100 revisions); `lastSyncedRevisionId` may disappear, so callers must
  handle `DownloadRevision` returning not-found.

## 7. Testing (`go test ./go/gdrive/...`)

- Fake `DoFunc` / `httptest` server covering: token refresh single-flight, cooldown after `400`/`401`, rotated token persistence callback,
  `401` invalidate-and-retry-once, retry/backoff classification (`429`, `5xx`, `403 rateLimitExceeded` retried; other `4xx` not).
- Semaphore cap honored under concurrent calls; refresh bypasses the semaphore.
- `changes.list` paging and `newStartPageToken` handoff; `includeRemoved` / `trashed` mapping.
- `CreateFile` idempotency: simulated timeout then retry with the same generated ID yields one file.
- Resumable upload resumes from server `Range` after a failed chunk.
- `DownloadRevision` `404` → not-found (no error).
- `NeedsLogin` transitions tested in `go/syncengine` with a fake `DrivePort` returning `*gdrive.AuthError`.

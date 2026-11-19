# VaultShare functionality
Describes the intended user capabilities and architectural overview of the VaultShare plugin.

## User Scenarios
### Coexistance with vault-share plugin
User had previously been using vault-share plugin.
User now installs VaultShare in that vault and enables it.
User gets persistent Notice that VaultShare cannot be active in the same vault as vault-share, 
recommending that user *disable* vault-share in order to use VaultShare.

If user ignores the Notice, VaultShare rejects the enable operation, or, if it cannot do that, at least does not complete initialization and remains dormant.
Later in the same Obsidian session, user *disables* or uninstalls vault-share, then enables or reinstalls VaultShare and it initializes normally.

### Initial sync

As soon as user enables the plugin and completes Google Drive login, plugin starts monitoring for local and remote changes.  
It also starts a (long running) Full Scan to initialize or update the Sync Status DB.

### Initial sync: new Drive root folder

User has a fairly mature local vault established: lots of files and structure.
User now installs VaultShare and accepts all default settings, then logs the plugin into Google Drive for the first time.
We're lucky.  This user did not have a pre-existing vault folder with same name as default sync root setting.

As soon as user clicks log in, VaultShare starts listening for file changes.
It also initiates a Full Scan to eventually initialize the Sync State DB.

In the meantime, if user opens any local file, the open event will drive synchronization with a corresopnding Drive file even if Full Scan hasn't yet discovered it..

User sees some flickering of the global sync indicator icon, but does not experience any additional latency attributable to sync.

### Initial sync: existing Drive root folder, but empty local vault

Here, user had a mature vault in Drive sync root folder.
User installs Obsidian and VaultShare on a new device.  Local vault is empty on the device.
VaultShare initiates a Full Scan (as soon as user logs into Google Drive) and user sees files starting to appear in the local vault folder tree.
User knows it may take many minutes or longer to complete populating the local drive.

In the meantime, if user opens a "new" *note* file (meaning a *text* type file but one that wasn't in the local vault at the time) 
and the open event sync check discovers a preexisting Drive file, 
sync will merge the Drive contents into the mostly empty local file in the edit view.  
For non-text files, sync skips text merge and follows the Sync Resolution pipeline
(see [Open questions](#open-questions) regarding slow creation of non-text files).
 
### Continuous sync
From the user's perspective, synchronization is just always happening while the plugin is active.  
S/He knows that local changes will be replicated to the Drive folder "quickly" (seconds) after they're made locally.  
S/He knows that changes in the Drive folder will be replicated to the local vault "not quite so quickly" (10s of sec, a few minutes)
And s/he knows there's a special case for the files s/he opens for viewing or editing: the act of opening the file will 
merge with the Drive folder "quickly" (seconds), as will subsequent edits made in bursts to the open file.

Most of the sync operations don't affect files the user is looking at and get done invisibly 
in background (though perhaps updating the folder tree or status indicators).

If a sync operation needs to modify a file the user *is* looking at, then user sees a brief interruption:
s/he sees a spinner and is prevented from changing the file while synchronization is updating it.
Any edits already typed before the interruption are preserved, while keystrokes during the brief lock are blocked rather than being replayed afterward.

## Architectural Concepts
Key concepts in the user's mental model and perhaps also in the implementation of the plugin.

### "files" vs "versions (of a file)"
A terminology thing: the term "file" in this spec generally means an entry in the vault index or in the Sync Status DB. 
The term "version" (short for "versions of a file") means an instance of the "file" in the file system or Drive folder 
(despite the API docs for the file system also calling its entities `files`.)
Usually, we're talking about *conflicting* versions of the Obsidian file in different filesystems or Drive folders, and how to reconcile them to a single result.

### Sync State DB
A store of information about files and versions maintained by the plugin to manage synchronization.  

### sync resolution operations

These are the different ways the plugin can resolve conflicting versions of the same Obsidian "file". 

If there is only one version, or all the versions that exist are considered identical, then there is no conflict to resolve.
Versions are considered identical when they have identical metadata (size, modification date, mime type) 
*or*, if checksums are known or can be computed, if the checksums are equal.

There may be more than 2 distinct versions of a file, not just one in Drive and another in the local vault.  
For example, a version might be modified in different ways on 3 clients sharing the same Drive root, or there might be multiple duplicate-named versions in
Google Drive.  So the resolution options are all defined to handle N (`>2`) distinct versions.

User can specify the order in which options are tried to resolve a conflict.  

The options are:
- (for text files only) Merge - Combine all versions into a single, minimal N-way diff with conflict markers
- Keep newer - comparing modification timestamp
- Keep older - comparing modification timestamp
- Keep the local version
- Keep the Drive version
- Manual intervention - user manually reviews the alternative versions and decides how to handle the conflict

### Text file merge

Applies to `.md` files, with conflict markers delimited by backtick.
Also applies to other text files, with same conflict markers, but not delimited by backtick.

Uses N-way diff algorithm pioneered in vault-share to produce minimal conflict regions that are whole lines 
(meaning a conflict marker can never appear within a content line).

The Sync State DB records `lastSyncedRevisionId` (taking a hard dependency on Google Drive revision history).
When a concurrent edit requires a merge, VaultShare fetches that base revision on demand from Drive for a 3-way merge,
falling back to 2-way N-way diff if Drive has pruned the old revision.

The merge may leave conflict markers in the result file, which flags the file for [manual intervention](#manual-intervention).

### Manual Intervention

The sync resolution operations may fail to produce a conflict-free resolution.  

When this happens, the file is flagged as needing manual intervention in the Sync State DB.

At some later time, the user can step through the flagged files and decide how to deal with each one.  
User's options:
- merged text with unresolved conflict markers: edit the file manually and remove all conflicts and conflict markers (plugin provides editor tooling to expedite this)
- conflicting versions of a file: review a list of the available versions, possibly view the individual contents as well, 
then decide which one to keep and which other(s) to discard. ((different from vault-share: since conflicts are not renamed, no particular need
to offer a way to rename result file here.))
- skip it for later.

(see [](#command-reconcile-conflicts))

### handling Google Drive duplicated files
Google Drive API is not atomic and it is possible to find multiple Drive files with the same name and directory path (but different file IDs).
Plugin should avoid creating duplicates in the first place.
But if drive duplicates do appear, the [sync resolution operations](#sync-resolution-operations) 
will treat them as additional conflicting versions of a file to be reconciled.

"Merge (text only)" handles duplicate named Drive files if they are text.  It treats them as additional versions to be merged into a result file.
"Keep Oldest/Newest" also handles duplicate named Drive files.  It considers the modification date of each duplicate in determining a result file.
"Keep Drive" cannot produce a result for duplicated files.  Conflict processing will skip to the next specified resolution option.

Duplicate Drive files may not be detected by event-driven sync as an efficiency concern, but are always detected by Full Scan.

### handling file renames
In-Obsidian renames are handled directly via `vault.on('rename')`. For external renames (where Obsidian emits separate delete and create
events in unspecified order), the plugin correlates pending deletes and creates symmetrically by size/checksum, and matches vanished vs.
new files on startup, mirroring inferred renames on the other side with native move/rename operations.

### handling file deletions

File deletion initiated by the user and not part of a file rename is detected by the plugin and mirrored on the "other" side
with a native trash operation on that store: Google Drive Trash (`trashed: true`, which `changes.list` and `files.get` report explicitly)
and Obsidian's `app.fileManager.trashFile`.
Because Obsidian plugins cannot inspect OS system trash, documentation should recommend using Obsidian's `.trash` folder,
and the Settings page may show a warning notice if `.trash` is not configured.

Then, when one client detects a remote version is missing from the location recorded in its Sync State DB, 
it can look in the archive or wastebasket to confirm some other instance of the plugin initiated the operation.  
If it can't find the version, it treats the missing version as a file conflict requiring manual intervention.  
User then has an opportunity to restore a "missing" version or to actually delete it.

### event-driven sync

Both Obsidian and Google Drive will notify the plugin when a file has changed or has been opened by the user.
(implementation note: Use the google drive `changes.list` api, and iterate through the list of files)

When a notification event is received, ignore it if the referenced file is not within the scope of the plugin: 
ie, on Google Drive, files must be under the sync root folder.
Also ignore it if it's a change event caused by a recently run resolution task, 
or if that file already has a task in the queue (because the task will do whatever resolution is necessary given the 
state of things when it runs).

Local file modification events use a per-path trailing hold-down (debounce) timer with a max-wait cap (configurable in Advanced settings)
so bursts of small edits do not flood Drive. File-open checks are not debounced.
State transitions must be crash-safe (advancing `startPageToken` only after Drive changes are recorded/processed, and flushing debounced edits
on `visibilitychange` as a best effort) because Obsidian does not reliably await async cleanup on exit or mobile backgrounding.

Tasks queued by event notifications are queued at higher priority than tasks queued by full scan.

### full scan sync
Traverses all local and Drive files by directory (not sync state DB), initializing or updating Sync State DB as needed, but queuing
resolution tasks to handle any resolution of any conflicts detected. These resolution tasks are queued at lower priority than event-driven sync.

This may discover previously unknown files, or, in the case of Drive, duplicate-named files.

The scan runs to completion without user interaction, though it may flag conflicts for later manual intervention.

Current status and performance of the scan is visible on the Sync Status page
(and any newly discovered conflicts update the [](#global-conflict-indicator)).
However, the scan does *not* show a distracting Notice when it's done.

Full Scan runs once at startup to initialize or refresh the Sync State DB.

It runs periodically while the plugin is active to audit the Sync State DB (interval configurable in Advanced settings).

On mobile, Obsidian's WebView runtime does not support background plugin execution, so Full Scan cannot continue while backgrounded.
Because the underlying store may change arbitrarily while Obsidian is inactive, Full Scan restarts conservatively from the beginning on wake
rather than skipping files, randomizing its starting place / traversal order each run so partial sessions balance coverage across the vault.

When auditing the Sync State DB, Full Scan logs INFO messages for any unknown or newer files it finds (compared to info in Sync State DB), but
logs WARN messages for any discrepancies it finds (e.g file on disk appears *older* than info in Sync State DB).

Full Scan might have a planning phase, where it estimates the number of operations.  
And if so, it might have "reasonability" checks and warn or require confirmation if too much work or the wrong kind of work is planned.

### conflict handling tasks 

Both event-driven and full scan queue tasks to perform sync resolutions on identified files.
The tasks are self-contained, in that they can perform any or all resolution actions and
determine what to do based on metadata and Sync Status DB data collected when they run. This avoids status race conditions and
allows requests for the same file to be coalesced into one.

Executing tasks are removed from the queue when picked up by a worker.
Deduplication applies only against currently queued tasks by path (upgrading priority to high if a high-priority event arrives for a queued path).
A per-path execution lock prevents two workers from running the same path concurrently; if a new event arrives while a worker is running,
it naturally re-enqueues once without needing a separate rerun flag.

There is a high priority and bulk priority level, such that all the high priority work is completed before any bulk priority work is started.

## User visible commands and settings
(not exhaustive)

### Setting: sync root folder
The folder in Drive that corresponds to the root of the local vault.  Default is the name of the local vault (sanitized as needed for Drive).
The full folder path in Drive is `My Drive / VaultShare / <syncRootFolder>`.  All folders created by the plugin are children of `VaultShare`.

#### Setting: Sync Resolution options
A list of resolution handling techniques:
- Merge (text only) -- if present, must be first in list
- Keep newest
- Keep oldest
- Keep local version
- Keep Drive version - (this one fails if there are multiple Drive files with same name due to API non-atomicity)
- Manual resolution - always present and always last in list

When resolving a conflict, VaultShare tries each option in the order specified by user until one can be applied.

Default: [Merge (text only), Keep newest, Manual resolution]

Alternative setting, appropriate for a secondary, "slave" client:
[Merge (text only), Keep drive, Manual resolution]

### Settings: Advanced (Timers)
Configurable timers grouped under an Advanced section in Settings:
- Local edit hold-down (trailing debounce) timer and max-wait cap for local modification events.
- Google Drive `changes.list` polling timer (active / idle cadence).
- Periodic Full Scan audit timer.

### Command: Full Scan

Allows user to manually initiate a Full Scan, described in [](#full-scan-sync).

### Command: Pause/Resume Conflict Resolution

Allows user to pause execution of conflict resolution tasks.
When user says Pause, any currently active task runs to completion, but no more tasks are dequeued.
When user says Resume, the next task is started.

Global status indicator shows current state.
The paused state persists across plugin restarts; if the plugin loads paused, it displays a persistent Notice
(all plugin Notices persist until explicitly dismissed by the user).

Full Scan has no separate pause control, but automatically pauses whenever conflict resolution is paused and resumes when resumed.

### Command: Reconcile conflicts

Primary command **Reconcile conflicts** opens or focuses a dedicated non-modal workspace tab (`ItemView` via `workspace.getLeaf('tab')`)
that steps through all files flagged for manual intervention in the Sync State DB (`Conflict 1 of N`, with `Previous`, `Next`, and `Skip`).
Additional editor commands **Next conflict marker** and **Previous conflict marker** navigate markers inside an open file.

If the conflict is a merged text file containing conflict markers, VaultShare opens the note in a normal editor tab and uses a
CodeMirror 6 editor extension to highlight conflict regions and offer inline chunk actions (`Accept Local`, `Accept Drive`,
`Accept Both`, `Next Conflict`), clearing the conflict flag in the Sync State DB once no conflict markers remain.

If the conflict involves multiple conflicting versions of the file, the non-modal tab shows:
- a list of candidate versions with metadata (Local vs. Drive, Drive file ID for duplicates, modified time, size),
- a non-modal "View" / side-by-side preview action for inspecting individual versions,
- a "Keep this version" action to choose the winner and discard the rest (no rename control needed).

### Global conflict indicator
A single compact status bar item on desktop (`addStatusBarItem()`), mirrored on a ribbon icon on mobile (where the status bar is hidden),
using a two-part layout:
- **Engine icon (left)**: shows conflict resolution state only (**paused** vs. **running**; Full Scan status is intentionally omitted
  from the icon to keep it uncluttered). Clicking opens or focuses the **Sync Status** window.
- **Conflict badge (right, shown only when conflicts > 0)**: high-contrast count pill (e.g., `⚠ 3`) for pending manual interventions.
  Clicking opens **Reconcile conflicts**.

### Side window: Logging
Uses technology from "vault-share" project.
Filterable severity.
Might have Pause/Resume button.

### Side window: Sync Status

Shows current conflict resolution status (paused or running) and has a Pause/Resume button.
It also shows whether Full Scan is currenty running and some kind of progress bar.

Shows statistics of queued resolution tasks by priority (High vs. Bulk), with separate performance metrics:
- **High-priority latency** (enqueued to completion): EWMA average alongside last task latency (and optional max/p95 in tooltip).
- **Bulk / Full Scan**: progress bar and throughput (`files/s`) rather than queue-wait latency.

Shows counts of kinds of resolutions actually performed counts reset at start of plugin session.

Window autorefreshes, but has a refresh button with a countdown till next refresh.

### Updating an open file, file locking
A sync event or Full Scan may need to update a file while the user has it open.  When that happens, the open file view will display an overlay with text "Synchronizing..." that prevents user changes to the file.  
Any unsaved edits typed before the lock are flushed before reading, while keystrokes during the overlay are blocked rather than buffered.
Sync will then read the content of the file if needed for the sync operation, write back the updated content and finally remove the overlay, allowing 
user interaction once again.
Sync will arrange the order of operations to minimize the time that the file is locked out. (e.g, will read any necessary content from Google Drive before locking the file)

## Open questions
1. Slow creation of non-text files: if a non-text file is being written slowly (e.g. during initial Full Scan or an external export),
   syncing before writes finish could cause destructive updates.
2. Quick edit and immediate app switch (especially mobile): crash-safe persistence handles eventual recovery, but if a user edits a note and
   immediately switches away before high-priority sync runs, the edit won't be pushed until Obsidian is reopened. Consider an explicit
   "Leave Obsidian" / "Flush & Sync Now" button to cancel hold-down timers and drain high-priority sync before switching apps
   (note: Obsidian's mobile plugin API cannot programmatically minimize or switch apps on iOS/Android, so this would likely show a clear
   "Synced — safe to switch" confirmation instead of auto-minimizing).

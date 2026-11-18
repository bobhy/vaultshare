# VaultShare functionality
Describes the intended user capabilities and architectural overview of the VaultShare plugin.

## User Scenarios
todo: user scenarios
### Initial sync: new Drive root folder
### Initial sync: existing Drive root folder
### Continuous sync

## User visible commands and settings
(not exhaustive)

### Setting: sync root folder
The folder in Drive that corresponds to the root of the local vault.

### Setting: sync resolution options

#### Sync Resolution
A list of max 3 elements, each one one of:
- Merge (text only)
- Keep newest
- Keep oldest
- Keep local version
- Keep Drive version - (this one fails if there are multiple Drive files with same name due to API non-atomicity)
- Manual resolution (called "leave" in the Concepts)

User can specify the options in any order, but each option can be chosen only once.
Default: [Merge (text only), Keep newest, Manual resolution]

Document a recommended default for second client that the user considers the the "satellite":
[Merge (text only), Keep drive, Manual resolution]

#### Disable Text Merge
A boolean, default No.

If enabled, prevents Merge operation from applying to text files.

### Command: Full Scan

Traverses all local and Drive files by directory (not sync state store), syncing each and updating sync state store.  
This may discover previously unknown files, or, in the case of Drive, duplicate-named files.
Flags (in sync state store) merged text files left with conflict markers and sets of conflicting versions that fell through to "manual resolution".

This might have a planning phase, where it estimates the number of operations.  And if so, it might have "reasonability" checks and warn or require confirmation if too much work is planned.

### Command: Reconcile Next / Previous conflict
(or is this Reconcile All Conflicts?)

Does a complete pass through sync state store handling all currently flagged conflicts (e.g, flagged by previous Full Scan).
If a merged text file contains conflict markers, opens an editor view and does a Find Next Conflict.  When user closes the file, moves to the next conflict
If it's a multi-file conflict (one that fell through to "manual resolution"), shows a list of candidate files, offers some kind of file view experience and lets user choose the winner (or skip)


### Global conflict indicator
An icon in window chrome or a toolbar that shows whether there are any conflicts at the moment.  
It might be a badge showing number of conflicts, updated in real time.

### Side window: Logging
Uses technology from "vault-share" project.
Filterable severity.
Might have Pause/Resume button.

### Side window: Sync Status

Shows current status (paused or running) and has a Pause/Resume button.

Want to show *current backlog* and *rate of progress* .  Might also have heuristics in place to automatically pause sync if guardrails are exceeded.

This is based on the notion that there is a queue of pending sync operations that is being worked on "in the background".  
The status window updates ~~1/sec while sync is not paused.

For backlog: 1. display counts of operations in queue, grouped by sync operation: e.g. N1 pending text merges, N2 pending keep newer, ...
2. display counts of pending manual resolutions, one count for text merges that still have conflict markers another count for file conflicts awaiting user attention.

For rate of work: (At least one statistic: latency in the sync queue: time from receipt of a file change event to completion of the deferred operation.  Others are TBD: once we have a design, we can investigate how to instrument it)

## Architectural Concepts

### sync resolution operations

These are the different ways the plugin handles conflicting versions of the "same" file.  
If there is only one version, or all the versions that exist are considered identical, then there is no conflict to resolve.
Files are considered identical when they have identical metadata (size, modification date, mime type) 
*or*, if checksums are known or can be computed, if the checksums are equal.

There may be more than 2 versions of a file in conflict, not just one in Drive and another in the local vault.  
For example, a file might be modified in different ways on 3 clients sharing the same Drive root, or there might be multiple duplicate-named files in
Google Drive.  So the resolution options are all defined to handle N distinct versions.

User can specify a sequence of options to use to try to resolve conflict.  The "winning" version replaces all others.
The options for each step are:
- (for text files only) Merge - Combine all versions into a single, minimal N-way diff with conflict markers
- Keep newer - comparing modification timestamp
- Keep older - comparing modification timestamp
- Keep the local version
- Keep the Drive version
- Leave for later - user manually reviews the versions (at some later time) and decides which one to keep, replacing all other versions.

### Text file merge

Applies to `.md` files, with conflict markers delimited by backtick
Also applies to other text files, with same conflict markers, but not delimited by backtick

Uses N-way diff algorithm pioneered in vault-share to produce minimal conflict regions that are whole lines 
(meaning a conflict marker can never appear within a content line).

### event-driven sync

Syncing that relies on the underlying store to provide notification that a given file has been changed.  Both Obsidian and google drive can do this (implementation note: Use the google drive `changes.list` api )

The general idea is to receive an event notification that a particular file has changed (in Drive or vault), filter out ignorable events (file outside sync root folder, event is an echo from a prior sync operation) and queue a deferred work item to complete the rest of the sync operation.

In general the deferred sync operation will need to gather metadata from all affected files and may also need to read or write file contents.  
All I/O should be async and care taken to not block event loops.

### handling Google Drive duplicated files
Google Drive API is not atomic and it is possible to find multiple Drive files with the same name and directory path (but different file IDs).
Plugin should avoid creating duplicates in the first place.
But if drive duplicates do appear, the N-version support in [sync resolution operations](#sync-resolution-operations) will resolve them.

"Keep Drive" will not resolve in this case, but processing will fall through to the next step in the sequence, which will usually be "leave".

Duplicate Drive files may not be detected by event-driven sync, but must be detected by the full scan and 
the manual resolution dialog must also surface duplicate file names to the user in a reasonable way.

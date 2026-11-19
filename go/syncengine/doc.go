// Package syncengine implements VaultShare V2's pure-Go synchronization engine:
// two-lane priority scheduling, per-path execution locking, symmetric rename
// correlation, N-version conflict resolution, changes.list delta polling, and
// randomized pause-aware Full Scan auditing.
//
// All host I/O (Obsidian Vault, Google Drive API, Auth secrets, and UI event
// delivery) is abstracted behind VaultPort, DrivePort, AuthPort, and EventSink
// so the entire engine runs natively under go test ./go/syncengine/...
package syncengine

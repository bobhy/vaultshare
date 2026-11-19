// Package syncstate implements VaultShare V2's 3-store synchronization state model
// (`files`, `folders`, `meta`), write-through in-memory Cache, and pure-Go
// MemoryStore for native unit testing.
//
// No base file content blobs or pending task queues are persisted here: base
// revisions are fetched on demand from Google Drive via LastSyncedRevisionID,
// and crash recovery replays unacknowledged changes.list batches alongside a
// fast startup stat diff.
//
// Package syncstate contains zero syscall/js imports; the browser IndexedDB
// Store implementation lives in go/cmd/plugin/idb_store.go.
package syncstate

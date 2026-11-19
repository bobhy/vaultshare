//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"syscall/js"

	"github.com/hack-pad/go-indexeddb/idb"

	"vaultshare/go/syncstate"
)

const (
	idbSchemaVersion = 1
	storeFiles       = "files"
	storeFolders     = "folders"
	storeMeta        = "meta"
	indexDriveFileID = "byDriveFileId"
	indexFolderPath  = "byPath"
)

// idbStore implements syncstate.Store directly over browser window.indexedDB
// using github.com/hack-pad/go-indexeddb/idb.
type idbStore struct {
	mu      sync.Mutex
	dbName  string
	db      *idb.Database
	opening chan struct{}
	jsonVal js.Value
	objCtor js.Value
}

func newIDBStore(vaultName string) *idbStore {
	return &idbStore{
		dbName:  dbNameForVault(vaultName),
		jsonVal: js.Global().Get("JSON"),
		objCtor: js.Global().Get("Object"),
	}
}

func dbNameForVault(vaultName string) string {
	return "vaultshare-" + slugifyVaultName(vaultName)
}

func (s *idbStore) SetVaultName(vaultName string) {
	next := dbNameForVault(vaultName)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbName == next {
		return
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
	s.dbName = next
}

func (s *idbStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// openDB single-flights factory.Open across concurrent callers so duplicate
// IDB connections never open simultaneously or trigger onblocked.
func (s *idbStore) openDB(ctx context.Context) (*idb.Database, error) {
	for {
		s.mu.Lock()
		if s.db != nil {
			db := s.db
			s.mu.Unlock()
			return db, nil
		}
		if s.opening != nil {
			ch := s.opening
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ch:
				continue
			}
		}
		ch := make(chan struct{})
		s.opening = ch
		dbName := s.dbName
		s.mu.Unlock()

		db, err := s.openDBOnce(ctx, dbName)

		s.mu.Lock()
		if err == nil && s.dbName == dbName {
			s.db = db
		} else if db != nil {
			_ = db.Close()
		}
		close(ch)
		s.opening = nil
		s.mu.Unlock()

		if err != nil {
			return nil, err
		}
		if s.dbName == dbName {
			return db, nil
		}
	}
}

func (s *idbStore) openDBOnce(ctx context.Context, dbName string) (*idb.Database, error) {
	factory := idb.Global()
	if factory == nil {
		return nil, fmt.Errorf("window.indexedDB is unavailable")
	}

	req, err := factory.Open(ctx, dbName, idbSchemaVersion, func(db *idb.Database, _ uint, _ uint) error {
		// Pre-1.0 schema upgrades cold-start all stores cleanly.
		names, namesErr := db.ObjectStoreNames()
		if namesErr != nil {
			return namesErr
		}
		for _, name := range names {
			if delErr := db.DeleteObjectStore(name); delErr != nil {
				return delErr
			}
		}

		filesOS, err := db.CreateObjectStore(storeFiles, idb.ObjectStoreOptions{
			KeyPath: js.ValueOf("path"),
		})
		if err != nil {
			return err
		}
		if _, err := filesOS.CreateIndex(indexDriveFileID, js.ValueOf("driveFileIds"), idb.IndexOptions{
			Unique:     false,
			MultiEntry: true,
		}); err != nil {
			return err
		}

		foldersOS, err := db.CreateObjectStore(storeFolders, idb.ObjectStoreOptions{
			KeyPath: js.ValueOf("driveFolderId"),
		})
		if err != nil {
			return err
		}
		if _, err := foldersOS.CreateIndex(indexFolderPath, js.ValueOf("path"), idb.IndexOptions{
			Unique: false,
		}); err != nil {
			return err
		}

		if _, err := db.CreateObjectStore(storeMeta, idb.ObjectStoreOptions{
			KeyPath: js.ValueOf("key"),
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open IndexedDB %q: %w", dbName, err)
	}
	db, err := req.Await(ctx)
	if err != nil {
		return nil, fmt.Errorf("await IndexedDB %q open: %w", dbName, err)
	}
	return db, nil
}

func (s *idbStore) marshalJS(v any) (js.Value, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return js.Undefined(), err
	}
	return s.jsonVal.Call("parse", string(raw)), nil
}

type metaRow struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func (s *idbStore) LoadAll(ctx context.Context) (*syncstate.Snapshot, error) {
	db, err := s.openDB(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := db.Transaction(idb.TransactionReadOnly, storeFiles, storeFolders, storeMeta)
	if err != nil {
		return nil, err
	}

	filesOS, err := tx.ObjectStore(storeFiles)
	if err != nil {
		return nil, err
	}
	foldersOS, err := tx.ObjectStore(storeFolders)
	if err != nil {
		return nil, err
	}
	metaOS, err := tx.ObjectStore(storeMeta)
	if err != nil {
		return nil, err
	}

	// Queue all three GetAll requests synchronously in the same IDB transaction.
	filesReq, err := filesOS.GetAll()
	if err != nil {
		return nil, err
	}
	foldersReq, err := foldersOS.GetAll()
	if err != nil {
		return nil, err
	}
	metaReq, err := metaOS.GetAll()
	if err != nil {
		return nil, err
	}

	filesVal, err := filesReq.Request.Await(ctx)
	if err != nil {
		return nil, err
	}
	foldersVal, err := foldersReq.Request.Await(ctx)
	if err != nil {
		return nil, err
	}
	metaVal, err := metaReq.Request.Await(ctx)
	if err != nil {
		return nil, err
	}

	snap := &syncstate.Snapshot{
		Meta: make(map[string]json.RawMessage),
	}
	if !filesVal.IsUndefined() && !filesVal.IsNull() && filesVal.Length() > 0 {
		raw := s.jsonVal.Call("stringify", filesVal).String()
		if err := json.Unmarshal([]byte(raw), &snap.Files); err != nil {
			return nil, fmt.Errorf("decode files from IndexedDB: %w", err)
		}
	}
	if !foldersVal.IsUndefined() && !foldersVal.IsNull() && foldersVal.Length() > 0 {
		raw := s.jsonVal.Call("stringify", foldersVal).String()
		if err := json.Unmarshal([]byte(raw), &snap.Folders); err != nil {
			return nil, fmt.Errorf("decode folders from IndexedDB: %w", err)
		}
	}
	if !metaVal.IsUndefined() && !metaVal.IsNull() && metaVal.Length() > 0 {
		raw := s.jsonVal.Call("stringify", metaVal).String()
		var rows []metaRow
		if err := json.Unmarshal([]byte(raw), &rows); err != nil {
			return nil, fmt.Errorf("decode meta from IndexedDB: %w", err)
		}
		for _, r := range rows {
			snap.Meta[r.Key] = r.Value
		}
	}
	return snap, nil
}

func (s *idbStore) PutFile(ctx context.Context, rec syncstate.FileRecord) error {
	syncstate.NormalizeFileRecord(&rec)
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	recJS, err := s.marshalJS(rec)
	if err != nil {
		return err
	}
	tx, err := db.Transaction(idb.TransactionReadWrite, storeFiles)
	if err != nil {
		return err
	}
	os, err := tx.ObjectStore(storeFiles)
	if err != nil {
		return err
	}
	if _, err := os.Put(recJS); err != nil {
		return err
	}
	return tx.Await(ctx)
}

func (s *idbStore) DeleteFile(ctx context.Context, path string) error {
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	tx, err := db.Transaction(idb.TransactionReadWrite, storeFiles)
	if err != nil {
		return err
	}
	os, err := tx.ObjectStore(storeFiles)
	if err != nil {
		return err
	}
	if _, err := os.Delete(js.ValueOf(path)); err != nil {
		return err
	}
	return tx.Await(ctx)
}

func (s *idbStore) MigrateFilePath(ctx context.Context, oldPath, newPath string, updated syncstate.FileRecord) error {
	updated.Path = newPath
	syncstate.NormalizeFileRecord(&updated)
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	recJS, err := s.marshalJS(updated)
	if err != nil {
		return err
	}
	tx, err := db.Transaction(idb.TransactionReadWrite, storeFiles)
	if err != nil {
		return err
	}
	os, err := tx.ObjectStore(storeFiles)
	if err != nil {
		return err
	}
	if _, err := os.Delete(js.ValueOf(oldPath)); err != nil {
		return err
	}
	if _, err := os.Put(recJS); err != nil {
		return err
	}
	return tx.Await(ctx)
}

func (s *idbStore) PutFolders(ctx context.Context, folders []syncstate.FolderRecord) error {
	if len(folders) == 0 {
		return nil
	}
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	jsVals := make([]js.Value, len(folders))
	for i, f := range folders {
		v, err := s.marshalJS(f)
		if err != nil {
			return err
		}
		jsVals[i] = v
	}
	tx, err := db.Transaction(idb.TransactionReadWrite, storeFolders)
	if err != nil {
		return err
	}
	os, err := tx.ObjectStore(storeFolders)
	if err != nil {
		return err
	}
	for _, v := range jsVals {
		if _, err := os.Put(v); err != nil {
			return err
		}
	}
	return tx.Await(ctx)
}

func (s *idbStore) DeleteFolder(ctx context.Context, driveFolderID string) error {
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	tx, err := db.Transaction(idb.TransactionReadWrite, storeFolders)
	if err != nil {
		return err
	}
	os, err := tx.ObjectStore(storeFolders)
	if err != nil {
		return err
	}
	if _, err := os.Delete(js.ValueOf(driveFolderID)); err != nil {
		return err
	}
	return tx.Await(ctx)
}

func (s *idbStore) PutMeta(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	rowJS, err := s.marshalJS(metaRow{Key: key, Value: raw})
	if err != nil {
		return err
	}
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	tx, err := db.Transaction(idb.TransactionReadWrite, storeMeta)
	if err != nil {
		return err
	}
	os, err := tx.ObjectStore(storeMeta)
	if err != nil {
		return err
	}
	if _, err := os.Put(rowJS); err != nil {
		return err
	}
	return tx.Await(ctx)
}

func (s *idbStore) ResetSyncScope(ctx context.Context) error {
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	tx, err := db.Transaction(idb.TransactionReadWrite, storeFiles, storeFolders, storeMeta)
	if err != nil {
		return err
	}
	filesOS, err := tx.ObjectStore(storeFiles)
	if err != nil {
		return err
	}
	foldersOS, err := tx.ObjectStore(storeFolders)
	if err != nil {
		return err
	}
	metaOS, err := tx.ObjectStore(storeMeta)
	if err != nil {
		return err
	}
	if _, err := filesOS.Clear(); err != nil {
		return err
	}
	if _, err := foldersOS.Clear(); err != nil {
		return err
	}
	for _, k := range []string{syncstate.MetaKeySyncRoot, syncstate.MetaKeyChangeToken, syncstate.MetaKeyFullScan} {
		if _, err := metaOS.Delete(js.ValueOf(k)); err != nil {
			return err
		}
	}
	return tx.Await(ctx)
}

func (s *idbStore) ClearAll(ctx context.Context) error {
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	tx, err := db.Transaction(idb.TransactionReadWrite, storeFiles, storeFolders, storeMeta)
	if err != nil {
		return err
	}
	for _, name := range []string{storeFiles, storeFolders, storeMeta} {
		os, err := tx.ObjectStore(name)
		if err != nil {
			return err
		}
		if _, err := os.Clear(); err != nil {
			return err
		}
	}
	return tx.Await(ctx)
}

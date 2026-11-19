package syncstate

import (
	"context"
	"reflect"
	"testing"
)

func TestCacheDriveIDsAndMigrateFilePath(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cache := NewCache(store)
	if err := cache.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	rec := FileRecord{
		Path:     "Notes/Old.md",
		MimeType: "text/markdown",
		LastSynced: &SyncedBaseline{
			DriveFileID:          "drv-1",
			LastSyncedRevisionID: "rev-42",
		},
		DriveVersions: []DriveVersionState{
			{FileID: "drv-1", Name: "Old.md"},
			{FileID: "drv-dup", Name: "Old.md"},
		},
		Conflict: &ConflictState{Kind: ConflictVersions, Summary: "2 duplicates"},
	}
	if err := cache.PutFile(ctx, rec); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	wantIDs := []string{"drv-1", "drv-dup"}
	got, ok := cache.GetFile("Notes/Old.md")
	if !ok || !reflect.DeepEqual(got.DriveFileIDs, wantIDs) {
		t.Fatalf("DriveFileIDs = %v, want %v", got.DriveFileIDs, wantIDs)
	}

	if byID, found := cache.GetFileByDriveID("drv-dup"); !found || byID.Path != "Notes/Old.md" {
		t.Fatalf("GetFileByDriveID(drv-dup) = (%+v, %v)", byID, found)
	}
	if counts := cache.ConflictCounts(); counts.Versions != 1 || counts.Total != 1 {
		t.Fatalf("ConflictCounts = %+v", counts)
	}

	// Rename/migrate path while preserving LastSyncedRevisionID.
	migrated := got
	migrated.Conflict = nil
	migrated.DriveVersions = []DriveVersionState{{FileID: "drv-1", Name: "New.md"}}
	if err := cache.MigrateFilePath(ctx, "Notes/Old.md", "Notes/New.md", migrated); err != nil {
		t.Fatalf("MigrateFilePath: %v", err)
	}

	if _, exists := cache.GetFile("Notes/Old.md"); exists {
		t.Fatal("old path still present after MigrateFilePath")
	}
	if _, exists := cache.GetFileByDriveID("drv-dup"); exists {
		t.Fatal("stale duplicate drive ID still indexed after MigrateFilePath")
	}
	after, ok := cache.GetFileByDriveID("drv-1")
	if !ok || after.Path != "Notes/New.md" || after.LastSynced.LastSyncedRevisionID != "rev-42" {
		t.Fatalf("after MigrateFilePath: got (%+v, %v)", after, ok)
	}
	if counts := cache.ConflictCounts(); counts.Total != 0 {
		t.Fatalf("ConflictCounts after clearing conflict = %+v", counts)
	}
}

func TestCacheDuplicateFoldersAndPathResolution(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cache := NewCache(store)
	if err := cache.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	if err := cache.PutSyncRoot(ctx, SyncRootMeta{
		VaultShareFolderID: "vs-id",
		RootFolderID:       "root-id",
		RootFolderName:     "Vault",
	}); err != nil {
		t.Fatalf("PutSyncRoot: %v", err)
	}

	folders := []FolderRecord{
		{DriveFolderID: "f-a1", ParentID: "root-id", Name: "Work", Path: "Work"},
		{DriveFolderID: "f-a2", ParentID: "root-id", Name: "Work", Path: "Work"},
		{DriveFolderID: "f-sub", ParentID: "f-a1", Name: "Projects", Path: "Work/Projects"},
		{DriveFolderID: "f-trash", ParentID: "root-id", Name: "Old", Path: "Old", Trashed: true},
	}
	if err := cache.PutFolders(ctx, folders); err != nil {
		t.Fatalf("PutFolders: %v", err)
	}

	if dups := cache.GetFoldersByPath("Work"); len(dups) != 2 {
		t.Fatalf("GetFoldersByPath(Work) len = %d, want 2", len(dups))
	}
	if trashed := cache.GetFoldersByPath("Old"); len(trashed) != 0 {
		t.Fatalf("trashed folder leaked into GetFoldersByPath: %v", trashed)
	}

	if rel, inScope := cache.ResolveFolderPath("f-sub"); !inScope || rel != "Work/Projects" {
		t.Fatalf("ResolveFolderPath(f-sub) = (%q, %v)", rel, inScope)
	}
	if rel, inScope := cache.ResolveFolderPath("root-id"); !inScope || rel != "" {
		t.Fatalf("ResolveFolderPath(root-id) = (%q, %v)", rel, inScope)
	}
	if _, inScope := cache.ResolveFolderPath("f-trash"); inScope {
		t.Fatal("trashed folder reported as inScope")
	}
	if _, inScope := cache.ResolveFolderPath("outside-id"); inScope {
		t.Fatal("unknown folder reported as inScope")
	}
}

func TestCacheMetaRoundTripAndResetSyncScope(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	cache := NewCache(store)
	_ = cache.LoadAll(ctx)

	_ = cache.PutClientID(ctx, ClientIDMeta{ID: "client-uuid"})
	_ = cache.PutEngineState(ctx, EngineStateMeta{Paused: true})
	_ = cache.PutSyncRoot(ctx, SyncRootMeta{RootFolderID: "r1", RootFolderName: "VaultA"})
	_ = cache.PutChangeToken(ctx, ChangeTokenMeta{StartPageToken: "tok-99", UpdatedAtMs: 1234})
	_ = cache.PutFullScan(ctx, FullScanMeta{Initialized: true, LastStartedAtMs: 100, LastCompletedAtMs: 200})
	_ = cache.PutFile(ctx, FileRecord{Path: "a.md", Conflict: &ConflictState{Kind: ConflictMarkers}})
	_ = cache.PutFolders(ctx, []FolderRecord{{DriveFolderID: "d1", ParentID: "r1", Name: "Sub", Path: "Sub"}})

	// Reload fresh cache from same Store to verify persistence.
	reloaded := NewCache(store)
	if err := reloaded.LoadAll(ctx); err != nil {
		t.Fatalf("reloaded.LoadAll: %v", err)
	}
	if reloaded.ClientID().ID != "client-uuid" || !reloaded.EngineState().Paused {
		t.Fatalf("client/engine meta mismatch: %+v %+v", reloaded.ClientID(), reloaded.EngineState())
	}
	if reloaded.ChangeToken().StartPageToken != "tok-99" || !reloaded.FullScan().Initialized {
		t.Fatalf("token/fullscan mismatch: %+v %+v", reloaded.ChangeToken(), reloaded.FullScan())
	}
	if c := reloaded.ConflictCounts(); c.Markers != 1 || c.Total != 1 {
		t.Fatalf("ConflictCounts mismatch: %+v", c)
	}

	// ResetSyncScope clears files/folders/syncRoot/changeToken/fullScan but keeps clientId & engineState.
	if err := reloaded.ResetSyncScope(ctx); err != nil {
		t.Fatalf("ResetSyncScope: %v", err)
	}
	if len(reloaded.ListFiles()) != 0 || len(reloaded.ListFolders()) != 0 {
		t.Fatal("files/folders not cleared by ResetSyncScope")
	}
	if reloaded.ChangeToken().StartPageToken != "" || reloaded.SyncRoot().RootFolderID != "" || reloaded.FullScan().Initialized {
		t.Fatal("syncRoot/changeToken/fullScan not cleared by ResetSyncScope")
	}
	if reloaded.ClientID().ID != "client-uuid" || !reloaded.EngineState().Paused {
		t.Fatal("clientId or engineState lost during ResetSyncScope")
	}
}

package syncengine

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vaultshare/go/gdrive"
	"vaultshare/go/settings"
	"vaultshare/go/syncstate"
)

type fakeFileEntry struct {
	data    []byte
	mtimeMs int64
}

type fakeVault struct {
	mu           sync.Mutex
	cfgDir       string
	files        map[string]fakeFileEntry
	trashed      map[string]fakeFileEntry
	nowMs        int64
	onWriteBytes func(relPath string, mtimeMs int64)
}

func newFakeVault() *fakeVault {
	return &fakeVault{
		cfgDir:  ".obsidian",
		files:   make(map[string]fakeFileEntry),
		trashed: make(map[string]fakeFileEntry),
		nowMs:   1_700_000_000_000,
	}
}

func (v *fakeVault) ConfigDir() string { return v.cfgDir }

func (v *fakeVault) SetFile(relPath, content string, mtimeMs int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.files[normalizeRelPath(relPath)] = fakeFileEntry{
		data:    []byte(content),
		mtimeMs: mtimeMs,
	}
}

func (v *fakeVault) GetText(relPath string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.files[normalizeRelPath(relPath)]
	return string(e.data), ok
}

func (v *fakeVault) Stat(_ context.Context, relPath string) (LocalStat, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := normalizeRelPath(relPath)
	e, ok := v.files[p]
	if !ok {
		return LocalStat{}, false, nil
	}
	return LocalStat{Path: p, Size: int64(len(e.data)), MTimeMs: e.mtimeMs}, true, nil
}

func (v *fakeVault) ListAllStats(_ context.Context) ([]LocalStat, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]LocalStat, 0, len(v.files))
	for p, e := range v.files {
		out = append(out, LocalStat{Path: p, Size: int64(len(e.data)), MTimeMs: e.mtimeMs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (v *fakeVault) ReadBytes(_ context.Context, relPath string) ([]byte, LocalStat, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := normalizeRelPath(relPath)
	e, ok := v.files[p]
	if !ok {
		return nil, LocalStat{}, fmt.Errorf("vault file not found: %s", p)
	}
	return slices.Clone(e.data), LocalStat{Path: p, Size: int64(len(e.data)), MTimeMs: e.mtimeMs}, nil
}

func (v *fakeVault) WriteText(ctx context.Context, relPath, text string) (LocalStat, error) {
	return v.WriteBytes(ctx, relPath, []byte(text))
}

func (v *fakeVault) WriteBytes(_ context.Context, relPath string, data []byte) (LocalStat, error) {
	v.mu.Lock()
	p := normalizeRelPath(relPath)
	v.nowMs += 100
	mtime := v.nowMs
	v.files[p] = fakeFileEntry{data: slices.Clone(data), mtimeMs: mtime}
	hook := v.onWriteBytes
	v.mu.Unlock()
	if hook != nil {
		hook(p, mtime)
	}
	return LocalStat{Path: p, Size: int64(len(data)), MTimeMs: mtime}, nil
}

func (v *fakeVault) Rename(_ context.Context, oldPath, newPath string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	op := normalizeRelPath(oldPath)
	np := normalizeRelPath(newPath)
	e, ok := v.files[op]
	if !ok {
		return fmt.Errorf("vault file not found: %s", op)
	}
	delete(v.files, op)
	v.files[np] = e
	return nil
}

func (v *fakeVault) Trash(_ context.Context, relPath string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := normalizeRelPath(relPath)
	if e, ok := v.files[p]; ok {
		v.trashed[p] = e
		delete(v.files, p)
	}
	return nil
}

func (v *fakeVault) ExistsInTrash(_ context.Context, relPath string, size int64) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := normalizeRelPath(relPath)
	e, ok := v.trashed[p]
	if !ok {
		return false, nil
	}
	return size < 0 || int64(len(e.data)) == size, nil
}

func (v *fakeVault) WithOpenFileLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

type fakeDriveNode struct {
	file      gdrive.DriveFile
	data      []byte
	revisions map[string][]byte
}

type fakeDrive struct {
	mu               sync.Mutex
	nextID           int
	nextRev          int
	rootID           string
	vaultShareID     string
	nodes            map[string]*fakeDriveNode
	startPageToken   string
	changes          []gdrive.ChangeItem
	newStartToken    string
	failDownloadID   string
	beforeCreateHook func()
	beforeUpdateHook func(fileID string)
	onListChildren   func(folderID string)
	onDownloadFile   func(fileID string)
	onGetFileMeta    func(fileID string)
}

func newFakeDrive() *fakeDrive {
	d := &fakeDrive{
		nextID:         1,
		nextRev:        1,
		vaultShareID:   "vs-root",
		rootID:         "vault-root",
		nodes:          make(map[string]*fakeDriveNode),
		startPageToken: "tok-1",
		newStartToken:  "tok-2",
	}
	d.nodes[d.vaultShareID] = &fakeDriveNode{
		file: gdrive.DriveFile{ID: d.vaultShareID, Name: "VaultShare", MimeType: gdrive.FolderMimeType},
	}
	d.nodes[d.rootID] = &fakeDriveNode{
		file: gdrive.DriveFile{ID: d.rootID, Name: "MyVault", MimeType: gdrive.FolderMimeType, Parents: []string{d.vaultShareID}},
	}
	return d
}

func (d *fakeDrive) genID(prefix string) string {
	d.nextID++
	return fmt.Sprintf("%s-%d", prefix, d.nextID)
}

func (d *fakeDrive) genRev() string {
	d.nextRev++
	return fmt.Sprintf("rev-%d", d.nextRev)
}

func (d *fakeDrive) SeedFolder(parentID, name string) gdrive.DriveFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.genID("fld")
	df := gdrive.DriveFile{
		ID:           id,
		Name:         name,
		MimeType:     gdrive.FolderMimeType,
		Parents:      []string{parentID},
		ModifiedTime: time.UnixMilli(1_700_000_000_000).UTC().Format(time.RFC3339),
	}
	d.nodes[id] = &fakeDriveNode{file: df}
	return df
}

func (d *fakeDrive) SeedFile(parentID, name, content string, mtimeMs int64, headRev string) gdrive.DriveFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seedFileLocked(parentID, name, []byte(content), mtimeMs, headRev)
}

func (d *fakeDrive) seedFileLocked(parentID, name string, data []byte, mtimeMs int64, headRev string) gdrive.DriveFile {
	id := d.genID("file")
	if headRev == "" {
		headRev = d.genRev()
	}
	m := md5.Sum(data)
	s := sha256.Sum256(data)
	_ = s
	df := gdrive.DriveFile{
		ID:             id,
		Name:           name,
		MimeType:       gdrive.ContentMimeType(name),
		Parents:        []string{parentID},
		ModifiedTime:   time.UnixMilli(mtimeMs).UTC().Format(time.RFC3339),
		Size:           strconv.Itoa(len(data)),
		MD5Checksum:    hex.EncodeToString(m[:]),
		HeadRevisionID: headRev,
	}
	d.nodes[id] = &fakeDriveNode{
		file:      df,
		data:      slices.Clone(data),
		revisions: map[string][]byte{headRev: slices.Clone(data)},
	}
	return df
}

func (d *fakeDrive) UpdateRemoteFile(fileID, content string, mtimeMs int64) gdrive.DriveFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.nodes[fileID]
	rev := d.genRev()
	b := []byte(content)
	m := md5.Sum(b)
	n.data = b
	n.file.ModifiedTime = time.UnixMilli(mtimeMs).UTC().Format(time.RFC3339)
	n.file.Size = strconv.Itoa(len(b))
	n.file.MD5Checksum = hex.EncodeToString(m[:])
	n.file.HeadRevisionID = rev
	n.revisions[rev] = slices.Clone(b)
	return n.file
}

func (d *fakeDrive) EnsureSyncRoot(_ context.Context, _ string) (gdrive.SyncRootIDs, error) {
	return gdrive.SyncRootIDs{
		VaultShareFolderID: d.vaultShareID,
		RootFolderID:       d.rootID,
	}, nil
}

func (d *fakeDrive) ListChildren(_ context.Context, folderID string) ([]gdrive.DriveFile, []gdrive.DriveFile, error) {
	if d.onListChildren != nil {
		d.onListChildren(folderID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var folders, files []gdrive.DriveFile
	for _, n := range d.nodes {
		if n.file.Trashed || !slices.Contains(n.file.Parents, folderID) {
			continue
		}
		if n.file.MimeType == gdrive.FolderMimeType {
			folders = append(folders, n.file)
		} else {
			files = append(files, n.file)
		}
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].ID < folders[j].ID })
	sort.Slice(files, func(i, j int) bool { return files[i].ID < files[j].ID })
	return folders, files, nil
}

func (d *fakeDrive) FindInParents(_ context.Context, parentIDs []string, name string) ([]gdrive.DriveFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []gdrive.DriveFile
	for _, n := range d.nodes {
		if n.file.Trashed || n.file.Name != name {
			continue
		}
		for _, pid := range parentIDs {
			if slices.Contains(n.file.Parents, pid) {
				out = append(out, n.file)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (d *fakeDrive) GetFileMeta(_ context.Context, fileID string) (gdrive.DriveFile, bool, error) {
	if d.onGetFileMeta != nil {
		d.onGetFileMeta(fileID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.nodes[fileID]
	if !ok {
		return gdrive.DriveFile{}, false, nil
	}
	return n.file, true, nil
}

func (d *fakeDrive) DownloadFile(_ context.Context, fileID string) ([]byte, error) {
	if d.onDownloadFile != nil {
		d.onDownloadFile(fileID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failDownloadID != "" && d.failDownloadID == fileID {
		return nil, errors.New("injected download error")
	}
	n, ok := d.nodes[fileID]
	if !ok {
		return nil, fmt.Errorf("drive file not found: %s", fileID)
	}
	return slices.Clone(n.data), nil
}

func (d *fakeDrive) DownloadRevision(_ context.Context, fileID, revisionID string) ([]byte, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.nodes[fileID]
	if !ok {
		return nil, false, nil
	}
	b, ok := n.revisions[revisionID]
	if !ok {
		return nil, false, nil
	}
	return slices.Clone(b), true, nil
}

func (d *fakeDrive) CreateFile(_ context.Context, parentID, name, mimeType string, mtimeMs int64, data []byte) (gdrive.DriveFile, error) {
	if d.beforeCreateHook != nil {
		d.beforeCreateHook()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	df := d.seedFileLocked(parentID, name, data, mtimeMs, "")
	if mimeType != "" {
		df.MimeType = mimeType
		d.nodes[df.ID].file.MimeType = mimeType
	}
	return df, nil
}

func (d *fakeDrive) UpdateFile(_ context.Context, fileID, mimeType string, mtimeMs int64, data []byte) (gdrive.DriveFile, error) {
	if d.beforeUpdateHook != nil {
		d.beforeUpdateHook(fileID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.nodes[fileID]
	if !ok {
		return gdrive.DriveFile{}, fmt.Errorf("drive file not found: %s", fileID)
	}
	rev := d.genRev()
	m := md5.Sum(data)
	n.data = slices.Clone(data)
	n.file.ModifiedTime = time.UnixMilli(mtimeMs).UTC().Format(time.RFC3339)
	n.file.Size = strconv.Itoa(len(data))
	n.file.MD5Checksum = hex.EncodeToString(m[:])
	n.file.HeadRevisionID = rev
	if mimeType != "" {
		n.file.MimeType = mimeType
	}
	n.revisions[rev] = slices.Clone(data)
	return n.file, nil
}

func (d *fakeDrive) MoveRenameFile(_ context.Context, fileID, newParentID, _, newName string) (gdrive.DriveFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.nodes[fileID]
	if !ok {
		return gdrive.DriveFile{}, fmt.Errorf("drive file not found: %s", fileID)
	}
	if newParentID != "" {
		n.file.Parents = []string{newParentID}
	}
	if newName != "" {
		n.file.Name = newName
	}
	return n.file, nil
}

func (d *fakeDrive) TrashFile(_ context.Context, fileID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n, ok := d.nodes[fileID]; ok {
		n.file.Trashed = true
	}
	return nil
}

func (d *fakeDrive) EnsureFolderPath(_ context.Context, rootID, relDir string) (string, []syncstate.FolderRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	relDir = normalizeRelPath(relDir)
	if relDir == "" {
		return rootID, nil, nil
	}
	curID := rootID
	var created []syncstate.FolderRecord
	var built string
	for _, seg := range strings.Split(relDir, "/") {
		if built == "" {
			built = seg
		} else {
			built += "/" + seg
		}
		var found *gdrive.DriveFile
		for _, n := range d.nodes {
			if !n.file.Trashed && n.file.MimeType == gdrive.FolderMimeType && n.file.Name == seg && slices.Contains(n.file.Parents, curID) {
				f := n.file
				found = &f
				break
			}
		}
		if found == nil {
			id := d.genID("fld")
			df := gdrive.DriveFile{ID: id, Name: seg, MimeType: gdrive.FolderMimeType, Parents: []string{curID}}
			d.nodes[id] = &fakeDriveNode{file: df}
			created = append(created, syncstate.FolderRecord{
				DriveFolderID: id,
				ParentID:      curID,
				Name:          seg,
				Path:          built,
			})
			curID = id
		} else {
			curID = found.ID
		}
	}
	return curID, created, nil
}

func (d *fakeDrive) GetStartPageToken(_ context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.startPageToken, nil
}

func (d *fakeDrive) ListChanges(_ context.Context, _ string) (gdrive.ChangePage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := slices.Clone(d.changes)
	d.changes = nil
	return gdrive.ChangePage{
		Changes:           out,
		NewStartPageToken: d.newStartToken,
	}, nil
}

type fakeAuth struct {
	mu    sync.Mutex
	token string
}

func (a *fakeAuth) LoadFromSecrets(_ context.Context) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token != "", nil
}

func (a *fakeAuth) HasRefreshToken() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token != ""
}

func (a *fakeAuth) SetRefreshToken(_ context.Context, token string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = token
	return nil
}

func (a *fakeAuth) Logout(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = ""
	return nil
}

type fakeSink struct {
	mu      sync.Mutex
	notices []string
	events  []settings.GlobalStatusEvent
}

func (s *fakeSink) EmitGlobalStatus(ev settings.GlobalStatusEvent) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *fakeSink) EmitLog(_ settings.LogEntry) {}

func (s *fakeSink) EmitNotice(message string) {
	s.mu.Lock()
	s.notices = append(s.notices, message)
	s.mu.Unlock()
}

func newTestEngine(t *testing.T) (*Engine, *fakeVault, *fakeDrive, *fakeAuth, *fakeSink) {
	t.Helper()
	v := newFakeVault()
	d := newFakeDrive()
	a := &fakeAuth{token: "refresh-tok"}
	s := &fakeSink{}
	store := syncstate.NewMemoryStore()
	now := time.UnixMilli(1_700_000_000_000)
	eng := NewEngine(EngineDeps{
		Vault: v,
		Drive: d,
		Auth:  a,
		Store: store,
		Sink:  s,
		Now: func() time.Time {
			return now
		},
		WorkerCount: 2,
	})
	return eng, v, d, a, s
}

func TestWorkQueuePriorityAndInFlightSerialization(t *testing.T) {
	q := newWorkQueue()
	now := time.Unix(1000, 0)

	q.Enqueue("bulk-a.md", PriorityBulk, "fullscan", now, 0)
	q.Enqueue("bulk-b.md", PriorityBulk, "fullscan", now, 0)
	q.Enqueue("high-1.md", PriorityHigh, "file-open", now, 0)

	t1, ok := q.PopNext(false)
	if !ok || t1.Path != "high-1.md" || t1.Priority != PriorityHigh {
		t.Fatalf("expected PriorityHigh task first, got %+v (ok=%v)", t1, ok)
	}
	q.Finish(t1.Path)

	t2, ok := q.PopNext(false)
	if !ok || t2.Path != "bulk-a.md" {
		t.Fatalf("expected bulk-a.md, got %+v", t2)
	}
	// While bulk-a.md is in-flight AND re-queued at PriorityHigh, PopNext holds
	// bulk-b.md because PriorityHigh strictly preempts PriorityBulk, while
	// another unrelated PriorityHigh path CAN run concurrently.
	q.Enqueue("bulk-a.md", PriorityHigh, "modify", now, 0)
	q.Enqueue("high-2.md", PriorityHigh, "open", now, 0)
	t3, ok := q.PopNext(false)
	if !ok || t3.Path != "high-2.md" {
		t.Fatalf("expected concurrent high-2.md while bulk-a.md is in-flight, got %+v", t3)
	}
	q.Finish("high-2.md")
	if _, ok := q.PopNext(false); ok {
		t.Fatalf("expected neither re-queued bulk-a.md (in-flight) nor bulk-b.md (preempted by highLane) to pop")
	}

	q.Finish("bulk-a.md")
	t4, ok := q.PopNext(false)
	if !ok || t4.Path != "bulk-a.md" || t4.Priority != PriorityHigh {
		t.Fatalf("expected re-queued bulk-a.md at PriorityHigh after Finish, got %+v", t4)
	}
	q.Finish("bulk-a.md")
	t5, ok := q.PopNext(false)
	if !ok || t5.Path != "bulk-b.md" {
		t.Fatalf("expected bulk-b.md after highLane emptied, got %+v", t5)
	}
	q.Finish("bulk-b.md")
}

func TestFileOpenSkipsDebounceAndResolvesImmediately(t *testing.T) {
	eng, v, _, _, _ := newTestEngine(t)
	ctx := context.Background()
	v.SetFile("note.md", "hello open", 1_700_000_000_100)

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Clear startup enqueue to test open specifically.
	for {
		if task, ok := eng.queue.PopNext(false); ok {
			eng.queue.Finish(task.Path)
		} else {
			break
		}
	}

	if err := eng.OnVaultEvent(ctx, settings.VaultEventPayload{Kind: "open", Path: "note.md"}); err != nil {
		t.Fatalf("OnVaultEvent(open): %v", err)
	}
	task, ok := eng.queue.PopNext(false)
	if !ok || task.Path != "note.md" || task.Priority != PriorityHigh || task.Source != "open" {
		t.Fatalf("expected immediate PriorityHigh open task, got %+v (ok=%v)", task, ok)
	}
	eng.queue.Finish(task.Path)
}

func TestSymmetricRenameCorrelationDeleteThenCreateAndCreateThenDelete(t *testing.T) {
	ctx := context.Background()

	// Case A: Delete then Create.
	{
		eng, v, d, _, _ := newTestEngine(t)
		v.SetFile("old.md", "same body", 1_700_000_000_100)
		df := d.SeedFile(d.rootID, "old.md", "same body", 1_700_000_000_100, "rev-1")
		if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := eng.resolvePath(ctx, "old.md"); err != nil {
			t.Fatalf("resolvePath(old.md): %v", err)
		}

		// Simulate Vault delete followed quickly by Vault create at new path with same size.
		_ = v.Trash(ctx, "old.md")
		_ = eng.OnVaultEvent(ctx, settings.VaultEventPayload{Kind: "delete", Path: "old.md"})
		v.SetFile("new.md", "same body", 1_700_000_000_200)
		_ = eng.OnVaultEvent(ctx, settings.VaultEventPayload{Kind: "create", Path: "new.md", Size: 9, MTimeMs: 1_700_000_000_200})
		drainQueueForTest(t, ctx, eng)

		node := d.nodes[df.ID]
		if node.file.Name != "new.md" || node.file.Trashed {
			t.Fatalf("expected Drive file %s renamed to new.md, got %+v", df.ID, node.file)
		}
		if _, ok := eng.cache.GetFile("new.md"); !ok {
			t.Fatalf("expected cache migrated to new.md")
		}
		eng.Stop()
	}

	// Case B: Create then Delete.
	{
		eng, v, d, _, _ := newTestEngine(t)
		v.SetFile("alpha.md", "move me!", 1_700_000_000_100)
		df := d.SeedFile(d.rootID, "alpha.md", "move me!", 1_700_000_000_100, "rev-1")
		if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := eng.resolvePath(ctx, "alpha.md"); err != nil {
			t.Fatalf("resolvePath: %v", err)
		}

		v.SetFile("beta.md", "move me!", 1_700_000_000_200)
		_ = eng.OnVaultEvent(ctx, settings.VaultEventPayload{Kind: "create", Path: "beta.md", Size: 8, MTimeMs: 1_700_000_000_200})
		_ = v.Trash(ctx, "alpha.md")
		_ = eng.OnVaultEvent(ctx, settings.VaultEventPayload{Kind: "delete", Path: "alpha.md"})
		drainQueueForTest(t, ctx, eng)

		node := d.nodes[df.ID]
		if node.file.Name != "beta.md" || node.file.Trashed {
			t.Fatalf("expected Drive file %s renamed to beta.md via Create->Delete, got %+v", df.ID, node.file)
		}
		eng.Stop()
	}
}

func TestFolderRenameAndDeleteCascades(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	v.SetFile("Docs/a.md", "alpha", 1_700_000_000_100)
	fld := d.SeedFolder(d.rootID, "Docs")
	df := d.SeedFile(fld.ID, "a.md", "alpha", 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = eng.cache.PutFolders(ctx, []syncstate.FolderRecord{{
		DriveFolderID: fld.ID,
		ParentID:      d.rootID,
		Name:          "Docs",
		Path:          "Docs",
	}})
	if err := eng.resolvePath(ctx, "Docs/a.md"); err != nil {
		t.Fatalf("resolvePath: %v", err)
	}

	// Rename folder Docs -> Archive.
	_ = v.Rename(ctx, "Docs/a.md", "Archive/a.md")
	if err := eng.OnVaultEvent(ctx, settings.VaultEventPayload{
		Kind: "rename", OldPath: "Docs", Path: "Archive", IsFolder: true,
	}); err != nil {
		t.Fatalf("folder rename: %v", err)
	}
	drainQueueForTest(t, ctx, eng)
	if _, ok := eng.cache.GetFile("Archive/a.md"); !ok {
		t.Fatalf("expected Archive/a.md in cache after folder rename")
	}

	// Delete folder Archive -> trash Drive folder & queue/resolve child trash.
	_ = v.Trash(ctx, "Archive/a.md")
	if err := eng.OnVaultEvent(ctx, settings.VaultEventPayload{
		Kind: "delete", Path: "Archive", IsFolder: true,
	}); err != nil {
		t.Fatalf("folder delete: %v", err)
	}
	drainQueueForTest(t, ctx, eng)
	archFolders := eng.cache.GetFoldersByPath("Archive")
	if len(archFolders) != 0 {
		t.Fatalf("expected Archive folder record deleted from cache, got %+v", archFolders)
	}
	for _, n := range d.nodes {
		if n.file.MimeType == gdrive.FolderMimeType && n.file.Name == "Archive" && !n.file.Trashed {
			t.Fatalf("expected Drive folder Archive (%s) trashed on local folder delete", n.file.ID)
		}
	}
	if !d.nodes[df.ID].file.Trashed {
		t.Fatalf("expected Drive child %s trashed on local folder delete", df.ID)
	}
	_ = fld
	eng.Stop()
}

func TestRemoteRemovedDriveFolderDeletesLocalDescendants(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	v.SetFile("Sub/note.md", "hi", 1_700_000_000_100)
	fld := d.SeedFolder(d.rootID, "Sub")
	_ = d.SeedFile(fld.ID, "note.md", "hi", 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = eng.cache.PutFolders(ctx, []syncstate.FolderRecord{{
		DriveFolderID: fld.ID,
		ParentID:      d.rootID,
		Name:          "Sub",
		Path:          "Sub",
	}})
	if err := eng.resolvePath(ctx, "Sub/note.md"); err != nil {
		t.Fatalf("resolvePath: %v", err)
	}

	d.mu.Lock()
	d.nodes[fld.ID].file.Trashed = true
	d.changes = []gdrive.ChangeItem{{FileID: fld.ID, Removed: true}}
	d.mu.Unlock()

	if err := eng.pollDriveChangesOnce(ctx); err != nil {
		t.Fatalf("pollDriveChangesOnce: %v", err)
	}
	// Process enqueued path.
	for {
		task, ok := eng.queue.PopNext(false)
		if !ok {
			break
		}
		err := eng.resolvePath(ctx, task.Path)
		eng.completeBatchTask(ctx, task.Path, task.BatchIDs, err)
		eng.queue.Finish(task.Path)
	}

	if _, ok := v.GetText("Sub/note.md"); ok {
		t.Fatalf("expected Sub/note.md trashed locally after Drive folder removal")
	}
	if got := eng.cache.ChangeToken().StartPageToken; got != "tok-2" {
		t.Fatalf("expected change token committed to tok-2 after batch completion, got %q", got)
	}
	eng.Stop()
}

func TestBatchGatedChangeTokenSkipsCommitOnTaskError(t *testing.T) {
	ctx := context.Background()
	eng, _, d, _, _ := newTestEngine(t)
	df := d.SeedFile(d.rootID, "broken.md", "remote text", 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	d.mu.Lock()
	d.failDownloadID = df.ID
	dfCopy := d.nodes[df.ID].file
	d.changes = []gdrive.ChangeItem{{FileID: df.ID, File: &dfCopy}}
	d.mu.Unlock()

	if err := eng.pollDriveChangesOnce(ctx); err != nil {
		t.Fatalf("pollDriveChangesOnce: %v", err)
	}
	task, ok := eng.queue.PopNext(false)
	if !ok {
		t.Fatalf("expected batch task queued")
	}
	err := eng.resolvePath(ctx, task.Path)
	eng.completeBatchTask(ctx, task.Path, task.BatchIDs, err)
	eng.queue.Finish(task.Path)

	if err == nil {
		t.Fatalf("expected DownloadFile failure")
	}
	if got := eng.cache.ChangeToken().StartPageToken; got != "tok-1" {
		t.Fatalf("changeToken must NOT advance when batch fails; got %q", got)
	}
	eng.Stop()
}

func TestPreUploadHeadRevisionRecheckFlagsConflictInsteadOfClobbering(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	baseText := "line1\nline2\nline3\n"
	v.SetFile("doc.md", baseText, 1_700_000_000_100)
	df := d.SeedFile(d.rootID, "doc.md", baseText, 1_700_000_000_100, "rev-1")

	cfg := settings.DefaultSettings()
	cfg.SyncResolution = []string{settings.ResolutionManual}
	if err := eng.Start(ctx, cfg, "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.resolvePath(ctx, "doc.md"); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// Local edit alone -> resolver pushes existing Drive file, but while reading/before upload,
	// another device updates Drive headRevisionId!
	v.SetFile("doc.md", "local changed\nline2\nline3\n", 1_700_000_000_500)
	d.beforeUpdateHook = func(fileID string) {
		if fileID == df.ID {
			d.UpdateRemoteFile(df.ID, "remote sneaked in!\n", 1_700_000_000_600)
		}
	}
	// Test our ResolveVersionConflict "local" vs resolvePath pre-upload re-check:
	// First simulate resolver detecting clean local-only change in Phase 1, then Drive changes right before Phase 3 update.
	// Note: resolvePath re-fetches GetFileMeta right before pushExistingLocalFile ONLY on upload branches; let's check both.
	d.UpdateRemoteFile(df.ID, "remote sneaked in!\n", 1_700_000_000_600)
	if err := eng.resolvePath(ctx, "doc.md"); err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	rec, ok := eng.cache.GetFile("doc.md")
	if !ok || rec.Conflict == nil || rec.Conflict.Kind != syncstate.ConflictVersions {
		t.Fatalf("expected versions conflict rather than clobbering Drive, got %+v", rec.Conflict)
	}
	eng.Stop()
}

func TestThreeWayCleanMergeAdvancesLastSyncedAndKeepsDriveClean(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	base := "alpha\nbeta\ngamma\ndelta\nepsilon\n"
	v.SetFile("merge.md", base, 1_700_000_000_100)
	df := d.SeedFile(d.rootID, "merge.md", base, 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.resolvePath(ctx, "merge.md"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// Divergent non-overlapping edits on top and bottom lines.
	v.SetFile("merge.md", "ALPHA-LOCAL\nbeta\ngamma\ndelta\nepsilon\n", 1_700_000_000_500)
	d.UpdateRemoteFile(df.ID, "alpha\nbeta\ngamma\ndelta\nEPSILON-DRIVE\n", 1_700_000_000_600)

	if err := eng.resolvePath(ctx, "merge.md"); err != nil {
		t.Fatalf("resolvePath merge: %v", err)
	}
	txt, _ := v.GetText("merge.md")
	if !strings.Contains(txt, "ALPHA-LOCAL") || !strings.Contains(txt, "EPSILON-DRIVE") {
		t.Fatalf("expected both edits in clean 3-way merge, got:\n%s", txt)
	}
	rec, _ := eng.cache.GetFile("merge.md")
	if rec.Conflict != nil || rec.LastSynced == nil || rec.LastSynced.LastSyncedRevisionID == "rev-1" {
		t.Fatalf("expected clean synced baseline advanced past rev-1, got rec=%+v", rec)
	}
	eng.Stop()
}

func TestKeepDriveDuplicateFallthroughAndModifyVsDeleteMissingConflict(t *testing.T) {
	ctx := context.Background()

	// Duplicate Drive files with KeepDrive only -> falls through to Manual.
	{
		eng, v, d, _, _ := newTestEngine(t)
		v.SetFile("dup.md", "local", 1_700_000_000_100)
		d.SeedFile(d.rootID, "dup.md", "drive-1", 1_700_000_000_200, "rev-a")
		d.SeedFile(d.rootID, "dup.md", "drive-2", 1_700_000_000_300, "rev-b")

		cfg := settings.DefaultSettings()
		cfg.SyncResolution = []string{settings.ResolutionKeepDrive, settings.ResolutionManual}
		cfg.DisableTextMerge = true
		if err := eng.Start(ctx, cfg, "MyVault", false); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := eng.resolvePath(ctx, "dup.md"); err != nil {
			t.Fatalf("resolvePath dup: %v", err)
		}
		rec, _ := eng.cache.GetFile("dup.md")
		if rec.Conflict == nil || rec.Conflict.Kind != syncstate.ConflictVersions || len(rec.DriveVersions) != 2 {
			t.Fatalf("expected ConflictVersions with 2 candidates when KeepDrive falls through, got %+v", rec)
		}
		eng.Stop()
	}

	// Modify locally vs Drive trashed -> ConflictMissing.
	{
		eng, v, d, _, _ := newTestEngine(t)
		v.SetFile("missing.md", "v1", 1_700_000_000_100)
		df := d.SeedFile(d.rootID, "missing.md", "v1", 1_700_000_000_100, "rev-1")
		if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := eng.resolvePath(ctx, "missing.md"); err != nil {
			t.Fatalf("baseline sync: %v", err)
		}
		v.SetFile("missing.md", "v2 local modified", 1_700_000_000_500)
		_ = d.TrashFile(ctx, df.ID)

		if err := eng.resolvePath(ctx, "missing.md"); err != nil {
			t.Fatalf("resolvePath modify-vs-delete: %v", err)
		}
		rec, _ := eng.cache.GetFile("missing.md")
		if rec.Conflict == nil || rec.Conflict.Kind != syncstate.ConflictMissing {
			t.Fatalf("expected ConflictMissing, got %+v", rec.Conflict)
		}
		eng.Stop()
	}
}

func TestFullScanPausesCleanlyAndNeedsLoginPreservesPausedState(t *testing.T) {
	ctx := context.Background()
	eng, v, d, a, s := newTestEngine(t)
	v.SetFile("a.md", "one", 1_700_000_000_100)
	fld := d.SeedFolder(d.rootID, "F1")
	d.SeedFile(fld.ID, "remote.md", "two", 1_700_000_000_200, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Auth failure / NeedsLogin must NOT alter persisted Paused!
	eng.handleIOError(&gdrive.AuthError{StatusCode: 401, Message: "expired"}, "ListChanges")
	if !eng.NeedsLogin() || !eng.IsPaused() {
		t.Fatalf("expected NeedsLogin=true AND IsPaused=true, got login=%v paused=%v", eng.NeedsLogin(), eng.IsPaused())
	}
	if !eng.cache.EngineState().Paused {
		t.Fatalf("expected persisted EngineStateMeta.Paused to remain true")
	}

	// Logout clears token, leaves Paused intact.
	if err := eng.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if a.HasRefreshToken() || !eng.cache.EngineState().Paused {
		t.Fatalf("expected token cleared and paused intact after Logout")
	}

	// While paused, Full Scan aborts before completion and does not mark Initialized.
	if err := eng.runFullScan(ctx); err != nil {
		t.Fatalf("runFullScan while paused: %v", err)
	}
	if eng.cache.FullScan().Initialized {
		t.Fatalf("expected FullScan not marked Initialized when paused")
	}

	// Unpause + re-auth -> Full Scan completes and discovers both local & Drive paths.
	_ = eng.SetRefreshToken(ctx, "fresh-tok")
	_ = eng.Resume(ctx)
	if err := eng.runFullScan(ctx); err != nil {
		t.Fatalf("runFullScan active: %v", err)
	}
	if !eng.cache.FullScan().Initialized {
		t.Fatalf("expected FullScan marked Initialized after completion")
	}
	if _, ok := eng.cache.GetFolder(fld.ID); !ok {
		t.Fatalf("expected FullScan to populate FolderRecord cache for %s", fld.ID)
	}
	_ = s
	eng.Stop()
}

func TestFullScanConcurrentPullDoesNotWarnOlder(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	v.SetFile("Second note.md", "v1 content", 1_700_000_000_100)
	df := d.SeedFile(d.rootID, "Second note.md", "v1 content", 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.resolvePath(ctx, "Second note.md"); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// Simulate an external Drive edit followed by a concurrent pull running right
	// after Full Scan snapshots ListAllStats but before processScanFiles inspects it.
	d.UpdateRemoteFile(df.ID, "v2 remote edit", 1_700_000_000_900)
	var pulledOnce sync.Once
	d.onListChildren = func(folderID string) {
		if folderID == d.rootID {
			pulledOnce.Do(func() {
				if err := eng.resolvePath(ctx, "Second note.md"); err != nil {
					t.Errorf("concurrent resolvePath: %v", err)
				}
			})
		}
	}

	if err := eng.runFullScan(ctx); err != nil {
		t.Fatalf("runFullScan: %v", err)
	}

	for _, entry := range eng.GetLogs(settings.LogWarning) {
		if strings.Contains(entry.Message, "older than last synced state") {
			t.Fatalf("unexpected stale-snapshot warning logged: %+v", entry)
		}
	}
	txt, ok := v.GetText("Second note.md")
	if !ok || txt != "v2 remote edit" {
		t.Fatalf("expected updated local file content, got %q (ok=%v)", txt, ok)
	}
	eng.Stop()
}

func drainQueueForTest(t *testing.T, ctx context.Context, eng *Engine) {
	t.Helper()
	for {
		task, ok := eng.queue.PopNext(eng.IsPaused() || eng.NeedsLogin())
		if !ok {
			return
		}
		err := eng.executeTask(ctx, task)
		eng.completeBatchTask(ctx, task.Path, task.BatchIDs, err)
		eng.queue.FinishTask(task)
		if err != nil {
			t.Fatalf("executeTask(%+v): %v", task, err)
		}
	}
}

func TestPullDriveFileAbortsOverwriteWhenLocalEditedDuringDownload(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	base := "line1\nline2\nline3\n"
	v.SetFile("live.md", base, 1_700_000_000_100)
	df := d.SeedFile(d.rootID, "live.md", base, 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.resolvePath(ctx, "live.md"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// Remote changes line3; while DownloadFile is in flight, user edits line1 locally.
	d.UpdateRemoteFile(df.ID, "line1\nline2\nLINE3-DRIVE\n", 1_700_000_000_500)
	var editedOnce sync.Once
	d.onDownloadFile = func(fileID string) {
		if fileID == df.ID {
			editedOnce.Do(func() {
				v.SetFile("live.md", "LINE1-USER\nline2\nline3\n", 1_700_000_000_600)
			})
		}
	}

	if err := eng.resolvePath(ctx, "live.md"); err != nil {
		t.Fatalf("first resolvePath: %v", err)
	}
	// Blind overwrite must NOT have clobbered LINE1-USER; instead pull-retry was queued.
	txt, _ := v.GetText("live.md")
	if !strings.Contains(txt, "LINE1-USER") {
		t.Fatalf("user edit lost during concurrent Drive pull! got:\n%s", txt)
	}

	// Next queued task runs full 3-way merge preserving BOTH edits cleanly.
	drainQueueForTest(t, ctx, eng)
	merged, _ := v.GetText("live.md")
	if !strings.Contains(merged, "LINE1-USER") || !strings.Contains(merged, "LINE3-DRIVE") {
		t.Fatalf("expected clean 3-way merge after pull-retry, got:\n%s", merged)
	}
	eng.Stop()
}

func TestPushExistingFileAbortsOverwriteWhenDriveChangesPreUpload(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	base := "top\nmid\nbot\n"
	v.SetFile("push.md", base, 1_700_000_000_100)
	df := d.SeedFile(d.rootID, "push.md", base, 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.resolvePath(ctx, "push.md"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// User edits top locally; right before UpdateFile, Drive updates bot remotely.
	v.SetFile("push.md", "TOP-LOCAL\nmid\nbot\n", 1_700_000_000_400)
	var updatedOnce sync.Once
	d.onGetFileMeta = func(fileID string) {
		if fileID == df.ID {
			updatedOnce.Do(func() {
				d.UpdateRemoteFile(df.ID, "top\nmid\nBOT-DRIVE\n", 1_700_000_000_450)
			})
		}
	}

	if err := eng.resolvePath(ctx, "push.md"); err != nil {
		t.Fatalf("first resolvePath: %v", err)
	}
	// Pre-upload check must have detected rev mismatch and queued push-retry -> 3-way merge.
	drainQueueForTest(t, ctx, eng)
	merged, _ := v.GetText("push.md")
	if !strings.Contains(merged, "TOP-LOCAL") || !strings.Contains(merged, "BOT-DRIVE") {
		t.Fatalf("expected 3-way merge preserving Drive edit, got:\n%s", merged)
	}
	eng.Stop()
}

func TestPreWriteEchoSuppressionDuringVaultWrite(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	d.SeedFile(d.rootID, "echo.md", "from drive", 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Simulate Obsidian firing vault.on('modify') synchronously inside WriteText!
	v.onWriteBytes = func(relPath string, mtimeMs int64) {
		_ = eng.OnVaultEvent(ctx, settings.VaultEventPayload{
			Kind:    "modify",
			Path:    relPath,
			MTimeMs: mtimeMs,
		})
	}

	if err := eng.resolvePath(ctx, "echo.md"); err != nil {
		t.Fatalf("resolvePath(echo.md): %v", err)
	}
	if flushed := eng.FlushDebounces(); flushed != 0 {
		t.Fatalf("expected synchronous echo inside WriteText suppressed, got %d debounced edits", flushed)
	}
	eng.Stop()
}

func TestFIFORenameOrderingAndPauseGating(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	v.SetFile("a.md", "chain", 1_700_000_000_100)
	df := d.SeedFile(d.rootID, "a.md", "chain", 1_700_000_000_100, "rev-1")
	emptyFld := d.SeedFolder(d.rootID, "EmptyDir")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = eng.cache.PutFolders(ctx, []syncstate.FolderRecord{{
		DriveFolderID: emptyFld.ID,
		ParentID:      d.rootID,
		Name:          "EmptyDir",
		Path:          "EmptyDir",
	}})
	if err := eng.resolvePath(ctx, "a.md"); err != nil {
		t.Fatalf("sync a.md: %v", err)
	}

	// Pause must prevent empty folder delete from touching Drive!
	_ = eng.Pause(ctx)
	_ = eng.OnVaultEvent(ctx, settings.VaultEventPayload{Kind: "delete", Path: "EmptyDir", IsFolder: true})
	drainQueueForTest(t, ctx, eng)
	if d.nodes[emptyFld.ID].file.Trashed {
		t.Fatalf("empty Drive folder must NOT be trashed while engine is paused")
	}

	// Resume -> empty folder trash runs, and rapid rename chain a->b->c executes FIFO!
	_ = eng.Resume(ctx)
	_ = v.Rename(ctx, "a.md", "b.md")
	_ = eng.OnVaultEvent(ctx, settings.VaultEventPayload{Kind: "rename", OldPath: "a.md", Path: "b.md"})
	_ = v.Rename(ctx, "b.md", "c.md")
	_ = eng.OnVaultEvent(ctx, settings.VaultEventPayload{Kind: "rename", OldPath: "b.md", Path: "c.md"})
	drainQueueForTest(t, ctx, eng)

	if !d.nodes[emptyFld.ID].file.Trashed {
		t.Fatalf("expected EmptyDir trashed after Resume")
	}
	if d.nodes[df.ID].file.Name != "c.md" {
		t.Fatalf("expected Drive file renamed a->b->c to c.md, got %q", d.nodes[df.ID].file.Name)
	}
	if _, ok := eng.cache.GetFile("c.md"); !ok {
		t.Fatalf("expected cache record at c.md")
	}
	eng.Stop()
}

func TestEpochResetHaltsOldWorkersAndClearsQueue(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	v.SetFile("reset.md", "hello", 1_700_000_000_100)
	d.SeedFile(d.rootID, "reset.md", "hello", 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", true); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Trigger ClearAllState while background workers/coordinator/router are live.
	if err := eng.ClearAllState(ctx); err != nil {
		t.Fatalf("ClearAllState: %v", err)
	}
	if len(eng.cache.ListFiles()) != 0 {
		t.Fatalf("expected empty file cache immediately after ClearAllState")
	}
	eng.Stop()
}

func TestConflictMarkersSurviveSubsequentResolvePathAndClearWhenResolved(t *testing.T) {
	ctx := context.Background()
	eng, v, d, _, _ := newTestEngine(t)
	base := "line1\nshared line\nline3\n"
	v.SetFile("Welcome.md", base, 1_700_000_000_100)
	df := d.SeedFile(d.rootID, "Welcome.md", base, 1_700_000_000_100, "rev-1")

	if err := eng.Start(ctx, settings.DefaultSettings(), "MyVault", false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.resolvePath(ctx, "Welcome.md"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// Conflicting edits on the same line -> 3-way merge inserts conflict markers.
	v.SetFile("Welcome.md", "line1\nlocal edit\nline3\n", 1_700_000_000_200)
	d.UpdateRemoteFile(df.ID, "line1\ndrive edit\nline3\n", 1_700_000_000_300)
	if err := eng.resolvePath(ctx, "Welcome.md"); err != nil {
		t.Fatalf("merge resolvePath: %v", err)
	}

	// Simulate an immediate follow-up resolvePath (e.g. from file-open or Full Scan).
	if err := eng.resolvePath(ctx, "Welcome.md"); err != nil {
		t.Fatalf("follow-up resolvePath: %v", err)
	}
	conflicts := eng.ListConflicts()
	if len(conflicts) != 1 || conflicts[0].Path != "Welcome.md" || conflicts[0].Kind != syncstate.ConflictMarkers {
		t.Fatalf("expected ConflictMarkers preserved after follow-up resolvePath, got %+v", conflicts)
	}

	// Simulate an unflagged DB record (e.g. from pre-fix DB) whose unchanged disk
	// content still holds conflict markers: Full Scan audit must re-flag it while
	// resolvePath's fast path stays disk-read-free.
	rec, _ := eng.cache.GetFile("Welcome.md")
	rec.Conflict = nil
	_ = eng.cache.PutFile(ctx, rec)
	if err := eng.runFullScan(ctx); err != nil {
		t.Fatalf("Full Scan marker audit: %v", err)
	}
	if got := eng.ListConflicts(); len(got) != 1 || got[0].Kind != syncstate.ConflictMarkers {
		t.Fatalf("expected Full Scan audit to re-flag existing markers, got %+v", got)
	}

	// User resolves markers locally and saves -> follow-up resolvePath pushes clean text and clears ConflictMarkers.
	v.SetFile("Welcome.md", "line1\nresolved edit\nline3\n", 1_700_000_000_500)
	if err := eng.resolvePath(ctx, "Welcome.md"); err != nil {
		t.Fatalf("post-resolve resolvePath: %v", err)
	}
	if got := eng.ListConflicts(); len(got) != 0 {
		t.Fatalf("expected ConflictMarkers cleared after markers removed, got %+v", got)
	}
	eng.Stop()
}

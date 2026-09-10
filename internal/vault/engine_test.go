package vault

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
)

type fakeDrive struct {
	identity         string
	files            map[string]pikpak.File
	outputs          []RemoteEntry
	shareEntries     []Entry
	calls            map[string]int
	next             int
	failList         string
	failPage         string
	lostOffline      bool
	lostMkdir        bool
	lostMove         bool
	lostRename       bool
	shareOutside     bool
	shareResponseIDs bool
	instantHit       bool
	tasks            []pikpak.Task
	tombstones       bool
	onWrite          func()
}

func newFake(identity string) *fakeDrive {
	return &fakeDrive{identity: identity, files: map[string]pikpak.File{}, calls: map[string]int{}}
}
func (f *fakeDrive) newID() string { f.next++; return fmt.Sprintf("%s-%d", f.identity, f.next) }
func (f *fakeDrive) write() {
	if f.onWrite != nil {
		f.onWrite()
	}
}
func (f *fakeDrive) Me(context.Context) (pikpak.Identity, error) {
	return pikpak.Identity{Sub: f.identity}, nil
}
func (f *fakeDrive) Quota(context.Context) (pikpak.Quota, error) {
	return pikpak.Quota{Limit: 1 << 40}, nil
}
func (f *fakeDrive) List(_ context.Context, parent, page string) (pikpak.Page, error) {
	f.calls["list"]++
	if parent == f.failPage && f.failPage != "" {
		if page == "" {
			return pikpak.Page{Next: "page-2"}, nil
		}
		return pikpak.Page{}, &pikpak.APIError{Status: 503, Code: "page_interrupted"}
	}
	if parent == f.failList && f.failList != "" {
		return pikpak.Page{}, &pikpak.APIError{Status: 503, Code: "temporarily_unavailable"}
	}
	files := []pikpak.File{}
	for _, v := range f.files {
		if v.ParentID == parent && !v.Trashed {
			files = append(files, v)
		}
	}
	return pikpak.Page{Files: files}, nil
}
func (f *fakeDrive) Get(_ context.Context, id string) (pikpak.File, error) {
	f.calls["get"]++
	v, ok := f.files[id]
	if !ok {
		return v, &pikpak.APIError{Status: 404, Code: "file_not_found"}
	}
	if f.tombstones && v.Trashed {
		return pikpak.File{ID: id, Trashed: true}, nil
	}
	return v, nil
}
func (f *fakeDrive) Mkdir(_ context.Context, parent, name string) (pikpak.File, error) {
	f.write()
	f.calls["mkdir"]++
	v := pikpak.File{ID: f.newID(), ParentID: parent, Name: name, Kind: "drive#folder", Phase: "PHASE_TYPE_COMPLETE"}
	f.files[v.ID] = v
	if f.lostMkdir {
		f.lostMkdir = false
		return pikpak.File{}, &pikpak.APIError{Code: "network_error"}
	}
	return v, nil
}
func (f *fakeDrive) Move(_ context.Context, id, parent string) error {
	f.write()
	f.calls["move"]++
	v, ok := f.files[id]
	if !ok {
		return &pikpak.APIError{Status: 404}
	}
	v.ParentID = parent
	f.files[id] = v
	if f.lostMove {
		f.lostMove = false
		return &pikpak.APIError{Code: "network_error"}
	}
	return nil
}
func (f *fakeDrive) Rename(_ context.Context, id, name string) error {
	f.write()
	f.calls["rename"]++
	v := f.files[id]
	v.Name = name
	f.files[id] = v
	if f.lostRename {
		f.lostRename = false
		return &pikpak.APIError{Code: "network_error"}
	}
	return nil
}
func (f *fakeDrive) Trash(_ context.Context, ids []string) error {
	f.write()
	f.calls["trash"]++
	for _, id := range ids {
		v := f.files[id]
		v.Trashed = true
		f.files[id] = v
	}
	return nil
}
func (f *fakeDrive) Untrash(_ context.Context, ids []string) error {
	f.write()
	f.calls["untrash"]++
	for _, id := range ids {
		v, ok := f.files[id]
		if !ok {
			return &pikpak.APIError{Status: 404}
		}
		v.Trashed = false
		v.ParentID = ""
		v.Name += "(1)"
		f.files[id] = v
	}
	return nil
}
func (f *fakeDrive) putOutputs(parent string) []pikpak.File {
	out := []pikpak.File{}
	parents := map[string]string{".": parent}
	for _, r := range f.outputs {
		v := r.File
		v.ID = f.newID()
		v.ParentID = parents[path.Dir(r.Path)]
		v.Name = path.Base(r.Path)
		v.Phase = "PHASE_TYPE_COMPLETE"
		f.files[v.ID] = v
		parents[r.Path] = v.ID
		if path.Dir(r.Path) == "." {
			out = append(out, v)
		}
	}
	return out
}
func (f *fakeDrive) Offline(_ context.Context, link, parent string) (pikpak.Transfer, error) {
	f.write()
	f.calls["offline"]++
	files := f.putOutputs(parent)
	task := pikpak.Task{ID: f.newID(), Phase: "PHASE_TYPE_COMPLETE", Progress: 100}
	task.Params.URL = link
	if len(files) > 0 {
		task.FileID = files[0].ID
	}
	f.tasks = append(f.tasks, task)
	if f.lostOffline {
		f.lostOffline = false
		return pikpak.Transfer{}, &pikpak.APIError{Code: "network_error"}
	}
	return pikpak.Transfer{Files: files}, nil
}
func (f *fakeDrive) Tasks(context.Context) ([]pikpak.Task, error) {
	f.calls["tasks"]++
	return f.tasks, nil
}
func (f *fakeDrive) Share(_ context.Context, id, pass, token, parent, page string) (pikpak.Share, error) {
	f.calls["share"]++
	if id == "expired" {
		return pikpak.Share{}, &pikpak.APIError{Status: 404, Code: "share_expired"}
	}
	files := []pikpak.File{}
	for _, entry := range f.shareEntries {
		entryParent := ""
		if dir := path.Dir(entry.Path); dir != "." {
			for _, p := range f.shareEntries {
				if p.Path == dir {
					entryParent = p.ID
				}
			}
		}
		if entryParent != parent {
			continue
		}
		kind := "drive#file"
		if entry.Kind == "folder" {
			kind = "drive#folder"
		}
		files = append(files, pikpak.File{ID: entry.ID, Name: entry.Name, Kind: kind, Size: pikpak.Number(entry.Size), Hash: entry.Hash, Phase: "PHASE_TYPE_COMPLETE"})
	}
	return pikpak.Share{Page: pikpak.Page{Files: files}, Status: "OK", PassCodeToken: "pass-token"}, nil
}
func (f *fakeDrive) RestoreShare(_ context.Context, id, token string, ids []string, parent string) (pikpak.Transfer, error) {
	f.write()
	f.calls["share_restore"]++
	for _, id := range ids {
		found := false
		for _, entry := range f.shareEntries {
			if entry.ID == id {
				found = true
			}
		}
		if !found {
			return pikpak.Transfer{}, &pikpak.APIError{Status: 400, Code: "invalid_share_file_id"}
		}
	}
	if f.shareOutside {
		parent = ""
	}
	files := f.putOutputs(parent)
	if f.shareResponseIDs || !f.shareOutside {
		return pikpak.Transfer{Files: files}, nil
	}
	return pikpak.Transfer{}, nil
}

func TestSharePublisherIDsSurviveImportAndRecovery(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	for i, r := range f.outputs {
		kind := "file"
		if r.File.Folder() {
			kind = "folder"
		}
		f.shareEntries = append(f.shareEntries, Entry{ID: fmt.Sprintf("publisher-%d", i), Path: r.Path, Name: path.Base(r.Path), Kind: kind, Size: int64(r.File.Size), Hash: r.File.Hash})
	}
	j := createImport(t, a, "share")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	source, e := a.Store.Source(nodes[0].SourceID)
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range source.Manifest {
		if !strings.HasPrefix(entry.ID, "publisher-") {
			t.Fatalf("publisher manifest overwritten: %+v", entry)
		}
	}
	f.files = map[string]pikpak.File{}
	r, _ := a.Store.NewJob("a", "recover", "Recover share", JobData{})
	requireComplete(t, execute(t, a, &r))
	if f.calls["share_restore"] != 2 {
		t.Fatal("wrong number of share submissions")
	}
}
func (f *fakeDrive) Instant(_ context.Context, parent string, file pikpak.File) (pikpak.Transfer, error) {
	f.write()
	f.calls["instant"]++
	if !f.instantHit {
		return pikpak.Transfer{}, &pikpak.APIError{Status: 400, Code: "hash_not_found"}
	}
	file.ID = f.newID()
	file.ParentID = parent
	file.Kind = "drive#file"
	file.Phase = "PHASE_TYPE_COMPLETE"
	f.files[file.ID] = file
	return pikpak.Transfer{File: &file}, nil
}
func testApp(t *testing.T) (*App, *fakeDrive) {
	t.Helper()
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.DB.Close() })
	a := NewApp(s)
	f := newFake("user-a")
	addAccount(t, a, "a", f.identity)
	if e = s.Set("active_account", "a"); e != nil {
		t.Fatal(e)
	}
	a.Factory = func(ac Account) (pikpak.Provider, error) { return f, nil }
	return a, f
}
func addAccount(t *testing.T, a *App, id, identity string) {
	t.Helper()
	secret, e := a.Store.Seal(pikpak.Credentials{})
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.Store.DB.Exec(`INSERT INTO accounts(id,name,identity,secret,status,created) VALUES(?,?,?,?,?,?)`, id, id, identity, secret, "ready", now())
	if e != nil {
		t.Fatal(e)
	}
}
func createImport(t *testing.T, a *App, kind string) Job {
	t.Helper()
	link := "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)
	if kind == "share" {
		link = "https://mypikpak.com/s/test-share"
	}
	s, e := ParseSource(link, "")
	if e != nil {
		t.Fatal(e)
	}
	s.Secret, e = a.Store.Seal("")
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Store.SaveSource(s); e != nil {
		t.Fatal(e)
	}
	j, e := a.Store.NewJob(a.active(), "import", "Test import", JobData{SourceID: s.ID, ParentID: "root"})
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func execute(t *testing.T, a *App, j *Job) Job {
	t.Helper()
	a.Execute(context.Background(), j)
	result, e := a.Store.Job(j.ID)
	if e != nil {
		t.Fatal(e)
	}
	*j = result
	return result
}
func requireComplete(t *testing.T, j Job) {
	t.Helper()
	if j.State != "completed" {
		t.Fatalf("job %s: state=%s message=%s data=%s", j.Kind, j.State, j.Message, j.Data)
	}
}
func sampleOutputs() []RemoteEntry {
	return []RemoteEntry{{pikpak.File{Kind: "drive#folder"}, "Collection"}, {pikpak.File{Kind: "drive#file", Size: 100, Hash: "hash-one", MimeType: "video/mp4"}, "Collection/one.mp4"}, {pikpak.File{Kind: "drive#folder"}, "Collection/Nested"}, {pikpak.File{Kind: "drive#file", Size: 200, Hash: "hash-two", MimeType: "text/plain"}, "Collection/Nested/two.txt"}}
}
func TestImportDeletionAndCrossAccountRecovery(t *testing.T) {
	a, first := testApp(t)
	first.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, e := a.Store.AllNodes("a")
	if e != nil || len(nodes) != 4 {
		t.Fatalf("nodes=%d err=%v", len(nodes), e)
	}
	var one Node
	for _, n := range nodes {
		if n.Name == "one.mp4" {
			one = n
		}
	}
	_, e = a.Store.DB.Exec(`UPDATE nodes SET name='Renamed movie.mp4',favorite=1,position=18 WHERE id=?`, one.ID)
	if e != nil {
		t.Fatal(e)
	}
	delete(first.files, one.RemoteID)
	scan, _ := a.Store.NewJob("a", "scan", "Scan", JobData{})
	requireComplete(t, execute(t, a, &scan))
	missing, _ := a.Store.Node(one.ID, "a")
	if missing.State != "missing" {
		t.Fatal(missing.State)
	}
	recovery, _ := a.Store.NewJob("a", "recover", "Recover", JobData{NodeIDs: []string{one.ID}})
	requireComplete(t, execute(t, a, &recovery))
	saved, _ := a.Store.Node(one.ID, "a")
	if saved.State != "present" || saved.Name != "Renamed movie.mp4" || !saved.Favorite || saved.Position != 18 {
		t.Fatalf("lost desired state: %+v", saved)
	}
	second := newFake("user-b")
	second.outputs = sampleOutputs()
	addAccount(t, a, "b", second.identity)
	_ = a.Store.Set("active_account", "b")
	a.Factory = func(ac Account) (pikpak.Provider, error) {
		if ac.ID == "a" {
			return first, nil
		}
		return second, nil
	}
	recovery, _ = a.Store.NewJob("b", "recover", "Cross account", JobData{})
	requireComplete(t, execute(t, a, &recovery))
	if second.calls["offline"] != 1 {
		t.Fatalf("multi-file source imported %d times", second.calls["offline"])
	}
	all, _ := a.Store.AllNodes("b")
	for _, n := range all {
		if n.State != "present" {
			t.Fatalf("not recovered %+v", n)
		}
		p, e := a.Store.Path(n.ID)
		if e != nil {
			t.Fatal(e)
		}
		f := second.files[n.RemoteID]
		if f.Name != n.Name {
			t.Fatalf("wrong recovered name %s / %s", p, f.Name)
		}
	}
}
func TestRootDeletionRebuildAndTrashSkip(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	var trashID string
	for _, n := range nodes {
		if n.Name == "two.txt" {
			trashID = n.ID
		}
	}
	_, _ = a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id=?`, trashID)
	f.files = map[string]pikpak.File{}
	r, _ := a.Store.NewJob("a", "recover", "Restore all", JobData{})
	requireComplete(t, execute(t, a, &r))
	nodes, _ = a.Store.AllNodes("a")
	for _, n := range nodes {
		if n.ID != trashID && n.State != "present" {
			t.Fatalf("missing %s: %s", n.Name, n.State)
		}
	}
	trashed, _ := a.Store.Node(trashID, "a")
	if !trashed.Trashed {
		t.Fatal("manual trash was reversed")
	}
}
func TestAccountVerificationDoesNotRestoreDeletedRoot(t *testing.T) {
	a, f := testApp(t)
	id, err := a.ensureRoot(context.Background(), f, "a")
	if err != nil {
		t.Fatal(err)
	}
	root := f.files[id]
	root.Trashed = true
	f.files[id] = root
	verify, _ := a.Store.NewJob("a", "verify", "Verify", JobData{})
	requireComplete(t, execute(t, a, &verify))
	prepare, _ := a.Store.NewJob("a", "root", "Switch", JobData{})
	requireComplete(t, execute(t, a, &prepare))
	if !f.files[id].Trashed || f.calls["untrash"] != 0 {
		t.Fatal("account verification restored data without a recovery request")
	}
}
func TestLostResponseAndRestartDoNotDuplicateTransfer(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	f.lostOffline = true
	j := createImport(t, a, "magnet")
	result := execute(t, a, &j)
	if result.State != "retry" {
		t.Fatal(result.State, result.Message)
	}
	dir := a.Store.Dir
	a.Store.DB.Close()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	a.Store = s
	t.Cleanup(func() { s.DB.Close() })
	requireComplete(t, execute(t, a, &j))
	if f.calls["offline"] != 1 {
		t.Fatalf("duplicated write after lost response: %d", f.calls["offline"])
	}
}
func TestIncompleteScanNeverMarksMissing(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	for _, n := range nodes {
		if n.Kind == "file" {
			delete(f.files, n.RemoteID)
		} else if n.Name == "Collection" {
			f.failList = n.RemoteID
		}
	}
	scan, _ := a.Store.NewJob("a", "scan", "Scan", JobData{})
	result := execute(t, a, &scan)
	if result.State != "retry" {
		t.Fatal(result.State)
	}
	after, _ := a.Store.AllNodes("a")
	for _, n := range after {
		if n.State != "unknown" {
			t.Fatalf("incomplete scan marked %s as %s", n.Name, n.State)
		}
	}
}

func TestInterruptedSecondPageKeepsMissingStateUnknown(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	ac, err := a.Store.Account("a")
	if err != nil {
		t.Fatal(err)
	}
	f.failPage = ac.RootID
	nodes, _ := a.Store.AllNodes("a")
	for _, n := range nodes {
		if n.Kind == "file" {
			delete(f.files, n.RemoteID)
		}
	}
	scan, _ := a.Store.NewJob("a", "scan", "Scan", JobData{})
	result := execute(t, a, &scan)
	if result.State != "retry" {
		t.Fatal(result.State, result.Message)
	}
	nodes, _ = a.Store.AllNodes("a")
	for _, n := range nodes {
		if n.State != "unknown" {
			t.Fatalf("partial page marked %s as %s", n.Name, n.State)
		}
	}
}

func TestExpiredSourceRetainsPathsAndReplacementRecovers(t *testing.T) {
	a, f := testApp(t)
	f.outputs = []RemoteEntry{{pikpak.File{Kind: "drive#file", Size: 23, Hash: "content"}, "hello.txt"}}
	f.shareEntries = []Entry{{ID: "publisher-1", Path: "hello.txt", Name: "hello.txt", Kind: "file", Size: 23, Hash: "content"}}
	j := createImport(t, a, "share")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	n := nodes[0]
	delete(f.files, n.RemoteID)
	if _, err := a.Store.DB.Exec(`UPDATE nodes SET name='renamed.txt',favorite=1 WHERE id=?`, n.ID); err != nil {
		t.Fatal(err)
	}
	source, _ := a.Store.Source(n.SourceID)
	source.ShareID = "expired"
	if err := a.Store.SaveSource(source); err != nil {
		t.Fatal(err)
	}
	recovery, _ := a.Store.NewJob("a", "recover", "Recover expired source", JobData{NodeIDs: []string{n.ID}})
	result := execute(t, a, &recovery)
	if result.State != "partial" || !strings.Contains(string(result.Data), "share_expired") {
		t.Fatalf("missing source error: %+v", result)
	}
	kept, err := a.Store.Node(n.ID, "a")
	if err != nil || kept.Name != "renamed.txt" || !kept.Favorite || kept.SourcePath != "hello.txt" {
		t.Fatalf("metadata lost: %+v %v", kept, err)
	}
	sessionForTest(t, a)
	r := request(t, a.Handler(nil), "PUT", "/api/v1/sources/"+source.ID, map[string]string{"link": "https://mypikpak.com/s/replacement", "pass_code": "new-code"}, "test-csrf")
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	f.shareEntries[0].ID = "replacement-publisher-2"
	recovery, _ = a.Store.NewJob("a", "recover", "Retry with new source", JobData{NodeIDs: []string{n.ID}})
	requireComplete(t, execute(t, a, &recovery))
	kept, _ = a.Store.Node(n.ID, "a")
	if kept.State != "present" || kept.Name != "renamed.txt" || !kept.Favorite {
		t.Fatalf("incorrect recovery: %+v", kept)
	}
}

func TestSwitchDuringRecoveryKeepsResultsBoundToOriginalAccount(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	addAccount(t, a, "b", "user-b")
	nodes, _ := a.Store.AllNodes("a")
	for _, n := range nodes {
		if n.Kind == "file" {
			delete(f.files, n.RemoteID)
		}
	}
	f.onWrite = func() { _ = a.Store.Set("active_account", "b") }
	recovery, _ := a.Store.NewJob("a", "recover", "Recover", JobData{})
	result := execute(t, a, &recovery)
	if result.State != "paused" {
		t.Fatal(result.State, result.Message)
	}
	var count int
	if err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM bindings WHERE account_id='b'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("old account result was associated with the new account")
	}
	f.onWrite = nil
	_ = a.Store.Set("active_account", "a")
	recovery.State = "queued"
	if err := a.Store.SaveJob(&recovery, nil); err != nil {
		t.Fatal(err)
	}
	requireComplete(t, execute(t, a, &recovery))
}
func TestUntrashRepositionsAndRenames(t *testing.T) {
	a, f := testApp(t)
	f.outputs = []RemoteEntry{{pikpak.File{Kind: "drive#file", Size: 23, Hash: "content"}, "hello.txt"}}
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	n := nodes[0]
	remote := f.files[n.RemoteID]
	remote.Trashed = true
	f.files[n.RemoteID] = remote
	r, _ := a.Store.NewJob("a", "recover", "Restore", JobData{NodeIDs: []string{n.ID}})
	requireComplete(t, execute(t, a, &r))
	restored := f.files[n.RemoteID]
	ac, _ := a.Store.Account("a")
	if restored.Name != n.Name || restored.ParentID != ac.RootID || restored.Trashed {
		t.Fatalf("untrash did not align %+v", restored)
	}
	if f.calls["offline"] != 1 {
		t.Fatal("source replayed unnecessarily")
	}
}
func TestSameNameConflictNeverOverwrites(t *testing.T) {
	a, f := testApp(t)
	f.outputs = []RemoteEntry{{pikpak.File{Kind: "drive#file", Size: 23, Hash: "one"}, "hello.txt"}}
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	n := nodes[0]
	delete(f.files, n.RemoteID)
	ac, _ := a.Store.Account("a")
	conflict := pikpak.File{ID: "untracked", ParentID: ac.RootID, Name: n.Name, Kind: "drive#file", Size: 900, Hash: "other", Phase: "PHASE_TYPE_COMPLETE"}
	f.files[conflict.ID] = conflict
	r, _ := a.Store.NewJob("a", "recover", "Restore", JobData{NodeIDs: []string{n.ID}})
	result := execute(t, a, &r)
	if result.State != "partial" {
		t.Fatal(result.State, result.Message)
	}
	if got := f.files[conflict.ID]; got.Hash != "other" || got.Size != 900 || got.Trashed {
		t.Fatal("overwrote unrelated item")
	}
}
func TestShareOutsideDestinationStopsWithoutMoving(t *testing.T) {
	for _, identified := range []bool{false, true} {
		t.Run(fmt.Sprint(identified), func(t *testing.T) {
			a, f := testApp(t)
			f.outputs = []RemoteEntry{{pikpak.File{Kind: "drive#file", Size: 23, Hash: "one"}, "shared.txt"}}
			f.shareEntries = []Entry{{ID: "publisher-1", Path: "shared.txt", Name: "shared.txt", Kind: "file", Size: 23, Hash: "one"}}
			f.shareOutside = true
			f.shareResponseIDs = identified
			j := createImport(t, a, "share")
			result := execute(t, a, &j)
			if result.State == "completed" || f.calls["share_restore"] != 1 || f.calls["move"] != 0 {
				t.Fatal("wrong destination must stop without a fallback move", result.State, f.calls)
			}
		})
	}
}
func TestLostFolderCreationResponseUsesMarker(t *testing.T) {
	a, f := testApp(t)
	_, e := a.ensureRoot(context.Background(), f, "a")
	if e != nil {
		t.Fatal(e)
	}
	n := Node{ID: ID(), ParentID: "root", Name: "Folder", Kind: "folder", Created: now(), Modified: now()}
	if e = a.Store.InsertNode(n); e != nil {
		t.Fatal(e)
	}
	initialCreates := f.calls["mkdir"]
	f.lostMkdir = true
	_, e = a.folder(context.Background(), f, "a", n.ID, map[string]bool{})
	if e == nil {
		t.Fatal("expected timeout")
	}
	id, e := a.folder(context.Background(), f, "a", n.ID, map[string]bool{})
	if e != nil {
		t.Fatal(e)
	}
	if f.files[id].Name != "Folder" || f.calls["mkdir"] != initialCreates+1 {
		t.Fatalf("duplicated folder: %#v", f.calls)
	}
}
func TestSwitchPausesJobWithoutLosingLocalSource(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	addAccount(t, a, "b", "user-b")
	f.onWrite = func() {
		if f.calls["offline"] == 0 && len(f.files) > 1 {
			_ = a.Store.Set("active_account", "b")
		}
	}
	result := execute(t, a, &j)
	if result.State != "paused" {
		t.Fatalf("expected paused, got %s %s", result.State, result.Message)
	}
	var sourceCount int
	_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&sourceCount)
	if sourceCount != 1 {
		t.Fatal("source lost on switch")
	}
}

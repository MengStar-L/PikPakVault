package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
)

type transferListing struct {
	Files []Node `json:"files"`
	Total int    `json:"total"`
}

func readTransferListing(t *testing.T, a *App, query string) transferListing {
	t.Helper()
	w := httptest.NewRecorder()
	if err := a.filesList(w, httptest.NewRequest("GET", "/api/v1/files?"+query, nil)); err != nil {
		t.Fatal(err)
	}
	var v transferListing
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestImportFilesStayScopedAndSurviveRestart(t *testing.T) {
	a, f := testApp(t)
	sessionForTest(t, a)
	if err := a.Store.InsertNode(Node{ID: "target", ParentID: "root", Name: "Target", Kind: "folder"}); err != nil {
		t.Fatal(err)
	}
	w := request(t, a.Handler(nil), "POST", "/api/v1/imports", map[string]any{"items": []importInput{{Link: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + "&dn=My+movie.mp4", ParentID: "target"}}}, "test-csrf")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Jobs []Job `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	j, err := a.Store.Job(result.Jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	list := readTransferListing(t, a, "parent=target")
	if list.Total != 1 || list.Files[0].Name != "My movie.mp4" || list.Files[0].Transfer.State != "queued" {
		t.Fatalf("missing immediate item: %+v", list)
	}
	id := list.Files[0].ID
	if _, err := a.Store.Node(id, "a"); err == nil {
		t.Fatal("unverified source became a real node")
	}
	for _, query := range []string{"parent=root", "parent=target&transfers=0", "view=trash", "view=missing", "view=favorites", "view=folders"} {
		for _, n := range readTransferListing(t, a, query).Files {
			if n.ID == id {
				t.Fatalf("placeholder leaked into %s", query)
			}
		}
	}
	if got := readTransferListing(t, a, "search=movie&view=video"); got.Total != 1 {
		t.Fatal("global search/type lost pending file")
	}
	if len(f.calls) != 0 {
		t.Fatal("listing made upstream requests", f.calls)
	}
	// Reopening the database must retain status and stable display IDs.
	s, err := Open(a.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	if got := readTransferListing(t, NewApp(s), "parent=target"); got.Files[0].ID != id {
		t.Fatal("pending item did not survive restart")
	}
	addAccount(t, a, "b", "user-b")
	if err = a.Store.Set("active_account", "b"); err != nil {
		t.Fatal(err)
	}
	if got := readTransferListing(t, a, "parent=target"); got.Total != 0 {
		t.Fatal("other account's import appeared")
	}
	a.Store.Set("active_account", "a")
	for _, state := range []string{"waiting", "paused", "failed", "attention", "retry", "cancelled"} {
		j.State = state
		j.Progress = 42
		j.Message = "specific failure"
		if err = a.Store.SaveJob(&j, nil); err != nil {
			t.Fatal(err)
		}
		got := readTransferListing(t, a, "parent=target")
		if state == "cancelled" {
			if got.Total != 0 {
				t.Fatal("cancelled placeholder remained")
			}
			continue
		}
		if got.Total != 1 || got.Files[0].Transfer.State != state || got.Files[0].Transfer.Progress != 42 || got.Files[0].Transfer.Message != j.Message {
			t.Fatal("status lost", state, got)
		}
	}
}

func TestSharePreviewIsDisplayOnlyAndRegistrationDoesNotDuplicate(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	preview := []Entry{{ID: "chosen", Name: "Selected", Path: "Selected", Kind: "folder", Size: 10}, {ID: "unselected", Name: "Wrong.txt", Path: "Wrong.txt", Kind: "file"}}
	w := request(t, a.Handler(nil), "POST", "/api/v1/imports", map[string]any{"items": []importInput{{Link: "https://mypikpak.com/s/demo", Selected: []string{"chosen"}, Preview: preview}}}, "test-csrf")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	jobs, err := a.Store.Jobs()
	if err != nil {
		t.Fatal(err)
	}
	j := jobs[0]
	d := loadJobData(t, j)
	s, err := a.Store.Source(d.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Manifest) != 0 || len(d.Preview) != 1 {
		t.Fatal("UI hint trusted as manifest or selection ignored")
	}
	if got := readTransferListing(t, a, ""); got.Total != 1 || got.Files[0].Name != "Selected" {
		t.Fatal(got)
	}
	d.Transfers[d.SourceID] = &TransferState{Expected: []Entry{{Name: "Real folder", Path: "Real folder", Kind: "folder"}, {Name: "child.txt", Path: "Real folder/child.txt", Kind: "file"}, {Name: "movie.mp4", Path: "movie.mp4", Kind: "file"}}}
	a.Store.SaveJob(&j, &d)
	if got := readTransferListing(t, a, ""); got.Total != 2 || got.Files[0].Name != "Real folder" {
		t.Fatal("manifest must replace hints and list top-level results", got)
	}
	n := Node{ID: stableNode(s.ID, "Real folder"), ParentID: "root", Name: "Renamed folder", Kind: "folder", SourceID: s.ID, SourcePath: "Real folder"}
	if err = a.Store.InsertNode(n); err != nil {
		t.Fatal(err)
	}
	got := readTransferListing(t, a, "")
	if got.Total != 2 {
		t.Fatal("partially registered output duplicated", got)
	}
	for _, file := range got.Files {
		if file.Name == "Real folder" || file.Transfer == nil {
			t.Fatal("wrong name or lost registration status", file)
		}
	}
	a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id=?`, n.ID)
	if got := readTransferListing(t, a, ""); got.Total != 1 {
		t.Fatal("trashed result reappeared", got)
	}
	j.State = "completed"
	a.Store.SaveJob(&j, nil)
	if got := readTransferListing(t, a, ""); got.Total != 0 {
		t.Fatal("completed placeholder remained", got)
	}
}

func TestTransferPaginationAndRealCompletion(t *testing.T) {
	a, f := testApp(t)
	if _, err := a.ensureRoot(context.Background(), f, "a"); err != nil {
		t.Fatal(err)
	}
	f.calls = map[string]int{}
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	d := loadJobData(t, j)
	for i := 0; i < 103; i++ {
		d.Preview = append(d.Preview, Entry{Name: fmt.Sprintf("Pending %03d", i), Path: fmt.Sprint(i), Kind: "file"})
	}
	a.Store.SaveJob(&j, &d)
	for i := 0; i < 7; i++ {
		if err := a.Store.InsertNode(Node{ID: fmt.Sprint(i), ParentID: "root", Name: fmt.Sprint(i), Kind: "file"}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for page := 0; page < 6; page++ {
		got := readTransferListing(t, a, fmt.Sprintf("page=%d&limit=20&sort=name&direction=desc", page))
		if got.Total != 110 || len(got.Files) != min(20, 110-page*20) {
			t.Fatal("pagination counts wrong", page, got.Total, len(got.Files))
		}
		for _, n := range got.Files {
			if seen[n.ID] {
				t.Fatal("duplicate across pages")
			}
			seen[n.ID] = true
		}
	}
	if len(seen) != 110 {
		t.Fatal("page boundary lost items")
	}
	// A real mock import should replace the unknown/preview entries with verified
	// output while keeping ordinary files and avoiding any staging operations.
	requireComplete(t, execute(t, a, &j))
	got := readTransferListing(t, a, "")
	if got.Total != 8 {
		t.Fatal("placeholder not replaced by final folder", got.Total)
	}
	for _, n := range got.Files {
		if n.Transfer != nil {
			t.Fatal("completed transfer still pending")
		}
	}
	if f.calls["offline"] != 1 || f.calls["move"] != 0 || f.calls["rename"] != 0 {
		t.Fatal("extra upstream operations", f.calls)
	}
}

func TestIncompleteMagnetDisplaysKnownOutputWithoutPrematureRegistration(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	c := &alteredDrive{fakeDrive: f, afterOffline: func(result *pikpak.Transfer) {
		for id, file := range f.files {
			if !file.Folder() {
				file.Phase = "PHASE_TYPE_RUNNING"
				f.files[id] = file
			}
		}
	}}
	a.Factory = func(Account) (pikpak.Provider, error) { return c, nil }
	j := createImport(t, a, "magnet")
	execute(t, a, &j)
	if j.State != "waiting" {
		t.Fatal(j.State, j.Message)
	}
	got := readTransferListing(t, a, "")
	if got.Total != 1 || got.Files[0].Kind != "folder" || got.Files[0].Name != "Collection" || got.Files[0].Transfer.State != "waiting" {
		t.Fatal("actual pending output metadata missing", got)
	}
	nodes, _ := a.Store.AllNodes("a")
	if len(nodes) != 0 {
		t.Fatal("incomplete output entered recovery database")
	}
}

package vault

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
)

func TestFolderPreviewsUseOnlyAvailableLocalVideosAndPersistSetting(t *testing.T) {
	a, f := testApp(t)
	sessionForTest(t, a)
	add := func(id, parent, kind, mime, state string, trashed bool) {
		t.Helper()
		if err := a.Store.InsertNode(Node{ID: id, ParentID: parent, Name: id, Kind: kind, Mime: mime, Trashed: trashed}); err != nil {
			t.Fatal(err)
		}
		if trashed {
			if _, err := a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}
		if state != "" {
			if err := a.Store.Bind("a", id, "remote-"+id, state, id, "", " "); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("folder", "root", "folder", "", "present", false)
	add("nested", "folder", "folder", "", "present", false)
	add("unavailable", "folder", "folder", "", "missing", false)
	add("nested-video", "nested", "file", "video/mp4", "present", false)
	add("behind-missing", "unavailable", "file", "video/mp4", "present", false)
	add("a-video", "folder", "file", "video/mp4", "present", false)
	add("b-video", "folder", "file", "video/mp4", "present", false)
	add("c-image", "folder", "file", "image/jpeg", "present", false)
	add("d-deleted", "folder", "file", "video/mp4", "present", true)
	add("e-missing", "folder", "file", "video/mp4", "missing", false)
	add("f-unknown", "folder", "file", "video/mp4", "unknown", false)
	add("g-unbound", "folder", "file", "video/mp4", "", false)
	list := readTransferListing(t, a, "")
	if len(list.Files[0].FolderPreviews) != 0 {
		t.Fatal("previews must default to off")
	}
	h := a.Handler(nil)
	w := request(t, h, "PATCH", "/api/v1/settings", map[string]any{"folder_previews": true}, "test-csrf")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	list = readTransferListing(t, a, "")
	previews := list.Files[0].FolderPreviews
	if len(previews) != 3 {
		t.Fatal("wrong eligible previews", previews)
	}
	for i, id := range []string{"a-video", "b-video", "nested-video"} {
		if previews[i].ID != id {
			t.Fatal("wrong ordering", previews)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("listing contacted PikPak", f.calls)
	}
	if !strings.Contains(previews[0].URL, "account=a") || !strings.Contains(previews[0].URL, "folder=folder") {
		t.Fatal("unscoped preview link")
	}
	if got := readTransferListing(t, a, "transfers=0"); len(got.Files[0].FolderPreviews) != 0 {
		t.Fatal("picker requested covers")
	}
	w = request(t, h, "PATCH", "/api/v1/settings", map[string]any{"scan_minutes": 30}, "test-csrf")
	if w.Code != 200 || a.Store.Get("folder_previews") != "true" {
		t.Fatal("unrelated settings reset preview preference")
	}
	s, err := Open(a.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	if s.Get("folder_previews") != "true" {
		t.Fatal("preference did not survive reopen")
	}
	addAccount(t, a, "b", "user-b")
	a.Store.Set("active_account", "b")
	if got := readTransferListing(t, a, ""); len(got.Files[0].FolderPreviews) != 0 {
		t.Fatal("previews leaked across accounts")
	}
	a.Store.Set("active_account", "a")
	a.Store.DB.Exec(`UPDATE nodes SET parent_id='root' WHERE id='a-video'`)
	if a.previewAncestor(Node{ParentID: "root"}, "folder", "a") {
		t.Fatal("moved video still belongs to folder")
	}
	a.Store.Set("folder_previews", "false")
	for _, n := range readTransferListing(t, a, "").Files {
		if len(n.FolderPreviews) > 0 {
			t.Fatal("disabled previews remained")
		}
	}
}

func TestFolderThumbnailRefreshesOnceAndChecksScope(t *testing.T) {
	a, f := testApp(t)
	sessionForTest(t, a)
	f.outputs = []RemoteEntry{{File: pikpak.File{Kind: "drive#folder"}, Path: "Collection"}, {File: pikpak.File{Kind: "drive#file", Size: 10, Hash: "one", MimeType: "video/mp4", Thumbnail: "https://media.example.test/fresh"}, Path: "Collection/one.mp4"}}
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	a.Store.Set("folder_previews", "true")
	list := readTransferListing(t, a, "")
	preview := list.Files[0].FolderPreviews[0]
	f.calls = map[string]int{}
	paths := []string{}
	a.MediaHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		status := 200
		if r.URL.Path == "/expired" {
			status = 403
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("image bytes")), Request: r}, nil
	})}
	h := a.Handler(nil)
	for i := 0; i < 2; i++ {
		w := request(t, h, "GET", preview.URL, nil, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "private, max-age=300" {
			t.Fatal("missing private image cache")
		}
	}
	if f.calls["get"] != 0 {
		t.Fatal("cached link caused metadata requests")
	}
	a.Store.DB.Exec(`UPDATE bindings SET thumbnail='https://media.example.test/expired' WHERE node_id=?`, preview.ID)
	w := request(t, h, "GET", preview.URL, nil, "")
	if w.Code != 200 || f.calls["get"] != 1 || paths[len(paths)-2] != "/expired" || paths[len(paths)-1] != "/fresh" {
		t.Fatal("expired URL not refreshed exactly once", w.Code, f.calls, paths)
	}
	a.Store.DB.Exec(`UPDATE bindings SET thumbnail='' WHERE node_id=?`, preview.ID)
	w = request(t, h, "GET", preview.URL, nil, "")
	if w.Code != 200 || f.calls["get"] != 2 {
		t.Fatal("legacy empty metadata not loaded")
	}
	before := len(paths)
	for _, suffix := range []string{"&account=other", "&remote=wrong"} {
		// Replace the existing query value rather than add an ambiguous duplicate.
		url := preview.URL
		if strings.Contains(suffix, "account") {
			url = strings.Replace(url, "account=a", "account=other", 1)
		} else {
			url = strings.Replace(url, "remote=", "remote=wrong", 1)
		}
		if w := request(t, h, "GET", url, nil, ""); w.Code < 400 {
			t.Fatal("stale account/binding allowed")
		}
	}
	a.Store.Set("folder_previews", "false")
	if w := request(t, h, "GET", preview.URL, nil, ""); w.Code != 404 {
		t.Fatal("disabled preview endpoint remained enabled")
	}
	a.Store.Set("folder_previews", "true")
	a.Store.DB.Exec(`UPDATE nodes SET parent_id='root' WHERE id=?`, preview.ID)
	if w := request(t, h, "GET", preview.URL, nil, ""); w.Code != 404 {
		t.Fatal("moved video accepted")
	}
	if len(paths) != before {
		t.Fatal("invalid preview requests contacted CDN")
	}
}

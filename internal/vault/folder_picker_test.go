package vault

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestFolderPickerPaginationStaysWithinParent(t *testing.T) {
	a, upstream := testApp(t)
	sessionForTest(t, a)
	for _, n := range []Node{
		{ID: "picker-parent", ParentID: "root", Name: "Parent", Kind: "folder"},
		{ID: "picker-other", ParentID: "root", Name: "Other", Kind: "folder"},
		{ID: "picker-grandchild", ParentID: "picker-folder-000", Name: "Nested", Kind: "folder"},
		{ID: "picker-trashed", ParentID: "picker-parent", Name: "Trashed", Kind: "folder"},
	} {
		if err := a.Store.InsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id='picker-trashed'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 103; i++ {
		for _, kind := range []string{"file", "folder"} {
			if err := a.Store.InsertNode(Node{
				ID: fmt.Sprintf("picker-%s-%03d", kind, i), ParentID: "picker-parent",
				Name: fmt.Sprintf("%s %03d", kind, i), Kind: kind,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	type listing struct {
		Files       []Node              `json:"files"`
		Total       int                 `json:"total"`
		Page        int                 `json:"page"`
		Breadcrumbs []map[string]string `json:"breadcrumbs"`
	}
	handler := a.Handler(nil)
	read := func(query string) listing {
		t.Helper()
		w := request(t, handler, "GET", "/api/v1/files?"+query, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("listing failed: %d %s", w.Code, w.Body.String())
		}
		var result listing
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	seen := map[string]bool{}
	for page, wantCount := range []int{100, 3, 0} {
		got := read(fmt.Sprintf("parent=picker-parent&transfers=0&kind=folder&limit=100&page=%d", page))
		if got.Total != 103 || got.Page != page || len(got.Files) != wantCount {
			t.Fatalf("folder page %d: total=%d page=%d count=%d", page, got.Total, got.Page, len(got.Files))
		}
		if len(got.Breadcrumbs) != 1 || got.Breadcrumbs[0]["id"] != "picker-parent" {
			t.Fatalf("folder filtering changed breadcrumbs: %+v", got.Breadcrumbs)
		}
		for _, n := range got.Files {
			if n.Kind != "folder" || n.ParentID != "picker-parent" || n.Trashed || seen[n.ID] {
				t.Fatalf("wrong or duplicate picker result: %+v", n)
			}
			seen[n.ID] = true
		}
	}
	if len(seen) != 103 {
		t.Fatalf("folders lost between pages: %d", len(seen))
	}
	if got := read("parent=picker-parent&transfers=0&limit=500"); got.Total != 206 || len(got.Files) != 206 {
		t.Fatalf("unfiltered listing changed: total=%d count=%d", got.Total, len(got.Files))
	}
	// The sidebar's existing folders view remains global, even when a parent is supplied.
	if got := read("view=folders&parent=picker-parent&transfers=0&limit=500"); got.Total != 106 || len(got.Files) != 106 {
		t.Fatalf("global folders view changed: total=%d count=%d", got.Total, len(got.Files))
	}
	if got := read("parent=picker-folder-000&transfers=0&kind=folder"); got.Total != 1 || got.Files[0].ID != "picker-grandchild" {
		t.Fatalf("nested directory inaccessible: %+v", got)
	}
	if len(upstream.calls) != 0 {
		t.Fatalf("local folder navigation called upstream: %+v", upstream.calls)
	}
}

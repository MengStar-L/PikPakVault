package vault

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestFolderCreateRejectsDeletedAncestorAndAllowsRestoredPath(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	h := a.Handler(nil)
	for _, n := range []Node{
		{ID: "parent", ParentID: "root", Name: "Parent", Kind: "folder"},
		{ID: "child", ParentID: "parent", Name: "Child", Kind: "folder"},
	} {
		if e := a.Store.InsertNode(n); e != nil {
			t.Fatal(e)
		}
	}
	// A picker can retain the child ID while another view deletes its tree.
	if w := request(t, h, "POST", "/api/v1/files/action", map[string]any{"ids": []string{"parent"}, "action": "trash"}, "test-csrf"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Restoring an individual descendant does not restore its deleted ancestor.
	if w := request(t, h, "POST", "/api/v1/files/action", map[string]any{"ids": []string{"child"}, "action": "restore"}, "test-csrf"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	child, e := a.Store.Node("child", "a")
	if e != nil || child.Trashed {
		t.Fatal("fixture must retain an individually active child", e)
	}
	var jobsBefore int
	if e = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsBefore); e != nil {
		t.Fatal(e)
	}
	payload := map[string]string{"name": "New folder", "parent_id": "child"}
	w := request(t, h, "POST", "/api/v1/files", payload, "test-csrf")
	if w.Code != http.StatusConflict {
		t.Fatalf("created folder inside deleted ancestry: %d %s", w.Code, w.Body.String())
	}
	var added, jobsAfter int
	if e = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent_id='child'`).Scan(&added); e != nil || added != 0 {
		t.Fatal("rejected creation left a local child", added, e)
	}
	if e = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsAfter); e != nil || jobsAfter != jobsBefore {
		t.Fatal("rejected creation queued remote work", jobsAfter, jobsBefore, e)
	}
	if w = request(t, h, "POST", "/api/v1/files/action", map[string]any{"ids": []string{"parent"}, "action": "restore"}, "test-csrf"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(t, h, "POST", "/api/v1/files", payload, "test-csrf")
	if w.Code != http.StatusCreated {
		t.Fatalf("restored path cannot create a folder: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID  string `json:"id"`
		Job Job    `json:"job"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &created); e != nil {
		t.Fatal(e)
	}
	n, e := a.Store.Node(created.ID, "a")
	if e != nil || n.ParentID != "child" || n.Name != payload["name"] || n.Kind != "folder" || n.Trashed || created.Job.Kind != "sync" {
		t.Fatalf("creation did not return a usable local directory: %+v %+v %v", n, created.Job, e)
	}
	w = request(t, h, "POST", "/api/v1/files", payload, "test-csrf")
	if w.Code != http.StatusConflict {
		t.Fatal("same-name folder was duplicated", w.Code)
	}
}

func TestFolderCreateRejectsInvalidParentWithoutCreatingRecords(t *testing.T) {
	for _, variant := range []struct {
		name   string
		parent Node
		status int
	}{
		{"file", Node{ID: "target", ParentID: "root", Name: "File.txt", Kind: "file"}, http.StatusBadRequest},
		{"missing", Node{}, http.StatusNotFound},
		{"deleted", Node{ID: "target", ParentID: "root", Name: "Deleted", Kind: "folder"}, http.StatusConflict},
		{"orphaned", Node{ID: "target", ParentID: "missing-ancestor", Name: "Orphan", Kind: "folder"}, http.StatusConflict},
	} {
		t.Run(variant.name, func(t *testing.T) {
			a, _ := testApp(t)
			sessionForTest(t, a)
			if variant.parent.ID != "" {
				if e := a.Store.InsertNode(variant.parent); e != nil {
					t.Fatal(e)
				}
			}
			if variant.name == "deleted" {
				if _, e := a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id='target'`); e != nil {
					t.Fatal(e)
				}
			}
			w := request(t, a.Handler(nil), "POST", "/api/v1/files", map[string]string{"name": "Rejected child", "parent_id": "target"}, "test-csrf")
			if w.Code != variant.status {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
			}
			var children, jobs int
			if e := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent_id='target'`).Scan(&children); e != nil || children != 0 {
				t.Fatal("invalid target acquired a child", children, e)
			}
			if e := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs); e != nil || jobs != 0 {
				t.Fatal("invalid target queued a remote task", jobs, e)
			}
		})
	}
}

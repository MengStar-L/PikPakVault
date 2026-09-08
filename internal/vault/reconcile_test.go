package vault

import (
	"context"
	"testing"
)

func TestAdoptRemotePathsPreservesSourceAndPersonalMetadata(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	var file, collection Node
	for _, n := range nodes {
		if n.Name == "one.mp4" {
			file = n
		}
		if n.Name == "Collection" {
			collection = n
		}
	}
	a.Store.DB.Exec(`UPDATE nodes SET favorite=1,position=98 WHERE id=?`, file.ID)
	ac, _ := a.Store.Account("a")
	parent, _ := f.Mkdir(context.Background(), ac.RootID, "自己整理的目录")
	f.Move(context.Background(), collection.RemoteID, parent.ID)
	f.Rename(context.Background(), file.RemoteID, "renamed.mp4")
	f.Mkdir(context.Background(), ac.RootID, "无关的目录")
	f.calls = map[string]int{}
	preview, err := a.ReconcilePaths(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Applied || len(preview.Changes) != 4 || preview.AddedParents != 1 {
		t.Fatal(preview)
	}
	unchanged, _ := a.Store.Node(file.ID, "a")
	if unchanged.Name != file.Name {
		t.Fatal("preview mutated local path")
	}
	report, err := a.ReconcilePaths(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Applied {
		t.Fatal(report)
	}
	got, _ := a.Store.Node(file.ID, "a")
	p, _ := a.Store.Path(got.ID)
	if p != "/自己整理的目录/Collection/renamed.mp4" || got.SourceID != file.SourceID || got.SourcePath != file.SourcePath || !got.Favorite || got.Position != 98 {
		t.Fatalf("lost metadata: %s %+v", p, got)
	}
	all, _ := a.Store.AllNodes("a")
	if len(all) != 5 {
		t.Fatal("adopted unrelated folders")
	}
	for _, method := range []string{"mkdir", "move", "rename", "trash", "untrash", "offline", "instant"} {
		if f.calls[method] != 0 {
			t.Fatal("maintenance mutated upstream", method)
		}
	}
	again, err := a.ReconcilePaths(context.Background(), true)
	if err != nil || len(again.Changes) != 0 || again.AddedParents != 0 {
		t.Fatal(again, err)
	}
}
func TestAdoptPathsAbortsOnIncompleteListingAndLocalConflicts(t *testing.T) {
	for _, scenario := range []string{"page-error", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			a, f := testApp(t)
			f.outputs = sampleOutputs()
			j := createImport(t, a, "magnet")
			requireComplete(t, execute(t, a, &j))
			nodes, _ := a.Store.AllNodes("a")
			var file Node
			for _, n := range nodes {
				if n.Name == "one.mp4" {
					file = n
				}
			}
			f.Rename(context.Background(), file.RemoteID, "new.mp4")
			ac, _ := a.Store.Account("a")
			if scenario == "page-error" {
				f.failPage = ac.RootID
			} else {
				a.Store.InsertNode(Node{ID: ID(), ParentID: file.ParentID, Name: "new.mp4", Kind: "file", Created: now(), Modified: now()})
			}
			_, err := a.ReconcilePaths(context.Background(), true)
			if err == nil {
				t.Fatal("expected refusal")
			}
			got, _ := a.Store.Node(file.ID, "a")
			if got.Name != file.Name {
				t.Fatal("partial path commit")
			}
		})
	}
}

package vault

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"pikpakvault/internal/pikpak"
)

func TestPlaybackHistoryRequiresExplicitAuthenticatedPlayback(t *testing.T) {
	a, f := testApp(t)
	sessionForTest(t, a)
	f.outputs = []RemoteEntry{{pikpak.File{Kind: "drive#file", Size: 10, Hash: "movie", MimeType: "video/mp4", WebContentLink: "https://media.example.test/file"}, "movie.mp4"}}
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, err := a.Store.AllNodes("a")
	if err != nil || len(nodes) != 1 {
		t.Fatalf("files=%+v error=%v", nodes, err)
	}
	id := nodes[0].ID
	h := a.Handler(nil)
	path := "/api/v1/files/" + id
	preview := request(t, h, "GET", path+"/media", nil, "")
	if preview.Code != 200 {
		t.Fatal(preview.Body.String())
	}
	n, err := a.Store.Node(id, "a")
	if err != nil || n.Opened == 0 || n.PlayedAt != 0 {
		t.Fatalf("preview marked file played: %+v %v", n, err)
	}
	unauthenticated := httptest.NewRecorder()
	h.ServeHTTP(unauthenticated, httptest.NewRequest("POST", path+"/played", nil))
	if unauthenticated.Code != 401 {
		t.Fatalf("unauthenticated playback accepted: %d", unauthenticated.Code)
	}
	if w := request(t, h, "POST", path+"/played", nil, ""); w.Code != 403 {
		t.Fatalf("missing CSRF accepted: %d", w.Code)
	}
	for _, badID := range []string{"missing", "root"} {
		if w := request(t, h, "POST", "/api/v1/files/"+badID+"/played", nil, "test-csrf"); w.Code != 404 {
			t.Fatalf("invalid playback %s: %d %s", badID, w.Code, w.Body.String())
		}
	}
	before := now()
	w := request(t, h, "POST", path+"/played", nil, "test-csrf")
	var played struct {
		PlayedAt int64 `json:"played_at"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &played); err != nil || w.Code != 200 || played.PlayedAt < before || played.PlayedAt > now() {
		t.Fatalf("playback response: %d %s %v", w.Code, w.Body.String(), err)
	}
	var detail struct {
		File Node `json:"file"`
	}
	w = request(t, h, "GET", path, nil, "")
	if err = json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 || detail.File.PlayedAt != played.PlayedAt {
		t.Fatalf("detail lost playback: %d %s %v", w.Code, w.Body.String(), err)
	}
	var listing struct {
		Files []Node `json:"files"`
	}
	w = request(t, h, "GET", "/api/v1/files?parent_id=root", nil, "")
	if err = json.Unmarshal(w.Body.Bytes(), &listing); err != nil || w.Code != 200 || len(listing.Files) != 1 || listing.Files[0].PlayedAt != played.PlayedAt {
		t.Fatalf("listing lost playback: %d %s %v", w.Code, w.Body.String(), err)
	}
	// Playback belongs to the durable local identity, including after local
	// organization changes, a restart, or a switch to an unbound account.
	if err = a.Store.InsertNode(Node{ID: "destination", ParentID: "root", Name: "Destination", Kind: "folder"}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []map[string]any{
		{"ids": []string{id}, "action": "rename", "name": "renamed.mp4"},
		{"ids": []string{id}, "action": "move", "parent_id": "destination"},
	} {
		if w = request(t, h, "POST", "/api/v1/files/action", action, "test-csrf"); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	addAccount(t, a, "b", "user-b")
	if err = a.Store.Set("active_account", "b"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(a.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	n, err = reopened.Node(id, "b")
	if err != nil || n.PlayedAt != played.PlayedAt || n.Name != "renamed.mp4" || n.ParentID != "destination" || n.RemoteID != "" {
		t.Fatalf("identity lost playback: %+v %v", n, err)
	}
	if _, err = a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if w = request(t, h, "POST", path+"/played", nil, "test-csrf"); w.Code != 404 {
		t.Fatalf("trashed file accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestLegacyPlaybackHistoryMigrationAndImport(t *testing.T) {
	for _, mode := range []string{"open", "backup-import"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := testApp(t)
			for _, id := range []string{"played", "preview", "no-timestamp"} {
				if err := a.Store.InsertNode(Node{ID: id, ParentID: "root", Name: id + ".mp4", Kind: "file"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.Store.DB.Exec(`UPDATE nodes SET position=12.5,opened=123456 WHERE id='played'; UPDATE nodes SET opened=234567 WHERE id='preview'; UPDATE nodes SET position=2 WHERE id='no-timestamp'; ALTER TABLE nodes DROP COLUMN played_at; PRAGMA user_version=3`); err != nil {
				t.Fatal(err)
			}
			var migrated *Store
			if mode == "open" {
				if err := a.Store.DB.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				migrated, err = Open(a.Store.Dir)
				if err != nil {
					t.Fatal(err)
				}
				defer migrated.DB.Close()
			} else {
				archive := filepath.Join(t.TempDir(), "legacy.zip")
				if err := a.Store.Backup(archive); err != nil {
					t.Fatal(err)
				}
				src, err := ExtractBackup(archive, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer src.DB.Close()
				dst, _ := testApp(t)
				if err = replaceData(dst.Store, src); err != nil {
					t.Fatal(err)
				}
				migrated = dst.Store
			}
			for id, want := range map[string]int64{"played": 123456, "preview": 0, "no-timestamp": 0, "root": 0} {
				n, err := migrated.Node(id, "a")
				if err != nil || n.PlayedAt != want {
					t.Fatalf("migrated %s played_at=%d want=%d error=%v", id, n.PlayedAt, want, err)
				}
			}
			// Later previews must not rewrite the historical playback timestamp.
			if _, err := migrated.DB.Exec(`UPDATE nodes SET opened=999999 WHERE id='played'`); err != nil {
				t.Fatal(err)
			}
			again, err := Open(migrated.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer again.DB.Close()
			n, err := again.Node("played", "a")
			if err != nil || n.PlayedAt != 123456 || n.Position != 12.5 {
				t.Fatalf("repeat open changed playback: %+v %v", n, err)
			}
		})
	}
}

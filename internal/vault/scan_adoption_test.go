package vault

import (
	"context"
	"encoding/json"
	"testing"

	"pikpakvault/internal/pikpak"
)

func transferredAccount(t *testing.T) (*App, *fakeDrive, string) {
	t.Helper()
	a, first := testApp(t)
	first.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	second := newFake("user-b")
	second.outputs = sampleOutputs()
	pack, _ := second.Mkdir(context.Background(), "", "My Pack")
	root, _ := second.Mkdir(context.Background(), pack.ID, "PikPakVault")
	second.putOutputs(root.ID)
	addAccount(t, a, "b", second.identity)
	a.Factory = func(ac Account) (pikpak.Provider, error) {
		if ac.ID == "b" {
			return second, nil
		}
		return first, nil
	}
	second.calls = map[string]int{}
	return a, second, root.ID
}

func activateTransferredAccount(t *testing.T, a *App) Job {
	t.Helper()
	sessionForTest(t, a)
	r := request(t, a.Handler(nil), "POST", "/api/v1/accounts/b/activate", map[string]any{}, "test-csrf")
	if r.Code != 202 {
		t.Fatalf("activate: %d %s", r.Code, r.Body.String())
	}
	var result struct {
		Job Job `json:"job"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	requireComplete(t, execute(t, a, &result.Job))
	scan, err := jobScan(a.Store.DB.QueryRow(`SELECT ` + jobCols + ` FROM jobs WHERE account_id='b' AND kind='scan'`))
	if err != nil {
		t.Fatal(err)
	}
	return scan
}

func TestAccountSwitchAdoptsTransferredRootAndScansEveryFile(t *testing.T) {
	a, second, root := transferredAccount(t)
	before, _ := a.Store.AllNodes("a")
	if _, err := a.Store.DB.Exec(`UPDATE nodes SET favorite=1,position=18,played_at=123456 WHERE kind='file'`); err != nil {
		t.Fatal(err)
	}
	before, _ = a.Store.AllNodes("a")
	// Extra cloud content must be scanned but never imported or changed.
	extra, _ := second.Mkdir(context.Background(), root, "自己保存的内容")
	second.calls = map[string]int{}
	remoteBefore := jsonText(second.files)
	scan := activateTransferredAccount(t, a)
	ac, _ := a.Store.Account("b")
	if ac.RootID != root || ac.Status != "ready" || a.Store.Get("root_scan_pending:b") != root {
		t.Fatalf("existing root was not accepted: %+v", ac)
	}
	// A recovery queued before the scan cannot start restoring duplicate files.
	recovery, _ := a.Store.NewJob("b", "recover", "Recover", JobData{})
	for _, selected := range a.runnableJobs(map[string]bool{}) {
		if selected.ID == recovery.ID {
			t.Fatal("recovery started before the full scan")
		}
	}
	requireComplete(t, execute(t, a, &scan))
	if a.Store.Get("root_scan_pending:b") != "" {
		t.Fatal("successful scan did not unblock ordinary work")
	}
	nodes, _ := a.Store.AllNodes("b")
	if len(nodes) != len(before) {
		t.Fatal("scan imported unrelated files")
	}
	for _, n := range nodes {
		if n.State != "present" || n.RemoteID == "" || n.RemoteID == extra.ID {
			t.Fatalf("transferred node not matched: %+v", n)
		}
		if n.Kind == "file" && (!n.Favorite || n.Position != 18 || n.PlayedAt != 123456) {
			t.Fatal("scan lost personal metadata")
		}
	}
	requireComplete(t, execute(t, a, &recovery))
	after, _ := a.Store.AllNodes("a")
	if jsonText(before) != jsonText(after) || jsonText(second.files) != remoteBefore {
		t.Fatal("adoption changed saved records or cloud files")
	}
	for _, method := range []string{"mkdir", "move", "rename", "trash", "untrash", "offline", "instant"} {
		if second.calls[method] != 0 {
			t.Fatal("already transferred files were restored again", method, second.calls)
		}
	}
}

func TestTransferredScanDistinguishesMissingConflictingAndIncompleteFiles(t *testing.T) {
	for _, scenario := range []string{"missing", "size", "hash", "no-hash", "pending", "duplicate-file", "duplicate-folder", "wrong-path"} {
		t.Run(scenario, func(t *testing.T) {
			a, second, root := transferredAccount(t)
			want := "conflict"
			for id, f := range second.files {
				if f.Name != "two.txt" {
					continue
				}
				switch scenario {
				case "missing":
					delete(second.files, id)
					want = "missing"
				case "size":
					f.Size++
				case "hash":
					f.Hash = "different"
				case "no-hash":
					f.Hash, want = "", "unknown"
				case "pending":
					f.Phase, want = "PHASE_TYPE_RUNNING", "pending"
				case "duplicate-file":
					duplicate := f
					duplicate.ID = "duplicate"
					second.files[duplicate.ID] = duplicate
				case "duplicate-folder":
					parent := second.files[f.ParentID]
					_, _ = second.Mkdir(context.Background(), parent.ParentID, parent.Name)
				case "wrong-path":
					f.ParentID, want = root, "missing"
				}
				if scenario != "missing" {
					second.files[id] = f
				}
				break
			}
			scan := activateTransferredAccount(t, a)
			requireComplete(t, execute(t, a, &scan))
			nodes, _ := a.Store.AllNodes("b")
			for _, n := range nodes {
				if n.Name == "one.mp4" && n.State != "present" || n.Name == "two.txt" && (n.State != want || n.RemoteID != "") {
					t.Fatalf("incorrect file classification: %+v, want %s", n, want)
				}
			}
			if a.Store.Get("root_scan_pending:b") != "" {
				t.Fatal("individual file problem disabled the entire account")
			}
		})
	}
}

func TestTransferredScanFailureAndRestartNeverPublishPartialBindings(t *testing.T) {
	a, second, root := transferredAccount(t)
	scan := activateTransferredAccount(t, a)
	for id, f := range second.files {
		if f.Name == "Nested" {
			second.failPage = id
		}
	}
	if result := execute(t, a, &scan); result.State != "retry" {
		t.Fatalf("incomplete scan: %s %s", result.State, result.Message)
	}
	nodes, _ := a.Store.AllNodes("b")
	for _, n := range nodes {
		if n.RemoteID != "" || n.State == "missing" {
			t.Fatalf("incomplete scan published results: %+v", n)
		}
	}
	if a.Store.Get("root_scan_pending:b") != root {
		t.Fatal("incomplete scan unblocked recovery")
	}
	if _, err := a.ensureRoot(context.Background(), second, "b"); err == nil {
		t.Fatal("a writer bypassed the incomplete scan")
	}
	// A fresh app must continue the persisted scan without adopting twice.
	restarted := NewApp(a.Store)
	restarted.Factory = a.Factory
	second.failPage = ""
	requireComplete(t, execute(t, restarted, &scan))
	id, err := restarted.ensureRoot(context.Background(), second, "b")
	if err != nil || id != root {
		t.Fatalf("restart did not unblock adopted root: %s %v", id, err)
	}
	var count int
	if err = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE account_id='b' AND kind='scan'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("scan duplicated after restart: %d %v", count, err)
	}
}

func TestTransferredRootAdoptionAndScanQueueAreAtomic(t *testing.T) {
	a, second, _ := transferredAccount(t)
	_ = a.Store.Set("active_account", "b")
	if _, err := a.Store.DB.Exec(`CREATE TRIGGER reject_adoption_scan BEFORE INSERT ON jobs WHEN NEW.kind='scan' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.prepareRoot(context.Background(), second, "b"); err == nil {
		t.Fatal("accepted root without a durable full scan")
	}
	ac, _ := a.Store.Account("b")
	if ac.RootID != "" || a.Store.Get("root_path:b") != "" || a.Store.Get("root_scan_pending:b") != "" {
		t.Fatal("partially committed root adoption")
	}
}

func TestLegacyOccupiedRootTaskResumesWithoutManualIntervention(t *testing.T) {
	a, second, root := transferredAccount(t)
	_ = a.Store.Set("active_account", "b")
	j, _ := a.Store.NewJob("b", "root", "调整专用目录", JobData{RootPath: a.rootPath()})
	j.State, j.Message = "attention", "目标位置已有同名项目，未合并或覆盖。请更换专用目录路径："+a.rootPath()
	if err := a.Store.SaveJob(&j, nil); err != nil {
		t.Fatal(err)
	}
	a.scheduleRoot()
	a.scheduleRoot()
	requireComplete(t, execute(t, a, &j))
	ac, _ := a.Store.Account("b")
	if ac.RootID != root || second.calls["mkdir"] != 0 {
		t.Fatal("old conflict was not recovered safely")
	}
}

func TestTransferredScanRestoresOnlyMissingFile(t *testing.T) {
	a, second, _ := transferredAccount(t)
	for id, f := range second.files {
		if f.Name == "two.txt" {
			delete(second.files, id)
		}
	}
	scan := activateTransferredAccount(t, a)
	requireComplete(t, execute(t, a, &scan))
	nodes, _ := a.Store.AllNodes("b")
	var missing Node
	existing := map[string]string{}
	for _, n := range nodes {
		if n.Name == "two.txt" {
			missing = n
		} else {
			existing[n.ID] = n.RemoteID
		}
	}
	if missing.State != "missing" {
		t.Fatalf("missing file not detected: %+v", missing)
	}
	recovery, _ := a.Store.NewJob("b", "recover", "Restore missing file", JobData{NodeIDs: []string{missing.ID}, ExactNodes: true})
	requireComplete(t, execute(t, a, &recovery))
	nodes, _ = a.Store.AllNodes("b")
	for _, n := range nodes {
		if n.State != "present" || n.ID != missing.ID && n.RemoteID != existing[n.ID] {
			t.Fatalf("recovery changed an existing transferred file: %+v", n)
		}
	}
	if second.calls["offline"] != 1 {
		t.Fatal("missing file did not use exactly one source restore", second.calls)
	}
}

func TestTransferredScanRejectsConcurrentChanges(t *testing.T) {
	for _, scenario := range []string{"local-rename", "remote-change", "account-switch", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			a, second, root := transferredAccount(t)
			scan := activateTransferredAccount(t, a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			changed := false
			c := &scanHookDrive{fakeDrive: second, onList: func(_ string, page pikpak.Page) (pikpak.Page, error) {
				if changed {
					return page, nil
				}
				changed = true
				switch scenario {
				case "local-rename":
					_, err := a.Store.DB.Exec(`UPDATE nodes SET name='renamed',revision=revision+1 WHERE name='Collection'`)
					return page, err
				case "account-switch":
					return page, a.Store.Set("active_account", "a")
				case "cancel":
					cancel()
				case "remote-change":
					// List returned the old folder name; the final Get sees the edit.
					for id, f := range second.files {
						if f.Name == "Collection" {
							f.Name = "renamed"
							second.files[id] = f
						}
					}
				}
				return page, nil
			}}
			if err := a.checkLibrary(ctx, c, &scan); err == nil {
				t.Fatal("scan accepted concurrent change")
			}
			nodes, _ := a.Store.AllNodes("b")
			for _, n := range nodes {
				if n.RemoteID != "" || n.State == "missing" {
					t.Fatalf("changed scan committed partial results: %+v", n)
				}
			}
			if a.Store.Get("root_scan_pending:b") != root {
				t.Fatal("changed scan cleared pending validation")
			}
		})
	}
}

func TestUnboundRootRejectsAmbiguousOrNonFolderTarget(t *testing.T) {
	for _, scenario := range []string{"duplicate", "file"} {
		t.Run(scenario, func(t *testing.T) {
			a, second, root := transferredAccount(t)
			f := second.files[root]
			if scenario == "duplicate" {
				_, _ = second.Mkdir(context.Background(), f.ParentID, f.Name)
			} else {
				f.Kind = "drive#file"
				second.files[root] = f
			}
			before := jsonText(second.files)
			if err := a.prepareRoot(context.Background(), second, "b"); err == nil {
				t.Fatal("ambiguous root accepted")
			}
			ac, _ := a.Store.Account("b")
			if ac.RootID != "" || before != jsonText(second.files) {
				t.Fatal("root conflict mutated data")
			}
		})
	}
}

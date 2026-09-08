package vault

import (
	"context"
	"encoding/json"
	"testing"

	"pikpakvault/internal/pikpak"
)

func TestDefaultRootReusesMyPackWithoutTouchingOtherFiles(t *testing.T) {
	a, f := testApp(t)
	pack, _ := f.Mkdir(context.Background(), "", "My Pack")
	f.files["other"] = pikpak.File{ID: "other", ParentID: pack.ID, Name: "existing.txt", Kind: "drive#file", Size: 7}
	id, err := a.ensureRoot(context.Background(), f, "a")
	if err != nil {
		t.Fatal(err)
	}
	if f.files[id].Name != "PikPakVault" || f.files[id].ParentID != pack.ID {
		t.Fatalf("incorrect default root: %+v", f.files[id])
	}
	if f.files["other"].ParentID != pack.ID || len(f.files) != 3 {
		t.Fatal("modified unrelated account data")
	}
	if a.Store.Get("root_path:a") != DefaultRootPath {
		t.Fatal("applied path not persisted")
	}
}

func TestLegacyRootMigratesWithStableBindings(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	ac, _ := a.Store.Account("a")
	before, _ := a.Store.AllNodes("a")
	root := f.files[ac.RootID]
	root.Name = "PikPakVault-old-instance"
	root.ParentID = ""
	f.files[root.ID] = root
	if err := a.Store.Set("root_path:a", ""); err != nil {
		t.Fatal(err)
	}
	a.scheduleRoot()
	a.scheduleRoot()
	var count int
	if err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='root'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration jobs=%d err=%v", count, err)
	}
	migration, err := jobScan(a.Store.DB.QueryRow(`SELECT ` + jobCols + ` FROM jobs WHERE kind='root'`))
	if err != nil {
		t.Fatal(err)
	}
	requireComplete(t, execute(t, a, &migration))
	after, _ := a.Store.AllNodes("a")
	updated, _ := a.Store.Account("a")
	if updated.RootID != ac.RootID || jsonText(before) != jsonText(after) {
		t.Fatal("migration changed local records or bindings")
	}
	root = f.files[ac.RootID]
	if root.Name != "PikPakVault" || f.files[root.ParentID].Name != "My Pack" || f.calls["offline"] != 1 {
		t.Fatalf("incorrect legacy migration: %+v", root)
	}
}

func TestCustomRootMigrationReconcilesLostResponse(t *testing.T) {
	for _, phase := range []string{"move", "rename"} {
		t.Run(phase, func(t *testing.T) {
			a, f := testApp(t)
			id, err := a.ensureRoot(context.Background(), f, "a")
			if err != nil {
				t.Fatal(err)
			}
			if err = a.Store.Set("root_path", "My Pack/资料/收藏"); err != nil {
				t.Fatal(err)
			}
			f.lostMove = phase == "move"
			f.lostRename = phase == "rename"
			j, _ := a.Store.NewJob("a", "root", "Move root", JobData{RootPath: a.rootPath()})
			if result := execute(t, a, &j); result.State != "retry" {
				t.Fatal(result.State, result.Message)
			}
			requireComplete(t, execute(t, a, &j))
			ac, _ := a.Store.Account("a")
			root := f.files[id]
			if ac.RootID != id || root.Name != "收藏" || f.files[root.ParentID].Name != "资料" {
				t.Fatalf("wrong resumed root: %+v", root)
			}
			if f.calls["move"] != 1 {
				t.Fatal("blindly repeated move after lost response")
			}
		})
	}
}

func TestRootCreationResponseLossDoesNotDuplicateDirectory(t *testing.T) {
	a, f := testApp(t)
	_, _ = f.Mkdir(context.Background(), "", "My Pack")
	f.lostMkdir = true
	if _, err := a.ensureRoot(context.Background(), f, "a"); err == nil {
		t.Fatal("expected lost response")
	}
	id, err := a.ensureRoot(context.Background(), f, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.files) != 2 || f.files[id].Name != "PikPakVault" {
		t.Fatal("root duplicated after response loss")
	}
}

func TestRootPathRejectsOccupiedTargetAndOwnDescendant(t *testing.T) {
	for _, scenario := range []string{"occupied", "descendant", "duplicate-parent"} {
		t.Run(scenario, func(t *testing.T) {
			a, f := testApp(t)
			id, err := a.ensureRoot(context.Background(), f, "a")
			if err != nil {
				t.Fatal(err)
			}
			original := f.files[id]
			if scenario == "occupied" {
				_, _ = f.Mkdir(context.Background(), original.ParentID, "Other")
				_ = a.Store.Set("root_path", "My Pack/Other")
			}
			if scenario == "descendant" {
				_ = a.Store.Set("root_path", "My Pack/PikPakVault/Child")
			}
			if scenario == "duplicate-parent" {
				_, _ = f.Mkdir(context.Background(), "", "My Pack")
				_ = a.Store.Set("root_path", "My Pack/Other")
			}
			before := jsonText(f.files)
			if err = a.prepareRoot(context.Background(), f, "a"); err == nil {
				t.Fatal("unsafe root path accepted")
			}
			if jsonText(f.files) != before {
				t.Fatal("conflict changed remote data")
			}
		})
	}
}

func TestRootPathChangeDoesNotRestoreDeletedContent(t *testing.T) {
	a, f := testApp(t)
	id, err := a.ensureRoot(context.Background(), f, "a")
	if err != nil {
		t.Fatal(err)
	}
	root := f.files[id]
	root.Trashed = true
	f.files[id] = root
	_ = a.Store.Set("root_path", "My Pack/新的位置")
	if err = a.prepareRoot(context.Background(), f, "a"); err != nil {
		t.Fatal(err)
	}
	if !f.files[id].Trashed || f.calls["untrash"] != 0 {
		t.Fatal("settings change restored deleted content")
	}
	got, err := a.ensureRoot(context.Background(), f, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got != id || f.files[id].Name != "新的位置" || f.files[f.files[id].ParentID].Name != "My Pack" {
		t.Fatal("explicit restore used wrong path")
	}
}

func TestRootSettingsAreValidatedAndQueuedAtomically(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	h := a.Handler(nil)
	for _, invalid := range []string{"", "/", "My Pack/../Other", "My Pack//Other", "My Pack/./Other", "My Pack/\tOther"} {
		r := request(t, h, "PATCH", "/api/v1/settings", map[string]any{"root_path": invalid, "scan_minutes": 30}, "test-csrf")
		if r.Code != 400 {
			t.Fatalf("invalid path %q accepted: %s", invalid, r.Body.String())
		}
	}
	_, err := a.Store.DB.Exec(`CREATE TRIGGER reject_root_job BEFORE INSERT ON jobs WHEN NEW.kind='root' BEGIN SELECT RAISE(ABORT,'injected'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"root_path": `/My Pack\资料/收藏/`, "scan_minutes": 30}
	r := request(t, h, "PATCH", "/api/v1/settings", body, "test-csrf")
	if r.Code != 500 {
		t.Fatal(r.Code, r.Body.String())
	}
	if a.rootPath() != DefaultRootPath {
		t.Fatal("path saved without durable migration job")
	}
	_, _ = a.Store.DB.Exec(`DROP TRIGGER reject_root_job`)
	r = request(t, h, "PATCH", "/api/v1/settings", body, "test-csrf")
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	var result struct {
		Job Job `json:"job"`
	}
	if err = json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if a.rootPath() != "My Pack/资料/收藏" || result.Job.Kind != "root" || result.Job.AccountID != "a" {
		t.Fatal("path not queued for active account")
	}
	r = request(t, h, "PATCH", "/api/v1/settings", map[string]any{"scan_minutes": 60}, "test-csrf")
	if r.Code != 200 || a.rootPath() != "My Pack/资料/收藏" {
		t.Fatal("omitted path reset custom setting")
	}
}

func TestCustomPathAppliesAfterAccountSwitchAndRootDeletion(t *testing.T) {
	a, first := testApp(t)
	first.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	_ = a.Store.Set("root_path", "My Pack/普通资料/存档")
	second := newFake("user-b")
	second.outputs = sampleOutputs()
	addAccount(t, a, "b", "user-b")
	a.Factory = func(ac Account) (pikpak.Provider, error) {
		if ac.ID == "b" {
			return second, nil
		}
		return first, nil
	}
	_ = a.Store.Set("active_account", "b")
	if err := a.prepareRoot(context.Background(), second, "b"); err != nil {
		t.Fatal(err)
	}
	var checkPath = func() {
		ac, _ := a.Store.Account("b")
		root := second.files[ac.RootID]
		parent := second.files[root.ParentID]
		if root.Name != "存档" || parent.Name != "普通资料" || second.files[parent.ParentID].Name != "My Pack" {
			t.Fatal("wrong account path")
		}
	}
	checkPath()
	second.files = map[string]pikpak.File{}
	recovery, _ := a.Store.NewJob("b", "recover", "Recover", JobData{})
	requireComplete(t, execute(t, a, &recovery))
	checkPath()
	if a.Store.Get("root_path:a") != DefaultRootPath {
		t.Fatal("inactive account was changed")
	}
}

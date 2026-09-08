package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"pikpakvault/internal/pikpak"
)

func recoveryPreviewForTest(t *testing.T, a *App, ids []string) struct {
	Items   []Node `json:"items"`
	Bytes   int64  `json:"bytes"`
	Skipped int    `json:"skipped_pending"`
} {
	t.Helper()
	var out struct {
		Items   []Node `json:"items"`
		Bytes   int64  `json:"bytes"`
		Skipped int    `json:"skipped_pending"`
	}
	w := request(t, a.Handler(nil), "POST", "/api/v1/recovery/preview", recoveryInput{IDs: ids}, "test-csrf")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func recoveryCountForTest(t *testing.T, a *App) int {
	t.Helper()
	w := httptest.NewRecorder()
	if err := a.summary(w, httptest.NewRequest("GET", "/api/v1/summary", nil)); err != nil {
		t.Fatal(err)
	}
	var v struct {
		Missing int `json:"missing"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.Missing
}

func TestTelDrivePendingNodesDoNotEnterRecovery(t *testing.T) {
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	if scan := runTDScan(t, a, m); scan.State != "completed" {
		t.Fatal(scan)
	}
	j := tdUploadJob(t, a, "a")
	id := stableNode(m.ID, "movie")
	// Missing results must be filtered before pagination/counting, not merely
	// stripped from a response after the SQL limit has already been applied.
	for i := 0; i < 3; i++ {
		if err := a.Store.InsertNode(Node{ID: fmt.Sprint("missing-", i), ParentID: "root", Name: fmt.Sprint("z", i), Kind: "file"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []string{"queued", "running", "waiting", "retry", "paused", "failed", "attention", "partial"} {
		t.Run(state, func(t *testing.T) {
			j.State, j.Progress, j.Message = state, 15, "读取来源时连接中断"
			if err := a.Store.SaveJob(&j, nil); err != nil {
				t.Fatal(err)
			}
			list := readTransferListing(t, a, "search=movie")
			if list.Total != 1 || list.Files[0].ID != id || list.Files[0].State != "transferring" || list.Files[0].Transfer == nil || list.Files[0].Transfer.State != state {
				t.Fatalf("lost transfer: %+v", list)
			}
			for page := 0; page < 3; page++ {
				list = readTransferListing(t, a, fmt.Sprintf("view=missing&limit=1&page=%d", page))
				if list.Total != 3 || len(list.Files) != 1 || !strings.HasPrefix(list.Files[0].ID, "missing-") {
					t.Fatalf("pending node consumed recovery page: %+v", list)
				}
			}
			if count := recoveryCountForTest(t, a); count != 3 {
				t.Fatal("wrong recovery badge", count)
			}
			p := recoveryPreviewForTest(t, a, []string{id})
			if len(p.Items) != 0 || p.Bytes != 0 || p.Skipped != 1 {
				t.Fatal("pending upload in preview", p)
			}
			p = recoveryPreviewForTest(t, a, []string{stableNode(m.ID, "sub")})
			if len(p.Items) != 0 || p.Skipped != 2 {
				t.Fatal("folder expanded pending children", p)
			}
			w := request(t, a.Handler(nil), "POST", "/api/v1/recovery", recoveryInput{IDs: []string{id}, AccountID: "a", Confirm: true}, "test-csrf")
			if w.Code != 409 || !strings.Contains(w.Body.String(), "原任务") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	// Node identity survives a move even if the original parent is trashed.
	if _, err := a.Store.DB.Exec(`UPDATE nodes SET parent_id='root',name='renamed.mp4' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id=?`, stableNode(m.ID, "sub"))
	list := readTransferListing(t, a, "parent=root&search=renamed")
	if list.Total != 1 || list.Files[0].Transfer == nil {
		t.Fatal("move lost transfer", list)
	}
	if len(recoveryPreviewForTest(t, a, []string{id}).Items) != 0 {
		t.Fatal("move released reservation")
	}
	// An upload in A must never mask a file missing in B.
	addAccount(t, a, "b", "user-b")
	a.Store.Set("active_account", "b")
	if p := recoveryPreviewForTest(t, a, []string{id}); len(p.Items) != 1 || p.Skipped != 0 {
		t.Fatal("cross-account masking", p)
	}
	a.Store.Set("active_account", "a")
	// Completed and cancelled tasks release their claim; present bindings still
	// keep completed files out of recovery, but actual later deletions do not.
	for _, state := range []string{"cancelled", "completed"} {
		j.State = state
		a.Store.SaveJob(&j, nil)
		if p := recoveryPreviewForTest(t, a, []string{id}); len(p.Items) != 1 {
			t.Fatal(state, p)
		}
		if list := readTransferListing(t, a, "view=missing&search=renamed"); list.Total != 1 {
			t.Fatal(state, list)
		}
	}
	j.State = "queued"
	a.Store.SaveJob(&j, nil)
	drive.beforeContent = func() {
		if p := recoveryPreviewForTest(t, a, []string{id}); len(p.Items) != 0 {
			t.Fatal("in-flight upload can be recovered", p)
		}
	}
	a.Execute(context.Background(), &j)
	if j.State != "completed" || drive.begins != 1 || drive.sends != 1 {
		t.Fatal(j, drive.begins, drive.sends)
	}
	if list := readTransferListing(t, a, "search=renamed"); list.Total != 1 || list.Files[0].State != "present" || list.Files[0].Transfer != nil {
		t.Fatal("completed transfer still pending", list)
	}
	if p := recoveryPreviewForTest(t, a, []string{id}); len(p.Items) != 0 {
		t.Fatal(p)
	}
	a.Store.State("a", id, "missing")
	if p := recoveryPreviewForTest(t, a, []string{id}); len(p.Items) != 1 {
		t.Fatal("real deletion hidden", p)
	}
}

func TestTelDriveLegacyRecoveryNeverDuplicatesUpload(t *testing.T) {
	for _, ids := range []string{"file", "folder", "all"} {
		t.Run(ids, func(t *testing.T) {
			a, f := testApp(t)
			td := telDriveFixture(t)
			drive := &uploadFake{fakeDrive: f}
			a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
			m := addMonitor(t, a, td)
			runTDScan(t, a, m)
			id := stableNode(m.ID, "movie")
			selected := []string{id}
			if ids == "folder" {
				selected = []string{stableNode(m.ID, "sub")}
			}
			if ids == "all" {
				selected = nil
			}
			legacy, err := a.Store.NewJob("a", "recover", "legacy recovery", JobData{NodeIDs: selected})
			if err != nil {
				t.Fatal(err)
			}
			a.Execute(context.Background(), &legacy)
			if legacy.State != "completed" || !strings.Contains(legacy.Message, "跳过") || drive.begins != 0 || len(f.files) != 0 {
				t.Fatal("legacy recovery wrote to upstream", legacy, drive.begins, f.files)
			}
			j := tdUploadJob(t, a, "a")
			a.Execute(context.Background(), &j)
			if j.State != "completed" || drive.begins != 1 {
				t.Fatal(j, drive.begins)
			}
		})
	}
}

func TestTelDriveScanAndRecoveryShareReservations(t *testing.T) {
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	runTDScan(t, a, m)
	j := tdUploadJob(t, a, "a")
	id := stableNode(m.ID, "movie")
	w := request(t, a.Handler(nil), "POST", "/api/v1/jobs/"+j.ID+"/cancel", map[string]any{}, "test-csrf")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(t, a.Handler(nil), "POST", "/api/v1/recovery", recoveryInput{IDs: []string{id}, AccountID: "a", Confirm: true}, "test-csrf")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var restore Job
	json.Unmarshal(w.Body.Bytes(), &restore)
	// Repeated preview/create and resurrecting the cancelled ticket cannot
	// compete with the new recovery, including after an app restart.
	reopened, err := Open(a.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	if p := recoveryPreviewForTest(t, NewApp(reopened), []string{id}); len(p.Items) != 0 {
		t.Fatal(p)
	}
	w = request(t, a.Handler(nil), "POST", "/api/v1/recovery", recoveryInput{IDs: []string{id}, AccountID: "a", Confirm: true}, "test-csrf")
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(t, a.Handler(nil), "POST", "/api/v1/jobs/"+j.ID+"/retry", map[string]any{}, "test-csrf")
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if scan := runTDScan(t, a, m); scan.State != "completed" {
		t.Fatal(scan)
	}
	var uploads int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='teldrive_upload'`).Scan(&uploads)
	if uploads != 1 {
		t.Fatal("scan duplicated recovery", uploads)
	}
	a.Execute(context.Background(), &restore)
	if restore.State != "completed" || drive.begins != 1 || drive.sends != 1 {
		t.Fatal(restore, drive.begins, drive.sends)
	}
}

func TestTelDriveDiscoveryHonorsPendingFolderRecovery(t *testing.T) {
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	// A root recovery can be queued before the monitor discovers any nodes.
	restore, err := a.Store.NewJob("a", "recover", "restore all", JobData{})
	if err != nil {
		t.Fatal(err)
	}
	if scan := runTDScan(t, a, m); scan.State != "completed" {
		t.Fatal(scan)
	}
	var competing int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind IN ('sync','teldrive_upload')`).Scan(&competing)
	if competing != 0 {
		t.Fatal("discovery ignored recovery ancestor", competing)
	}
	a.Execute(context.Background(), &restore)
	if restore.State != "completed" || drive.begins != 1 || drive.sends != 1 {
		t.Fatal(restore, drive.begins, drive.sends)
	}
}

func TestConcurrentRecoveryRequestsReserveOnce(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	if err := a.Store.InsertNode(Node{ID: "missing", ParentID: "root", Name: "Missing", Kind: "folder"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(t, a.Handler(nil), "POST", "/api/v1/recovery", recoveryInput{IDs: []string{"missing"}, AccountID: "a", Confirm: true}, "test-csrf")
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[202] != 1 || counts[409] != 1 {
		t.Fatal("duplicate recovery accepted", counts)
	}
}

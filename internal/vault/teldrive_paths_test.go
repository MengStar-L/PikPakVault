package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
)

func preparedTelDrive(t *testing.T) (*App, *uploadFake, *tdFixture, TelDriveMonitor, Node) {
	t.Helper()
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	for _, id := range []string{"landing", "shelf", "other"} {
		if e := a.Store.InsertNode(Node{ID: id, ParentID: "root", Name: id, Kind: "folder"}); e != nil {
			t.Fatal(e)
		}
	}
	m.ParentID = "landing"
	if _, e := a.Store.DB.Exec(`UPDATE teldrive_monitors SET parent_id=? WHERE id=?`, m.ParentID, m.ID); e != nil {
		t.Fatal(e)
	}
	requireComplete(t, runTDScan(t, a, m))
	jobs, _ := a.Store.Jobs()
	for _, j := range jobs {
		if j.Kind == "sync" {
			requireComplete(t, execute(t, a, &j))
		}
	}
	j := tdUploadJob(t, a, "a")
	requireComplete(t, execute(t, a, &j))
	n, e := a.Store.Node(stableNode(m.ID, "movie"), "a")
	if e != nil {
		t.Fatal(e)
	}
	return a, drive, td, m, n
}

func localAction(t *testing.T, a *App, action, id, value string) Job {
	t.Helper()
	w := request(t, a.Handler(nil), "POST", "/api/v1/files/action", map[string]any{
		"action": action, "ids": []string{id}, "parent_id": value, "name": value,
	}, "test-csrf")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Job Job `json:"job"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	j, e := a.Store.Job(result.Job.ID)
	if e != nil {
		t.Fatal(e)
	}
	return j
}

func repairJobs(t *testing.T, a *App) []Job {
	t.Helper()
	jobs, e := a.Store.Jobs()
	if e != nil {
		t.Fatal(e)
	}
	var out []Job
	for _, j := range jobs {
		if j.Kind == "recover" {
			out = append(out, j)
		}
	}
	return out
}

func checkTelDrivePath(t *testing.T, a *App, drive *uploadFake, id, want string) Node {
	t.Helper()
	n, e := a.Store.Node(id, "a")
	if e != nil {
		t.Fatal(e)
	}
	path, e := a.Store.Path(id)
	if e != nil || path != want || n.State != "present" {
		t.Fatal("incorrect local target", path, n.State, e)
	}
	file := drive.files[n.RemoteID]
	parent, e := a.Store.Node(n.ParentID, "a")
	if e != nil || file.Name != n.Name || file.ParentID != parent.RemoteID || file.Hash != n.Hash || int64(file.Size) != n.Size {
		t.Fatal("incorrect remote target or content", file, n, parent, e)
	}
	return n
}

func TestTelDriveMovedFilesAndFoldersNeverResync(t *testing.T) {
	for _, folder := range []bool{false, true} {
		t.Run(fmt.Sprint(folder), func(t *testing.T) {
			a, drive, td, m, n := preparedTelDrive(t)
			id, name, want := n.ID, "renamed.mp4", "/shelf/renamed.mp4"
			if folder {
				id, name, want = n.ParentID, "renamed-folder", "/shelf/renamed-folder/movie.mp4"
			}
			move := localAction(t, a, "move", id, "shelf")
			rename := localAction(t, a, "rename", id, name)
			before := td.downloads.Load()
			requireComplete(t, runTDScan(t, a, m))
			var uploads int
			a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='teldrive_upload'`).Scan(&uploads)
			if uploads != 1 || len(repairJobs(t, a)) != 0 || drive.begins != 1 {
				t.Fatal("path change created transfer work", uploads, drive.begins)
			}
			requireComplete(t, execute(t, a, &move))
			requireComplete(t, execute(t, a, &rename))
			writes := drive.calls["mkdir"] + drive.calls["move"] + drive.calls["rename"]
			for i := 0; i < 2; i++ {
				requireComplete(t, runTDScan(t, a, m))
			}
			if td.downloads.Load() != before || writes != drive.calls["mkdir"]+drive.calls["move"]+drive.calls["rename"] {
				t.Fatal("unchanged resource caused cloud writes")
			}
			checkTelDrivePath(t, a, drive, n.ID, want)
		})
	}
}

func TestTelDriveMovedOutSurvivesDeletedOriginalFolders(t *testing.T) {
	for _, original := range []string{"sub", "landing"} {
		t.Run(original, func(t *testing.T) {
			a, drive, td, m, n := preparedTelDrive(t)
			move := localAction(t, a, "move", n.ID, "shelf")
			rename := localAction(t, a, "rename", n.ID, "last-name.mp4")
			requireComplete(t, execute(t, a, &move))
			requireComplete(t, execute(t, a, &rename))
			a.Store.DB.Exec(`UPDATE nodes SET favorite=1,position=42 WHERE id=?`, n.ID)
			old := "landing"
			if original == "sub" {
				old = stableNode(m.ID, "sub")
			}
			trash := localAction(t, a, "trash", old, "")
			requireComplete(t, execute(t, a, &trash))
			delete(drive.files, n.RemoteID)
			// Source identity remains valid even outside the watched source tree.
			sourceFile := td.files["movie"]
			sourceFile.ParentID = "outside"
			td.files["movie"] = sourceFile
			job := runTDScan(t, a, m)
			if job.State != "completed" && job.State != "partial" {
				t.Fatal(job)
			}
			repairs := repairJobs(t, a)
			if len(repairs) != 1 {
				t.Fatal("moved file was not repaired", repairs)
			}
			requireComplete(t, execute(t, a, &repairs[0]))
			got := checkTelDrivePath(t, a, drive, n.ID, "/shelf/last-name.mp4")
			if got.SourceID != n.SourceID || got.SourcePath != n.SourcePath || !got.Favorite || got.Position != 42 || drive.begins != 2 {
				t.Fatal("restore lost identity or history", got, drive.begins)
			}
		})
	}
}

func TestTelDriveRepairFollowsMonitorModeAndUsesTrashFirst(t *testing.T) {
	a, drive, td, m, n := preparedTelDrive(t)
	remote := drive.files[n.RemoteID]
	remote.Trashed = true
	drive.files[n.RemoteID] = remote
	scan, _ := a.Store.NewJob("a", "scan", "check", JobData{})
	requireComplete(t, execute(t, a, &scan))
	if len(repairJobs(t, a)) != 0 {
		t.Fatal("manual monitor restored automatically")
	}
	a.Store.DB.Exec(`UPDATE teldrive_monitors SET auto_minutes=5 WHERE id=?`, m.ID)
	scan, _ = a.Store.NewJob("a", "scan", "auto check", JobData{})
	requireComplete(t, execute(t, a, &scan))
	repairs := repairJobs(t, a)
	if len(repairs) != 1 {
		t.Fatal("automatic monitor did not queue repair", repairs)
	}
	downloads := td.downloads.Load()
	td.unavailable = true
	requireComplete(t, execute(t, a, &repairs[0]))
	if drive.calls["untrash"] != 1 || drive.begins != 1 || td.downloads.Load() != downloads {
		t.Fatal("reuploaded a file available in trash")
	}
	checkTelDrivePath(t, a, drive, n.ID, "/landing/nested/movie.mp4")
}

func TestTelDriveIncompleteScanAndUncertainStatesNeverUpload(t *testing.T) {
	a, drive, _, m, n := preparedTelDrive(t)
	a.Store.DB.Exec(`UPDATE teldrive_monitors SET auto_minutes=5 WHERE id=?`, m.ID)
	remote := drive.files[n.RemoteID]
	for _, state := range []string{"drift", "pending", "conflict"} {
		file := remote
		switch state {
		case "drift":
			file.ParentID = "elsewhere"
		case "pending":
			file.Phase = "PHASE_TYPE_RUNNING"
		case "conflict":
			file.Hash = "different"
		}
		drive.files[n.RemoteID] = file
		requireComplete(t, runTDScan(t, a, m))
		got, _ := a.Store.Node(n.ID, "a")
		if got.State != state || len(repairJobs(t, a)) != 0 || drive.begins != 1 {
			t.Fatal("uncertain state caused upload", state, got.State)
		}
	}
	delete(drive.files, n.RemoteID)
	ac, _ := a.Store.Account("a")
	drive.failPage = ac.RootID
	scan := runTDScan(t, a, m)
	got, _ := a.Store.Node(n.ID, "a")
	if scan.State != "retry" || got.State != "unknown" || len(repairJobs(t, a)) != 0 {
		t.Fatal("incomplete scan declared missing", scan, got.State)
	}
	drive.failPage = ""
	scan.State = "cancelled"
	a.Store.SaveJob(&scan, nil)
	move := localAction(t, a, "move", n.ParentID, "shelf")
	requireComplete(t, runTDScan(t, a, m))
	if len(repairJobs(t, a)) != 0 {
		t.Fatal("repair raced queued ancestor move")
	}
	move.State = "failed"
	a.Store.SaveJob(&move, nil)
	requireComplete(t, runTDScan(t, a, m))
	if len(repairJobs(t, a)) != 1 {
		t.Fatal("failed move prevented missing-file recovery")
	}
}

func TestTelDriveTransferUsesLatestTargetWithoutAnotherTicket(t *testing.T) {
	for _, stage := range []string{"download", "upload", "ancestor", "lost-ticket"} {
		t.Run(stage, func(t *testing.T) {
			a, f := testApp(t)
			td := telDriveFixture(t)
			drive := &uploadFake{fakeDrive: f}
			a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
			m := addMonitor(t, a, td)
			a.Store.InsertNode(Node{ID: "shelf", ParentID: "root", Name: "shelf", Kind: "folder"})
			runTDScan(t, a, m)
			id := stableNode(m.ID, "movie")
			change := func() {
				localAction(t, a, "move", id, "shelf")
				localAction(t, a, "rename", id, "latest.mp4")
			}
			want := "/shelf/latest.mp4"
			switch stage {
			case "download":
				td.onDownload = change
			case "upload":
				drive.beforeContent = change
			case "ancestor":
				want = "/shelf/relocated/movie.mp4"
				drive.beforeContent = func() {
					localAction(t, a, "move", stableNode(m.ID, "sub"), "shelf")
					localAction(t, a, "rename", stableNode(m.ID, "sub"), "relocated")
				}
			case "lost-ticket":
				drive.instant, drive.lostTicket, drive.afterBegin = true, true, change
			}
			j := tdUploadJob(t, a, "a")
			a.Execute(context.Background(), &j)
			if stage == "lost-ticket" {
				if j.State != "retry" {
					t.Fatal(j)
				}
				a = NewApp(a.Store)
				a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
				a.Execute(context.Background(), &j)
			}
			requireComplete(t, j)
			checkTelDrivePath(t, a, drive, id, want)
			if drive.begins != 1 || td.downloads.Load() != 1 || !strings.Contains(j.Message, want) {
				t.Fatal("duplicated transfer or stale path", j, drive.begins, td.downloads.Load())
			}
		})
	}
}

func TestTelDriveRootLossRepairsOnceAcrossRestartAndSwitch(t *testing.T) {
	a, drive, _, m, n := preparedTelDrive(t)
	move := localAction(t, a, "move", n.ParentID, "shelf")
	requireComplete(t, execute(t, a, &move))
	for id := range drive.files {
		delete(drive.files, id)
	}
	requireComplete(t, runTDScan(t, a, m))
	repairs := repairJobs(t, a)
	if len(repairs) != 1 {
		t.Fatal(repairs)
	}
	addAccount(t, a, "b", "user-b")
	drive.beforeContent = func() { a.Store.Set("active_account", "b") }
	a.Execute(context.Background(), &repairs[0])
	if repairs[0].State != "paused" || drive.begins != 2 || drive.sends != 1 {
		t.Fatal("switch lost repair ownership", repairs[0], drive.begins, drive.sends)
	}
	a = NewApp(a.Store)
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	a.Store.Set("active_account", "a")
	requireComplete(t, runTDScan(t, a, m))
	if len(repairJobs(t, a)) != 1 {
		t.Fatal("restart duplicated repair")
	}
	w := request(t, a.Handler(nil), "POST", "/api/v1/jobs/"+repairs[0].ID+"/retry", map[string]any{}, "test-csrf")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	requireComplete(t, execute(t, a, &repairs[0]))
	checkTelDrivePath(t, a, drive, n.ID, "/shelf/nested/movie.mp4")
	if drive.begins != 2 || drive.sends != 2 {
		t.Fatal("resuming repair created another ticket")
	}
}

func TestTelDriveDirectoryRepairLeavesHealthyDescendantsAlone(t *testing.T) {
	a, drive, td, m, n := preparedTelDrive(t)
	folder, _ := a.Store.Node(n.ParentID, "a")
	delete(drive.files, folder.RemoteID)
	file := drive.files[n.RemoteID]
	file.ParentID = "external-folder"
	drive.files[n.RemoteID] = file
	requireComplete(t, runTDScan(t, a, m))
	repairs := repairJobs(t, a)
	if len(repairs) != 1 {
		t.Fatal(repairs)
	}
	var d JobData
	json.Unmarshal(repairs[0].Data, &d)
	if !d.ExactNodes || len(d.NodeIDs) != 1 || d.NodeIDs[0] != folder.ID {
		t.Fatal("directory repair expanded into healthy files", d.NodeIDs)
	}
	operations, e := nodeJobs(a.Store.DB, "a")
	if e != nil {
		t.Fatal(e)
	}
	if _, reserved := operations[n.ID]; reserved {
		t.Fatal("exact directory repair reserved healthy file")
	}
	requireComplete(t, execute(t, a, &repairs[0]))
	if drive.files[n.RemoteID].ParentID != "external-folder" || drive.begins != 1 || td.downloads.Load() != 1 {
		t.Fatal("repair moved or uploaded a healthy descendant")
	}
}

func TestTelDriveRepairHonorsLatestTrashAndSourceFailure(t *testing.T) {
	for _, failure := range []string{"trash", "source", "conflict"} {
		t.Run(failure, func(t *testing.T) {
			a, drive, td, m, n := preparedTelDrive(t)
			delete(drive.files, n.RemoteID)
			requireComplete(t, runTDScan(t, a, m))
			repairs := repairJobs(t, a)
			if len(repairs) != 1 {
				t.Fatal(repairs)
			}
			switch failure {
			case "trash":
				localAction(t, a, "trash", n.ParentID, "")
			case "source":
				td.unavailable = true
			case "conflict":
				parent, _ := a.Store.Node(n.ParentID, "a")
				drive.files["untracked"] = pikpak.File{ID: "untracked", ParentID: parent.RemoteID, Name: n.Name, Kind: "drive#file", Size: 123, Hash: "foreign", Phase: "PHASE_TYPE_COMPLETE"}
			}
			a.Execute(context.Background(), &repairs[0])
			if failure == "trash" {
				requireComplete(t, repairs[0])
			} else if repairs[0].State != "partial" {
				t.Fatal("missing actionable repair error", repairs[0])
			}
			if drive.begins != 1 || drive.calls["untrash"] != 0 {
				t.Fatal("repair ignored deletion/source/conflict", drive.begins)
			}
			if failure == "conflict" && drive.files["untracked"].Hash != "foreign" {
				t.Fatal("overwrote conflicting item")
			}
		})
	}
}

func TestTelDriveFolderLostResponseUsesOriginalCreationLocation(t *testing.T) {
	a, drive, _, m, n := preparedTelDrive(t)
	id := stableNode(m.ID, "new-directory")
	a.Store.InsertNode(Node{ID: id, ParentID: n.ParentID, Name: "original", Kind: "folder", SourceKey: "new-directory"})
	drive.lostMkdir = true
	_, e := a.folder(context.Background(), drive, "a", id, map[string]bool{})
	if e == nil {
		t.Fatal("fixture did not lose creation response")
	}
	count := drive.calls["mkdir"]
	localAction(t, a, "move", id, "landing")
	localAction(t, a, "rename", id, "latest")
	remote, e := a.folder(context.Background(), drive, "a", id, map[string]bool{})
	if e != nil {
		t.Fatal(e)
	}
	parent, _ := a.Store.Node("landing", "a")
	if drive.calls["mkdir"] != count || drive.files[remote].ParentID != parent.RemoteID || drive.files[remote].Name != "latest" {
		t.Fatal("lost folder response created a duplicate", drive.files[remote])
	}
}

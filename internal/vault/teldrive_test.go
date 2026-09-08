package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
	"pikpakvault/internal/teldrive"
)

type tdFixture struct {
	server                     *httptest.Server
	files                      map[string]teldrive.File
	body                       string
	unavailable, broken, short bool
}

func telDriveFixture(t *testing.T) *tdFixture {
	t.Helper()
	f := &tdFixture{body: "original content", files: map[string]teldrive.File{}}
	f.files["folder"] = teldrive.File{ID: "folder", Name: "collection", Type: "folder"}
	f.files["sub"] = teldrive.File{ID: "sub", Name: "nested", Type: "folder", ParentID: "folder"}
	f.files["empty"] = teldrive.File{ID: "empty", Name: "empty", Type: "folder", ParentID: "folder"}
	f.files["movie"] = teldrive.File{ID: "movie", Name: "movie.mp4", Type: "file", ParentID: "sub", Size: int64(len(f.body)), Hash: "blake3-v1", Updated: "2026-09-08T00:00:00Z", Mime: "video/mp4"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, e := r.Cookie("access_token")
		if e != nil || cookie.Value != "td-private-token" {
			http.Error(w, "bad token", 401)
			return
		}
		if f.unavailable {
			http.Error(w, "td-private-token", 401)
			return
		}
		if r.URL.Path == "/api/files" {
			parent := r.URL.Query().Get("parentId")
			if f.broken && parent == "sub" {
				http.Error(w, "broken page", 503)
				return
			}
			p := teldrive.Page{Items: []teldrive.File{}}
			p.Meta.Current = 1
			p.Meta.Pages = 1
			for _, file := range f.files {
				if file.ParentID == parent {
					p.Items = append(p.Items, file)
				}
			}
			p.Meta.Count = len(p.Items)
			json.NewEncoder(w).Encode(p)
			return
		}
		if r.URL.Path == "/api/files/movie/movie.mp4" {
			var start, end int64
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(f.body)))
			w.WriteHeader(206)
			if f.short {
				io.WriteString(w, "short")
			} else {
				io.WriteString(w, f.body[start:end+1])
			}
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/files/")
		if file, ok := f.files[id]; ok {
			json.NewEncoder(w).Encode(file)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}
func addMonitor(t *testing.T, a *App, f *tdFixture) TelDriveMonitor {
	t.Helper()
	sessionForTest(t, a)
	w := request(t, a.Handler(nil), "POST", "/api/v1/teldrive", telDriveInput{Name: "Telegram 收藏", BaseURL: f.server.URL, Token: "td-private-token", FolderID: "folder", FolderPath: "/collection", ParentID: "root"}, "test-csrf")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var res struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	m, e := a.Store.monitor(res.ID)
	if e != nil {
		t.Fatal(e)
	}
	return m
}

type uploadFake struct {
	*fakeDrive
	begins, sends                    int
	lostTicket, lostContent, instant bool
	beforeContent                    func()
	data                             string
}

func (f *uploadFake) BeginUpload(_ context.Context, parent string, file pikpak.File) (pikpak.UploadTicket, error) {
	f.begins++
	file.ID = f.newID()
	file.Kind = "drive#file"
	file.ParentID = parent
	file.Phase = "PHASE_TYPE_RUNNING"
	if f.instant {
		file.Phase = "PHASE_TYPE_COMPLETE"
	}
	f.files[file.ID] = file
	u := pikpak.UploadTicket{File: &file, Resumable: &struct {
		Params pikpak.UploadParams `json:"params"`
	}{pikpak.UploadParams{AccessKeyID: "private-ticket"}}}
	if f.lostTicket {
		return pikpak.UploadTicket{}, &pikpak.APIError{Code: "response_lost"}
	}
	return u, nil
}
func (f *uploadFake) ContinueUpload(ctx context.Context, u *pikpak.UploadSession, size int64, read pikpak.RangeReader, save func(int64) error) error {
	if f.beforeContent != nil {
		f.beforeContent()
		f.beforeContent = nil
	}
	if e := save(0); e != nil {
		return e
	}
	b, e := read(ctx, 0, size)
	if e != nil {
		return e
	}
	f.data = string(b)
	f.sends++
	file := f.files[u.Ticket.File.ID]
	file.Phase = "PHASE_TYPE_COMPLETE"
	f.files[file.ID] = file
	if f.lostContent {
		f.lostContent = false
		return &pikpak.APIError{Code: "lost_upload_result"}
	}
	u.Sent = true
	return save(size)
}
func runTDScan(t *testing.T, a *App, m TelDriveMonitor) Job {
	t.Helper()
	j, e := a.newTelDriveScan(m, a.active())
	if e != nil {
		t.Fatal(e)
	}
	a.Execute(context.Background(), &j)
	j, _ = a.Store.Job(j.ID)
	return j
}
func tdUploadJob(t *testing.T, a *App, account string) Job {
	t.Helper()
	j, e := jobScan(a.Store.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE kind='teldrive_upload' AND account_id=? ORDER BY rowid DESC LIMIT 1`, account))
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func TestTelDriveScanUploadDeduplicateRecoverAcrossAccounts(t *testing.T) {
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	if j := runTDScan(t, a, m); j.State != "completed" {
		t.Fatal(j)
	}
	j := tdUploadJob(t, a, "a")
	a.Execute(context.Background(), &j)
	if j.State != "completed" || drive.data != td.body {
		t.Fatal(j, drive.data)
	}
	n, e := a.Store.Node(stableNode(m.ID, "movie"), "a")
	if e != nil || n.State != "present" || n.Hash == "" || n.Hash == "blake3-v1" {
		t.Fatal(n, e)
	}
	for _, file := range f.files {
		if strings.HasPrefix(file.Name, ".vault-folder-") {
			t.Fatal("temporary TelDrive folder", file.Name)
		}
	}
	runTDScan(t, a, m)
	var uploads int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='teldrive_upload'`).Scan(&uploads)
	if uploads != 1 || drive.begins != 1 {
		t.Fatal("duplicate sync", uploads, drive.begins)
	}
	// Local moves and renames remain the restore target, independently of source.
	a.Store.DB.Exec(`UPDATE nodes SET parent_id='root',name='renamed.mp4',revision=revision+1,favorite=1,position=12 WHERE id=?`, n.ID)
	delete(f.files, n.RemoteID)
	a.Store.State("a", n.ID, "missing")
	recoverJob, _ := a.Store.NewJob("a", "recover", "restore", JobData{NodeIDs: []string{n.ID}})
	a.Execute(context.Background(), &recoverJob)
	if recoverJob.State != "completed" {
		t.Fatal(recoverJob)
	}
	n, _ = a.Store.Node(n.ID, "a")
	if n.Name != "renamed.mp4" || n.ParentID != "root" || !n.Favorite || n.Position != 12 {
		t.Fatal(n)
	}
	addAccount(t, a, "b", "user-b")
	b := &uploadFake{fakeDrive: newFake("user-b")}
	a.Factory = func(ac Account) (pikpak.Provider, error) {
		if ac.ID == "b" {
			return b, nil
		}
		return drive, nil
	}
	a.Store.Set("active_account", "b")
	if scan := runTDScan(t, a, m); scan.State != "completed" {
		t.Fatal(scan)
	}
	jobB := tdUploadJob(t, a, "b")
	a.Execute(context.Background(), &jobB)
	if jobB.State != "completed" || b.data != td.body {
		t.Fatal(jobB)
	}
	n, _ = a.Store.Node(n.ID, "b")
	if n.Name != "renamed.mp4" || n.State != "present" {
		t.Fatal(n)
	}
}
func TestTelDriveIncompleteSourceAndChangesNeverUpload(t *testing.T) {
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	td.broken = true
	j := runTDScan(t, a, m)
	if j.State != "failed" {
		t.Fatal(j)
	}
	var count int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id<>'root'`).Scan(&count)
	if count != 0 {
		t.Fatal("partial scan wrote nodes")
	}
	td.broken = false
	runTDScan(t, a, m)
	j = tdUploadJob(t, a, "a")
	td.short = true
	a.Execute(context.Background(), &j)
	if j.State == "completed" || drive.begins != 0 {
		t.Fatal("truncated source dispatched", j)
	}
	td.short = false
	changed := td.files["movie"]
	changed.Hash = "changed-content"
	td.files["movie"] = changed
	j.State = "queued"
	a.Store.SaveJob(&j, nil)
	a.Execute(context.Background(), &j)
	if j.State != "attention" || drive.begins != 0 {
		t.Fatal(j)
	}
	if scan := runTDScan(t, a, m); scan.State != "partial" {
		t.Fatal("source mutation not shown", scan)
	}
}
func TestTelDriveLostResultsAndRestartDoNotDuplicate(t *testing.T) {
	for _, ticketLost := range []bool{false, true} {
		t.Run(fmt.Sprint(ticketLost), func(t *testing.T) {
			a, f := testApp(t)
			td := telDriveFixture(t)
			drive := &uploadFake{fakeDrive: f, lostTicket: ticketLost, lostContent: !ticketLost, instant: ticketLost}
			a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
			m := addMonitor(t, a, td)
			runTDScan(t, a, m)
			j := tdUploadJob(t, a, "a")
			a.Execute(context.Background(), &j)
			if j.State != "retry" {
				t.Fatal(j)
			}
			if !ticketLost {
				td.unavailable = true
			} // Saved file still wins over expired source authentication.
			restarted := NewApp(a.Store)
			restarted.Factory = a.Factory
			restarted.Execute(context.Background(), &j)
			if j.State != "completed" || drive.begins != 1 {
				t.Fatal("duplicate or lost result", j, drive.begins)
			}
		})
	}
}
func TestTelDriveSwitchAndBackupKeepEncryptedUploadSession(t *testing.T) {
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	runTDScan(t, a, m)
	j := tdUploadJob(t, a, "a")
	addAccount(t, a, "b", "user-b")
	drive.beforeContent = func() { a.Store.Set("active_account", "b") }
	a.Execute(context.Background(), &j)
	if j.State != "paused" || drive.sends != 0 || drive.begins != 1 {
		t.Fatal("switch did not pause", j)
	}
	stored, _ := a.Store.Job(j.ID)
	if strings.Contains(string(stored.Data), "private-ticket") || strings.Contains(string(stored.Data), "td-private-token") {
		t.Fatal("plaintext job credentials")
	}
	w := request(t, a.Handler(nil), "GET", "/api/v1/teldrive", nil, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "td-private-token") {
		t.Fatal(w.Code, w.Body.String())
	}
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if e := a.Store.Backup(archive); e != nil {
		t.Fatal(e)
	}
	src, e := ExtractBackup(archive, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer src.DB.Close()
	dst, _ := testApp(t)
	if e = replaceData(dst.Store, src); e != nil {
		t.Fatal(e)
	}
	imported, e := dst.Store.monitor(m.ID)
	if e != nil {
		t.Fatal(e)
	}
	client, e := dst.telDriveClient(imported)
	if e != nil || client.Token != "td-private-token" {
		t.Fatal("imported token invalid", e)
	}
	importJob, _ := dst.Store.Job(j.ID)
	var d JobData
	json.Unmarshal(importJob.Data, &d)
	var session pikpak.UploadSession
	if e = dst.Store.Unseal(d.Uploads[stableNode(m.ID, "movie")].Secret, &session); e != nil || session.Ticket.Resumable.Params.AccessKeyID != "private-ticket" {
		t.Fatal("imported session invalid", e)
	}
	// Resume on the original account after a switch without a second ticket.
	a.Store.Set("active_account", "a")
	j.State = "queued"
	a.Store.SaveJob(&j, nil)
	a.Execute(context.Background(), &j)
	if j.State != "completed" || drive.begins != 1 {
		t.Fatal(j, drive.begins)
	}
}
func TestTelDriveTrashConflictAutomaticAndAuthentication(t *testing.T) {
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	a.scheduleTelDrive()
	var count int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='teldrive_scan'`).Scan(&count)
	if count != 0 {
		t.Fatal("automatic sync enabled by default")
	}
	a.Store.DB.Exec(`UPDATE teldrive_monitors SET auto_minutes=5 WHERE id=?`, m.ID)
	a.scheduleTelDrive()
	a.scheduleTelDrive()
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='teldrive_scan'`).Scan(&count)
	if count != 1 {
		t.Fatal("scheduler duplicate", count)
	}
	runTDScan(t, a, m)
	id := stableNode(m.ID, "movie")
	a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id=?`, id)
	j := tdUploadJob(t, a, "a")
	a.Execute(context.Background(), &j)
	if drive.begins != 0 || j.State != "completed" {
		t.Fatal(j)
	}
	delete(td.files, "movie")
	runTDScan(t, a, m)
	n, e := a.Store.Node(id, "a")
	if e != nil || !n.Trashed {
		t.Fatal("source deletion removed history", e)
	}
	if w := request(t, a.Handler(nil), "POST", "/api/v1/teldrive/"+m.ID+"/sync", map[string]bool{}, ""); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	w := request(t, a.Handler(nil), "POST", "/api/v1/teldrive/browse", telDriveInput{ID: m.ID, BaseURL: "https://another.example", Page: 1}, "test-csrf")
	if w.Code != 400 {
		t.Fatal("implicit credential forwarded to other origin")
	}
}

func TestTelDriveRemoteCollisionAndFolderLostResponse(t *testing.T) {
	a, f := testApp(t)
	td := telDriveFixture(t)
	drive := &uploadFake{fakeDrive: f}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	runTDScan(t, a, m)
	if _, e := a.ensureRoot(context.Background(), f, "a"); e != nil {
		t.Fatal(e)
	}
	f.lostMkdir = true
	folder := stableNode(m.ID, "sub")
	if _, e := a.folder(context.Background(), f, "a", folder, map[string]bool{}); e == nil {
		t.Fatal("expected uncertain directory response")
	}
	before := f.calls["mkdir"]
	parent, e := a.folder(context.Background(), f, "a", folder, map[string]bool{})
	if e != nil || f.calls["mkdir"] != before {
		t.Fatal("duplicate directory", e)
	}
	// A process can stop after binding the folder but before clearing the intent.
	// Resolving that binding must clear the stale intent for future root recovery.
	a.Store.Set("teldrive_folder:a:"+folder, "stale-intent")
	if _, e = a.folder(context.Background(), f, "a", folder, map[string]bool{}); e != nil {
		t.Fatal(e)
	}
	if a.Store.Get("teldrive_folder:a:"+folder) != "" {
		t.Fatal("stale intent retained")
	}
	f.files["untracked"] = pikpak.File{ID: "untracked", ParentID: parent, Name: "movie.mp4", Kind: "drive#file", Size: 1, Hash: "different", Phase: "PHASE_TYPE_COMPLETE"}
	j := tdUploadJob(t, a, "a")
	a.Execute(context.Background(), &j)
	if j.State != "attention" || drive.begins != 0 || f.calls["trash"] != 0 {
		t.Fatal("overwrote conflict", j)
	}
}

func TestTelDriveAcceptsLegacyBackupAndMigrates(t *testing.T) {
	a, _ := testApp(t)
	if _, e := a.Store.DB.Exec(`DROP TABLE teldrive_monitors; PRAGMA user_version=1`); e != nil {
		t.Fatal(e)
	}
	archive := filepath.Join(t.TempDir(), "legacy.zip")
	if e := a.Store.Backup(archive); e != nil {
		t.Fatal(e)
	}
	src, e := ExtractBackup(archive, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer src.DB.Close()
	dst, _ := testApp(t)
	if e = replaceData(dst.Store, src); e != nil {
		t.Fatal(e)
	}
	ms, e := dst.Store.monitors()
	if e != nil || len(ms) != 0 {
		t.Fatal(ms, e)
	}
	var version int
	dst.Store.DB.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != 2 {
		t.Fatal(version)
	}
	dir := a.Store.Dir
	a.Store.DB.Close()
	reopened, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.DB.Close()
	if _, e = reopened.monitors(); e != nil {
		t.Fatal(e)
	}
}

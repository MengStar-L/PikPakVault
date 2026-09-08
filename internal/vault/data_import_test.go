package vault

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
)

func backupFixture(t *testing.T) (*App, []byte) {
	t.Helper()
	a, _ := testApp(t)
	a.Store.Set("password", passwordHash("backup-admin-password"))
	a.Store.Set("folder_previews", "true")
	ac, _ := a.Store.Account("a")
	ac.Secret, _ = a.Store.Seal(pikpak.Credentials{Username: "fixture@example.test", Password: "fixture-account-password", AccessToken: "fixture-access", RefreshToken: "fixture-refresh"})
	if e := func() error {
		_, e := a.Store.DB.Exec(`UPDATE accounts SET secret=? WHERE id=?`, ac.Secret, ac.ID)
		return e
	}(); e != nil {
		t.Fatal(e)
	}
	source, e := ParseSource("https://mypikpak.com/s/backup-share", "pass-code")
	if e != nil {
		t.Fatal(e)
	}
	source.Secret, _ = a.Store.Seal("pass-code")
	if e = a.Store.SaveSource(source); e != nil {
		t.Fatal(e)
	}
	n := Node{ID: "saved", ParentID: "root", Name: "Moved movie.mp4", Kind: "file", Size: 123, SourceID: source.ID, SourcePath: "old/path/movie.mp4", Created: 1, Modified: 2}
	if e = a.Store.InsertNode(n); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Store.DB.Exec(`UPDATE nodes SET favorite=1,position=123.5,opened=456,trashed=1 WHERE id='saved'`); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Store.DB.Exec(`INSERT INTO bindings(account_id,node_id,remote_id,state) VALUES('a','saved','remote-saved','present')`); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Store.NewJob("a", "scan", "unfinished", JobData{}); e != nil {
		t.Fatal(e)
	}
	sessionForTest(t, a)
	a.Store.Event("fixture", true)
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if e = a.Store.Backup(archive); e != nil {
		t.Fatal(e)
	}
	body, e := os.ReadFile(archive)
	if e != nil {
		t.Fatal(e)
	}
	return a, body
}

func previewBackup(t *testing.T, a *App, body []byte, setup bool) *httptest.ResponseRecorder {
	t.Helper()
	prefix := "data"
	if setup {
		prefix = "auth"
	}
	r := httptest.NewRequest("POST", "/api/v1/"+prefix+"/import/preview", bytes.NewReader(body))
	r.Header.Set("X-CSRF-Token", "test-csrf")
	r.Header.Set("X-Setup-Token", a.SetupToken)
	r.AddCookie(&http.Cookie{Name: "vault_session", Value: "test-session"})
	w := httptest.NewRecorder()
	a.Handler(nil).ServeHTTP(w, r)
	return w
}
func TestFullImportPreservesSecretsPathsAndHistory(t *testing.T) {
	original, body := backupFixture(t)
	dst, _ := testApp(t)
	dst.Store.Set("password", passwordHash("current-password"))
	sessionForTest(t, dst)
	key := append([]byte{}, dst.Store.key...)
	w := previewBackup(t, dst, body, false)
	if w.Code != 200 {
		t.Fatalf("preview %d %s", w.Code, w.Body.String())
	}
	var p BackupPreview
	json.Unmarshal(w.Body.Bytes(), &p)
	if p.Accounts != 1 || p.Files != 1 || p.Sources != 1 || p.Jobs != 1 {
		t.Fatalf("counts %+v", p)
	}
	h := dst.Handler(nil)
	args := map[string]any{"id": p.ID, "confirm": true, "current_password": "wrong"}
	w = request(t, h, "POST", "/api/v1/data/import/commit", args, "test-csrf")
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	args["current_password"] = "current-password"
	dst.dataMu.RLock()
	w = request(t, h, "POST", "/api/v1/data/import/commit", args, "test-csrf")
	dst.dataMu.RUnlock()
	if w.Code != 409 {
		t.Fatal("busy import was allowed")
	}
	w = request(t, h, "POST", "/api/v1/data/import/commit", args, "test-csrf")
	if w.Code != 200 {
		t.Fatalf("commit %d %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(key, dst.Store.key) || !passwordOK("backup-admin-password", dst.Store.Get("password")) {
		t.Fatal("key/password mismatch")
	}
	a, _ := dst.Store.Account("a")
	o, _ := original.Store.Account("a")
	if a.Secret == o.Secret {
		t.Fatal("ciphertext not rekeyed")
	}
	var creds pikpak.Credentials
	if e := dst.Store.Unseal(a.Secret, &creds); e != nil || creds.RefreshToken != "fixture-refresh" || creds.Password != "fixture-account-password" {
		t.Fatalf("credentials lost: %v", e)
	}
	n, e := dst.Store.Node("saved", "a")
	if e != nil || !n.Favorite || !n.Trashed || n.Position != 123.5 || n.RemoteID != "remote-saved" || n.SourcePath != "old/path/movie.mp4" {
		t.Fatalf("record mismatch: %+v %v", n, e)
	}
	source, e := dst.Store.Source(n.SourceID)
	var code string
	if e != nil {
		t.Fatal(e)
	}
	if e = dst.Store.Unseal(source.Secret, &code); e != nil || code != "pass-code" {
		t.Fatal("share code lost")
	}
	w = request(t, h, "GET", "/api/v1/accounts", nil, "")
	if w.Code != 401 {
		t.Fatal("old session still valid")
	}
	if dst.Store.Get("import_review_required") != "true" {
		t.Fatal("scheduler not gated")
	}
	var job Job
	job, e = jobScan(dst.Store.DB.QueryRow(`SELECT ` + jobCols + ` FROM jobs LIMIT 1`))
	if e != nil || job.State != "paused" {
		t.Fatal("incomplete job not paused")
	}
	dst.Execute(context.Background(), &job)
	if job.State != "paused" {
		t.Fatal("import ran upstream task")
	}
	backups, _ := filepath.Glob(filepath.Join(dst.Store.Dir, "before-import-*.zip"))
	if len(backups) != 1 {
		t.Fatal("previous database not backed up")
	}
	if _, e = os.Stat(filepath.Join(dst.Store.Dir, "imports", p.ID)); !os.IsNotExist(e) {
		t.Fatal("staging credentials retained")
	}
}

func TestSetupImportAndMalformedBackup(t *testing.T) {
	_, body := backupFixture(t)
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	a := NewApp(s)
	a.SetupToken = "fixture-setup-code"
	w := previewBackup(t, a, body, true)
	if w.Code != 200 {
		t.Fatalf("setup preview %d %s", w.Code, w.Body.String())
	}
	var p BackupPreview
	json.Unmarshal(w.Body.Bytes(), &p)
	r := httptest.NewRequest("POST", "/api/v1/auth/import/commit", strings.NewReader(jsonText(map[string]any{"id": p.ID, "confirm": true})))
	r.Header.Set("X-Setup-Token", a.SetupToken)
	w = httptest.NewRecorder()
	a.Handler(nil).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("setup commit %d %s", w.Code, w.Body.String())
	}
	if !passwordOK("backup-admin-password", s.Get("password")) {
		t.Fatal("setup import missing password")
	}
	w = previewBackup(t, a, body, true)
	if w.Code != 409 {
		t.Fatal("setup bypass after configuration")
	}
	sessionForTest(t, a)
	for _, variant := range []string{"broken", "wrong-key", "duplicate", "traversal"} {
		t.Run(variant, func(t *testing.T) {
			data := []byte("not-a-zip")
			if variant != "broken" {
				buf := &bytes.Buffer{}
				z := zip.NewWriter(buf)
				original, _ := zip.NewReader(bytes.NewReader(body), int64(len(body)))
				for _, f := range original.File {
					r, _ := f.Open()
					v := &bytes.Buffer{}
					v.ReadFrom(r)
					r.Close()
					if f.Name == "master.key" && variant == "wrong-key" {
						v = bytes.NewBuffer(bytes.Repeat([]byte{42}, 32))
					}
					entry, _ := z.Create(f.Name)
					entry.Write(v.Bytes())
				}
				if variant == "duplicate" {
					entry, _ := z.Create("master.key")
					entry.Write(make([]byte, 32))
				}
				if variant == "traversal" {
					entry, _ := z.Create("../escape")
					entry.Write([]byte("bad"))
				}
				z.Close()
				data = buf.Bytes()
			}
			w := previewBackup(t, a, data, false)
			if w.Code != 400 {
				t.Fatalf("invalid backup accepted: %d %s", w.Code, w.Body.String())
			}
			if !passwordOK("backup-admin-password", s.Get("password")) {
				t.Fatal("invalid import mutated data")
			}
		})
	}
}

func TestImportTransactionRollsBackOnBadSchema(t *testing.T) {
	src, _ := testApp(t)
	dst, _ := testApp(t)
	dst.Store.Set("sentinel", "keep")
	src.Store.NewJob("a", "scan", "incomplete", JobData{})
	if _, e := src.Store.DB.Exec(`ALTER TABLE jobs ADD COLUMN unsupported TEXT`); e != nil {
		t.Fatal(e)
	}
	if e := replaceData(dst.Store, src.Store); e == nil {
		t.Fatal("unsupported column not rejected")
	}
	if dst.Store.Get("sentinel") != "keep" {
		t.Fatal("failed replacement committed partial data")
	}
}

func TestExtractBackupRelativeDataDirectory(t *testing.T) {
	_, body := backupFixture(t)
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if e := os.WriteFile(archive, body, 0600); e != nil {
		t.Fatal(e)
	}
	t.Chdir(t.TempDir())
	relative := "backup-target"
	if e := os.Mkdir(relative, 0700); e != nil {
		t.Fatal(e)
	}
	s, e := ExtractBackup(archive, relative)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	if !passwordOK("backup-admin-password", s.Get("password")) {
		t.Fatal("relative extraction lost database")
	}
}

func TestUnconfiguredBackupSupportsRollbackButNotWebImport(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	archive := filepath.Join(t.TempDir(), "unconfigured.zip")
	if e = s.Backup(archive); e != nil {
		t.Fatal(e)
	}
	restored, e := ExtractBackup(archive, t.TempDir())
	if e != nil {
		t.Fatalf("rollback of a fresh instance failed: %v", e)
	}
	restored.DB.Close()
	body, e := os.ReadFile(archive)
	if e != nil {
		t.Fatal(e)
	}
	a := NewApp(s)
	a.SetupToken = "setup-test"
	w := previewBackup(t, a, body, true)
	if w.Code != 400 {
		t.Fatalf("web import should require an administrator: %d", w.Code)
	}
}

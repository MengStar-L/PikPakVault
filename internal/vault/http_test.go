package vault

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pikpakvault/internal/pikpak"
)

func request(t *testing.T, handler http.Handler, method, path string, body any, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var data io.Reader
	if body != nil {
		data = bytes.NewBufferString(jsonText(body))
	}
	r := httptest.NewRequest(method, path, data)
	r.AddCookie(&http.Cookie{Name: "vault_session", Value: "test-session"})
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func sessionForTest(t *testing.T, a *App) {
	t.Helper()
	_, err := a.Store.DB.Exec(`INSERT INTO sessions(id,csrf,expires) VALUES(?,?,?)`, tokenHash("test-session"), "test-csrf", now()+3600)
	if err != nil {
		t.Fatal(err)
	}
}
func TestAuthenticationCSRFAndMutations(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	h := a.Handler(nil)
	r := request(t, h, "POST", "/api/v1/files", map[string]string{"name": "folder", "parent_id": "root"}, "")
	if r.Code != 403 {
		t.Fatalf("missing CSRF allowed %d", r.Code)
	}
	r = request(t, h, "POST", "/api/v1/files", map[string]string{"name": "folder", "parent_id": "root"}, "test-csrf")
	if r.Code != 201 {
		t.Fatalf("create failed %d %s", r.Code, r.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(r.Body.Bytes(), &created)
	r = request(t, h, "POST", "/api/v1/files/action", map[string]any{"ids": []string{created.ID}, "action": "move", "parent_id": created.ID}, "test-csrf")
	if r.Code != 400 {
		t.Fatalf("cycle allowed %d", r.Code)
	}
	r = request(t, h, "POST", "/api/v1/files/action", map[string]any{"ids": []string{created.ID}, "action": "trash"}, "test-csrf")
	if r.Code != 200 {
		t.Fatalf("trash failed %s", r.Body.String())
	}
	r = request(t, h, "GET", "/api/v1/files?view=trash", nil, "")
	if !strings.Contains(r.Body.String(), created.ID) {
		t.Fatal("trash listing missing record")
	}
	r = request(t, h, "POST", "/api/v1/files/action", map[string]any{"ids": []string{created.ID}, "action": "restore"}, "test-csrf")
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	n, _ := a.Store.Node(created.ID, "a")
	if n.Trashed {
		t.Fatal("restore did not retain local item")
	}
	r = request(t, h, "POST", "/api/v1/files/action", map[string]any{"ids": []string{"root"}, "action": "trash"}, "test-csrf")
	if r.Code != 400 {
		t.Fatal("root trash allowed")
	}
}
func TestFolderMutationAndQueueAreAtomic(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	_, err := a.Store.DB.Exec(`CREATE TRIGGER reject_job BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT,'injected job write failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	r := request(t, a.Handler(nil), "POST", "/api/v1/files", map[string]string{"name": "not-created", "parent_id": "root"}, "test-csrf")
	if r.Code != 500 {
		t.Fatal(r.Code)
	}
	var count int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id<>'root'`).Scan(&count)
	if count != 0 {
		t.Fatal("directory persisted without durable task")
	}
}

func TestRecoveryConfirmationRemainsBoundToPreviewAccount(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	addAccount(t, a, "b", "user-b")
	if err := a.Store.InsertNode(Node{ID: "saved", ParentID: "root", Name: "saved.txt", Kind: "file", Size: 12}); err != nil {
		t.Fatal(err)
	}
	h := a.Handler(nil)
	preview := request(t, h, "POST", "/api/v1/recovery/preview", map[string]any{"ids": []string{"saved"}}, "test-csrf")
	if preview.Code != 200 {
		t.Fatal(preview.Body.String())
	}
	var value struct {
		Account Account `json:"account"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Set("active_account", "b"); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"ids": []string{"saved"}, "account_id": value.Account.ID, "confirm": true}
	r := request(t, h, "POST", "/api/v1/recovery", body, "test-csrf")
	if r.Code != 409 {
		t.Fatalf("stale confirmation accepted: %d %s", r.Code, r.Body.String())
	}
	var count int
	if err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='recover'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("stale confirmation queued recovery in a different account")
	}
	body["account_id"] = "b"
	r = request(t, h, "POST", "/api/v1/recovery", body, "test-csrf")
	if r.Code != 202 {
		t.Fatalf("fresh confirmation rejected: %s", r.Body.String())
	}
}
func TestRecoveryPreviewReservesWholeSourceBatch(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	sessionForTest(t, a)
	nodes, _ := a.Store.AllNodes("a")
	var id string
	for _, n := range nodes {
		if n.Name == "one.mp4" {
			id = n.ID
		}
	}
	if err := a.Store.State("a", id, "missing"); err != nil {
		t.Fatal(err)
	}
	r := request(t, a.Handler(nil), "POST", "/api/v1/recovery/preview", map[string]any{"ids": []string{id}}, "test-csrf")
	var preview struct {
		Bytes         int64 `json:"bytes"`
		TransferBytes int64 `json:"transfer_bytes"`
	}
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if err := json.Unmarshal(r.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Bytes != 100 || preview.TransferBytes != 300 {
		t.Fatalf("partial selection did not reserve full batch: %+v", preview)
	}
}
func TestBackupSnapshotIncludesMatchingKeyAndLeavesRunningJob(t *testing.T) {
	a, _ := testApp(t)
	j, err := a.Store.NewJob("a", "import", "running", JobData{})
	if err != nil {
		t.Fatal(err)
	}
	j.State = "running"
	a.Store.SaveJob(&j, nil)
	backup := filepath.Join(t.TempDir(), "backup.zip")
	if err = a.Store.Backup(backup); err != nil {
		t.Fatal(err)
	}
	other, err := Open(a.Store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	other.DB.Close()
	current, _ := a.Store.Job(j.ID)
	if current.State != "running" {
		t.Fatal("read/backup open requeued a live job")
	}
	z, err := zip.OpenReader(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	target := t.TempDir()
	for _, f := range z.File {
		src, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(src)
		src.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(target, f.Name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.DB.Close()
	ac, err := restored.Account("a")
	if err != nil {
		t.Fatal(err)
	}
	var credentials pikpak.Credentials
	if err = restored.Unseal(ac.Secret, &credentials); err != nil {
		t.Fatal("backup credentials not decryptable", err)
	}
}
func TestLostKeyDoesNotCreateNewKey(t *testing.T) {
	a, _ := testApp(t)
	a.Store.DB.Close()
	if err := os.Remove(filepath.Join(a.Store.Dir, "master.key")); err != nil {
		t.Fatal(err)
	}
	_, err := Open(a.Store.Dir)
	if err == nil {
		t.Fatal("opened encrypted database without its key")
	}
	if _, err = os.Stat(filepath.Join(a.Store.Dir, "master.key")); !os.IsNotExist(err) {
		t.Fatal("silently regenerated key")
	}
}
func TestMediaRangeAndNoArbitraryURLs(t *testing.T) {
	a, f := testApp(t)
	sessionForTest(t, a)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=2-5" {
			t.Error("Range not forwarded")
		}
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(206)
		w.Write([]byte("2345"))
	}))
	defer upstream.Close()
	a.MediaHTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		r := req.Clone(req.Context())
		r.URL.Scheme = "https"
		r.URL.Host = strings.TrimPrefix(upstream.URL, "https://")
		return upstream.Client().Transport.RoundTrip(r)
	})}
	f.outputs = []RemoteEntry{{pikpak.File{Kind: "drive#file", Size: 10, Hash: "data", WebContentLink: "https://media.example.test/file"}, "test.mp4"}}
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	r := httptest.NewRequest("GET", "/api/v1/files/"+nodes[0].ID+"/content?proxy=1", nil)
	r.Header.Set("Range", "bytes=2-5")
	r.AddCookie(&http.Cookie{Name: "vault_session", Value: "test-session"})
	w := httptest.NewRecorder()
	a.Handler(nil).ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "2345" || w.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("broken range %d %s", w.Code, w.Body.String())
	}
	for _, u := range []string{"http://example.com/file", "https://127.0.0.1/x", "https://169.254.169.254/metadata", "https://[::1]/x", "https://user:pass@example.com/x"} {
		if safeMediaURL(u) == nil {
			t.Fatal("unsafe media accepted", u)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestHLSRewriteBindsSegmentsToAccountAndExpiry(t *testing.T) {
	a, _ := testApp(t)
	body, err := a.playlist("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:4,\nsegment.ts\n", "https://media.example.test/path/stream.m3u8", "node", "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "https://media.example") || !strings.Contains(body, "/api/v1/files/node/content?proxy=1&part=") {
		t.Fatal(body)
	}
	if _, err = a.playlist("#EXTM3U\nhttp://127.0.0.1/private\n", "https://media.example.test/", "node", "a", 0); err == nil {
		t.Fatal("unsafe segment accepted")
	}
}
func TestInitializationAndSessionPassword(t *testing.T) {
	a, _ := testApp(t)
	a.SetupToken = "bootstrap-secret"
	h := a.Handler(nil)
	r := request(t, h, "POST", "/api/v1/auth/setup", map[string]string{"token": "wrong", "password": "a-strong-password"}, "")
	if r.Code != 403 {
		t.Fatal(r.Code)
	}
	r = request(t, h, "POST", "/api/v1/auth/setup", map[string]string{"token": "bootstrap-secret", "password": "a-strong-password"}, "")
	if r.Code != 200 || len(r.Result().Cookies()) != 1 {
		t.Fatal(r.Code, r.Body.String())
	}
	cookie := r.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie insecure")
	}
	r = request(t, h, "POST", "/api/v1/auth/setup", map[string]string{"token": "bootstrap-secret", "password": "replacement-password"}, "")
	if r.Code != 409 {
		t.Fatal("setup remained open")
	}
	r = request(t, h, "POST", "/api/v1/auth/login", map[string]string{"password": "wrong"}, "")
	if r.Code != 401 {
		t.Fatal("incorrect password accepted")
	}
	r = request(t, h, "POST", "/api/v1/auth/login", map[string]string{"password": "a-strong-password"}, "")
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
}
func TestPauseDuringWriteSurvivesCheckpoint(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	f.onWrite = func() {
		if f.calls["offline"] == 0 && len(f.files) > 1 {
			a.jobMu.Lock()
			_, _ = a.Store.DB.Exec(`UPDATE jobs SET state='paused',message='Paused by test' WHERE id=?`, j.ID)
			a.jobMu.Unlock()
		}
	}
	a.Execute(ctx, &j)
	saved, _ := a.Store.Job(j.ID)
	if saved.State != "paused" {
		t.Fatalf("pause overwritten: %s", saved.State)
	}
}

func TestStaleClientCannotOverwriteUpdatedCredentials(t *testing.T) {
	a, _ := testApp(t)
	a.Factory = nil
	p, e := a.client("a")
	if e != nil {
		t.Fatal(e)
	}
	c := p.(*pikpak.Client)
	replacement, e := a.Store.Seal(pikpak.Credentials{RefreshToken: "new-administrator-token"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.Store.DB.Exec(`UPDATE accounts SET secret=? WHERE id='a'`, replacement)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Save(pikpak.Credentials{RefreshToken: "stale-refresh"}); e == nil {
		t.Fatal("stale client overwrote new credentials")
	}
	if unlock, e := c.BeforeWrite(); e == nil {
		unlock()
		t.Fatal("stale client could write remote data")
	}
	ac, _ := a.Store.Account("a")
	if ac.Secret != replacement {
		t.Fatal("replacement credentials lost")
	}
}

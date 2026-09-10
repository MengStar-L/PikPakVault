package vault

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
	"pikpakvault/internal/pikpak"
	"pikpakvault/internal/update"
)

type sessionKey struct{}
type session struct{ ID, CSRF string }
type httpError struct {
	Status  int
	Message string
}

func (e *httpError) Error() string      { return e.Message }
func fail(status int, msg string) error { return &httpError{status, msg} }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return fail(400, "Invalid request body")
	}
	if d.Decode(new(any)) != io.EOF {
		return fail(400, "Only one JSON value is allowed")
	}
	return nil
}
func (a *App) endpoint(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if e := fn(w, r); e != nil {
			status := 500
			msg := "The request could not be completed"
			var he *httpError
			var api *pikpak.APIError
			var at *attention
			if errors.As(e, &he) {
				status = he.Status
				msg = he.Message
			} else if errors.Is(e, sql.ErrNoRows) {
				status = 404
				msg = "Item not found"
			} else if errors.As(e, &at) {
				status = 409
				msg = at.message
			} else if errors.As(e, &api) {
				status = 502
				writeJSON(w, status, map[string]any{"error": api.Error(), "code": api.Code, "endpoint": api.Endpoint, "upstream_status": api.Status, "verification_url": api.VerificationURL})
				return
			} else if errors.Is(e, errPaused) {
				status = 409
				msg = e.Error()
			}
			writeJSON(w, status, map[string]string{"error": msg})
		}
	}
}
func passwordHash(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	h := argon2.IDKey([]byte(password), salt, 2, 32*1024, 2, 32)
	return base64.RawStdEncoding.EncodeToString(salt) + "." + base64.RawStdEncoding.EncodeToString(h)
}
func passwordOK(password, stored string) bool {
	parts := strings.Split(stored, ".")
	if len(parts) != 2 {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(parts[0])
	if e != nil {
		return false
	}
	expected, e := base64.RawStdEncoding.DecodeString(parts[1])
	if e != nil || len(expected) != 32 {
		return false
	}
	h := argon2.IDKey([]byte(password), salt, 2, 32*1024, 2, 32)
	return subtle.ConstantTimeCompare(h, expected) == 1
}
func tokenHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (a *App) Handler(assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if e := a.Store.DB.PingContext(ctx); e != nil {
			writeJSON(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, 200, map[string]any{"status": "ok", "version": Version, "pid": update.PIDString(), "update_token": os.Getenv("VAULT_UPDATE_TOKEN"), "ready": !updatePending()})
	})
	mux.HandleFunc("GET /api/v1/auth/status", a.endpoint(a.authStatus))
	mux.HandleFunc("POST /api/v1/auth/import/preview", a.endpoint(a.importBackupPreview))
	mux.HandleFunc("POST /api/v1/auth/import/commit", a.endpoint(a.importBackupCommit))
	var loginMu sync.Mutex
	var attempts int
	var reset time.Time
	mux.HandleFunc("POST /api/v1/auth/login", a.endpoint(func(w http.ResponseWriter, r *http.Request) error {
		loginMu.Lock()
		defer loginMu.Unlock()
		if time.Now().After(reset) {
			attempts = 0
			reset = time.Now().Add(time.Minute)
		}
		if attempts >= 10 {
			return fail(429, "Too many sign-in attempts; try again in a minute")
		}
		attempts++
		return a.login(w, r)
	}))
	mux.HandleFunc("POST /api/v1/auth/setup", a.endpoint(func(w http.ResponseWriter, r *http.Request) error {
		loginMu.Lock()
		defer loginMu.Unlock()
		return a.setup(w, r)
	}))
	private := http.NewServeMux()
	private.HandleFunc("GET /api/v1/teldrive", a.endpoint(a.telDriveList))
	private.HandleFunc("GET /api/v1/teldrive/cache", a.endpoint(a.telDriveCacheGet))
	private.HandleFunc("POST /api/v1/teldrive/cache/clear", a.endpoint(a.telDriveCacheClear))
	private.HandleFunc("POST /api/v1/teldrive", a.endpoint(a.telDriveSave))
	private.HandleFunc("PATCH /api/v1/teldrive/{id}", a.endpoint(a.telDriveSave))
	private.HandleFunc("POST /api/v1/teldrive/browse", a.endpoint(a.telDriveBrowse))
	private.HandleFunc("POST /api/v1/teldrive/{id}/sync", a.endpoint(a.telDriveSync))
	private.HandleFunc("POST /api/v1/auth/logout", a.endpoint(a.logout))
	private.HandleFunc("GET /api/v1/summary", a.endpoint(a.summary))
	private.HandleFunc("GET /api/v1/accounts", a.endpoint(a.accountsList))
	private.HandleFunc("POST /api/v1/accounts", a.endpoint(a.accountCreate))
	private.HandleFunc("PATCH /api/v1/accounts/{id}", a.endpoint(a.accountUpdate))
	private.HandleFunc("POST /api/v1/accounts/{id}/activate", a.endpoint(a.accountActivate))
	private.HandleFunc("POST /api/v1/accounts/{id}/verify", a.endpoint(a.accountVerify))
	private.HandleFunc("GET /api/v1/files", a.endpoint(a.filesList))
	private.HandleFunc("POST /api/v1/files", a.endpoint(a.folderCreate))
	private.HandleFunc("GET /api/v1/files/{id}", a.endpoint(a.fileDetail))
	private.HandleFunc("POST /api/v1/files/action", a.endpoint(a.filesAction))
	private.HandleFunc("PATCH /api/v1/files/{id}/position", a.endpoint(a.position))
	private.HandleFunc("POST /api/v1/imports/preview", a.endpoint(a.sharePreview))
	private.HandleFunc("POST /api/v1/imports", a.endpoint(a.importCreate))
	private.HandleFunc("PUT /api/v1/sources/{id}", a.endpoint(a.sourceUpdate))
	private.HandleFunc("GET /api/v1/jobs", a.endpoint(a.jobsList))
	private.HandleFunc("GET /api/v1/jobs/{id}", a.endpoint(a.jobDetail))
	private.HandleFunc("POST /api/v1/jobs/{id}/{action}", a.endpoint(a.jobAction))
	private.HandleFunc("POST /api/v1/scans", a.endpoint(a.scanCreate))
	private.HandleFunc("POST /api/v1/recovery/preview", a.endpoint(a.recoveryPreview))
	private.HandleFunc("POST /api/v1/recovery", a.endpoint(a.recoveryCreate))
	private.HandleFunc("GET /api/v1/settings", a.endpoint(a.settingsGet))
	private.HandleFunc("PATCH /api/v1/settings", a.endpoint(a.settingsUpdate))
	private.HandleFunc("GET /api/v1/export", a.endpoint(a.export))
	private.HandleFunc("POST /api/v1/backup", a.endpoint(a.backup))
	private.HandleFunc("POST /api/v1/data/import/preview", a.endpoint(a.importBackupPreview))
	private.HandleFunc("POST /api/v1/data/import/commit", a.endpoint(a.importBackupCommit))
	private.HandleFunc("POST /api/v1/data/resume", a.endpoint(a.resumeAfterImport))
	private.HandleFunc("GET /api/v1/updates", a.endpoint(a.updatesStatus))
	private.HandleFunc("POST /api/v1/updates/check", a.endpoint(a.updatesCheck))
	private.HandleFunc("POST /api/v1/updates/install", a.endpoint(a.updatesInstall))
	private.HandleFunc("GET /api/v1/events", a.events)
	private.HandleFunc("GET /api/v1/files/{id}/media", a.endpoint(a.media))
	private.HandleFunc("GET /api/v1/files/{id}/content", a.endpoint(a.content))
	private.HandleFunc("HEAD /api/v1/files/{id}/content", a.endpoint(a.content))
	private.HandleFunc("GET /api/v1/files/{id}/thumbnail", a.endpoint(a.thumbnail))
	mux.Handle("/api/v1/", a.authenticate(private))
	if assets != nil {
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" && r.Method != "HEAD" {
				http.Error(w, "method not allowed", 405)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, "/")
			if name == "" {
				name = "index.html"
			}
			if !fs.ValidPath(name) {
				http.NotFound(w, r)
				return
			}
			data, e := fs.ReadFile(assets, name)
			if e != nil {
				if strings.Contains(filepath.Base(name), ".") {
					http.NotFound(w, r)
					return
				}
				data, e = fs.ReadFile(assets, "index.html")
				name = "index.html"
			}
			if e != nil {
				http.Error(w, "Frontend is not built", 503)
				return
			}
			if name == "index.html" {
				w.Header().Set("Cache-Control", "no-cache")
			} else if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(data)))
		}))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SSE owns no long-lived database reference; it rechecks sessions each tick.
		if strings.HasSuffix(r.URL.Path, "/import/commit") || r.URL.Path == "/api/v1/teldrive/cache/clear" {
			if !a.dataMu.TryLock() {
				writeJSON(w, 409, map[string]string{"error": "有请求或任务正在执行，请暂停并等待结束后重试"})
				return
			}
			defer a.dataMu.Unlock()
		} else if r.URL.Path != "/api/v1/events" {
			a.dataMu.RLock()
			defer a.dataMu.RUnlock()
		}
		if updatePending() && r.Method != "GET" && r.Method != "HEAD" {
			writeJSON(w, 503, map[string]string{"error": "正在安装更新，请等待服务恢复"})
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https: data: blob:; media-src 'self' https: blob:; connect-src 'self' https:; font-src 'self' data:; frame-src 'self' https: blob:; worker-src 'self' blob:; object-src 'none'; base-uri 'self'; frame-ancestors 'self'")
		mux.ServeHTTP(w, r)
	})
}

var Version = "0.3.4"

func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		c, e := r.Cookie("vault_session")
		if e != nil {
			writeJSON(w, 401, map[string]string{"error": "Sign in to continue"})
			return
		}
		s := session{ID: tokenHash(c.Value)}
		var expires int64
		e = a.Store.DB.QueryRow(`SELECT csrf,expires FROM sessions WHERE id=?`, s.ID).Scan(&s.CSRF, &expires)
		if e != nil || expires < now() {
			writeJSON(w, 401, map[string]string{"error": "Session expired"})
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
				writeJSON(w, 403, map[string]string{"error": "Invalid CSRF token"})
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, e := url.Parse(origin)
				if e != nil || !strings.EqualFold(u.Host, r.Host) {
					writeJSON(w, 403, map[string]string{"error": "Cross-origin request denied"})
					return
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
	})
}
func (a *App) authStatus(w http.ResponseWriter, r *http.Request) error {
	v := map[string]any{"configured": a.Store.Get("password") != "", "authenticated": false}
	if cookie, e := r.Cookie("vault_session"); e == nil {
		var csrf string
		var expires int64
		if e = a.Store.DB.QueryRow(`SELECT csrf,expires FROM sessions WHERE id=?`, tokenHash(cookie.Value)).Scan(&csrf, &expires); e == nil && expires > now() {
			v["authenticated"] = true
			v["csrf"] = csrf
		}
	}
	writeJSON(w, 200, v)
	return nil
}
func (a *App) newSession(w http.ResponseWriter) error {
	token := ID() + ID()
	csrf := ID()
	_, e := a.Store.DB.Exec(`INSERT INTO sessions(id,csrf,expires) VALUES(?,?,?)`, tokenHash(token), csrf, now()+7*86400)
	if e != nil {
		return e
	}
	_, _ = a.Store.DB.Exec(`DELETE FROM sessions WHERE expires<?`, now())
	http.SetCookie(w, &http.Cookie{Name: "vault_session", Value: token, Path: "/", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 7 * 86400})
	writeJSON(w, 200, map[string]any{"authenticated": true, "csrf": csrf})
	return nil
}
func (a *App) setup(w http.ResponseWriter, r *http.Request) error {
	if a.Store.Get("password") != "" {
		return fail(409, "Administrator already configured")
	}
	var v struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if e := decode(r, &v); e != nil {
		return e
	}
	if a.SetupToken == "" || subtle.ConstantTimeCompare([]byte(v.Token), []byte(a.SetupToken)) != 1 {
		return fail(403, "Initialization code is incorrect")
	}
	if len(v.Password) < 10 || len(v.Password) > 256 {
		return fail(400, "Use a password between 10 and 256 characters")
	}
	if e := a.Store.Set("password", passwordHash(v.Password)); e != nil {
		return e
	}
	return a.newSession(w)
}
func (a *App) login(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Password string `json:"password"`
	}
	if e := decode(r, &v); e != nil {
		return e
	}
	if len(v.Password) > 256 || !passwordOK(v.Password, a.Store.Get("password")) {
		return fail(401, "Incorrect password")
	}
	return a.newSession(w)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) error {
	s := r.Context().Value(sessionKey{}).(session)
	_, e := a.Store.DB.Exec(`DELETE FROM sessions WHERE id=?`, s.ID)
	http.SetCookie(w, &http.Cookie{Name: "vault_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]bool{"ok": e == nil})
	return e
}
func (a *App) summary(w http.ResponseWriter, r *http.Request) error {
	var count, folders, bytes, missing, tasks int64
	_ = a.Store.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN kind='folder' THEN 1 ELSE 0 END),0),COALESCE(SUM(size),0) FROM nodes WHERE id<>'root' AND trashed=0`).Scan(&count, &folders, &bytes)
	account := a.active()
	if err := a.Store.DB.QueryRow(resourceJobsCTE+`SELECT COUNT(*) FROM nodes n LEFT JOIN bindings b ON b.node_id=n.id AND b.account_id=? WHERE n.id<>'root' AND n.trashed=0 AND `+recoveryNeededSQL, account, account).Scan(&missing); err != nil {
		return err
	}
	_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state IN ('queued','running','waiting','retry')`).Scan(&tasks)
	writeJSON(w, 200, map[string]any{"count": count, "folders": folders, "bytes": bytes, "missing": missing, "tasks": tasks, "active_account": a.active(), "last_scan": a.Store.Get("last_scan"), "version": Version})
	return nil
}
func (a *App) accountsList(w http.ResponseWriter, r *http.Request) error {
	accounts, e := a.Store.Accounts()
	if e != nil {
		return e
	}
	writeJSON(w, 200, map[string]any{"accounts": accounts, "active_id": a.active()})
	return nil
}

type accountInput struct {
	Name string `json:"name"`
	pikpak.Credentials
}

func (a *App) accountCreate(w http.ResponseWriter, r *http.Request) error {
	var v accountInput
	if e := decode(r, &v); e != nil {
		return e
	}
	if strings.TrimSpace(v.Name) == "" || len(v.Name) > 100 {
		return fail(400, "Account name is required")
	}
	if v.RefreshToken == "" && v.AccessToken == "" && (v.Username == "" || v.Password == "") {
		return fail(400, "Enter an account password or token")
	}
	id := ID()
	secret, e := a.Store.Seal(v.Credentials)
	if e != nil {
		return e
	}
	_, e = a.Store.DB.Exec(`INSERT INTO accounts(id,name,secret,created) VALUES(?,?,?,?)`, id, v.Name, secret, now())
	if e != nil {
		return e
	}
	a.gate.Lock()
	if a.active() == "" {
		e = a.Store.Set("active_account", id)
	}
	a.gate.Unlock()
	if e != nil {
		return e
	}
	j, e := a.Store.NewJob(id, "verify", "Verify account · "+v.Name, JobData{})
	if e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 202, map[string]any{"id": id, "job": j})
	return nil
}
func (a *App) accountUpdate(w http.ResponseWriter, r *http.Request) error {
	var v accountInput
	if e := decode(r, &v); e != nil {
		return e
	}
	id := r.PathValue("id")
	ac, e := a.Store.Account(id)
	if e != nil {
		return e
	}
	var previous pikpak.Credentials
	if e = a.Store.Unseal(ac.Secret, &previous); e != nil {
		return e
	}
	if v.Name == "" {
		v.Name = ac.Name
	}
	if v.Password == "" && v.RefreshToken == "" && v.AccessToken == "" && v.CaptchaToken == "" {
		_, e = a.Store.DB.Exec(`UPDATE accounts SET name=? WHERE id=?`, v.Name, id)
		if e != nil {
			return e
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	}
	if v.DeviceID == "" {
		v.DeviceID = previous.DeviceID
	}
	if v.CaptchaToken != "" && v.Password == "" && v.RefreshToken == "" && v.AccessToken == "" {
		previous.CaptchaToken = v.CaptchaToken
		previous.CaptchaExpiry = now() + 240
		v.Credentials = previous
	}
	v.UserID = ac.Identity
	c := pikpak.New(v.Credentials)
	me, e := c.Me(r.Context())
	if e != nil {
		return e
	}
	if ac.Identity != "" && me.Sub != ac.Identity {
		return fail(409, "These credentials belong to another account; add a new account instead")
	}
	secret, e := a.Store.Seal(c.Credentials)
	if e != nil {
		return e
	}
	a.gate.Lock()
	defer a.gate.Unlock()
	a.clientsMu.Lock()
	_, e = a.Store.DB.Exec(`UPDATE accounts SET name=?,secret=?,status='ready',error='',verification_url='' WHERE id=?`, v.Name, secret, id)
	delete(a.clients, id)
	a.clientsMu.Unlock()
	if e != nil {
		return e
	}
	a.Store.Event("account", id)
	writeJSON(w, 200, map[string]bool{"ok": true})
	return nil
}
func (a *App) accountActivate(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	ac, e := a.Store.Account(id)
	if e != nil {
		return e
	}
	if ac.Identity == "" || ac.Status != "ready" {
		return fail(409, "Verify this account before activating it")
	}
	a.gate.Lock()
	old := a.active()
	e = a.Store.Set("active_account", id)
	if e == nil && old != id {
		_, e = a.Store.DB.Exec(`UPDATE jobs SET state='paused',message='Account switched' WHERE account_id=? AND state IN ('queued','waiting','retry') AND kind<>'verify'`, old)
	}
	a.gate.Unlock()
	if e != nil {
		return e
	}
	j, e := a.Store.NewJob(id, "root", "调整专用目录 · "+a.rootPath(), JobData{RootPath: a.rootPath()})
	if e != nil {
		return e
	}
	a.Store.Event("account_switched", id)
	a.notify()
	writeJSON(w, 202, map[string]any{"job": j, "active_id": id})
	return nil
}
func (a *App) accountVerify(w http.ResponseWriter, r *http.Request) error {
	ac, e := a.Store.Account(r.PathValue("id"))
	if e != nil {
		return e
	}
	j, e := a.Store.NewJob(ac.ID, "verify", "Verify account · "+ac.Name, JobData{})
	if e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 202, j)
	return nil
}

func (a *App) filesList(w http.ResponseWriter, r *http.Request) error {
	account := a.active()
	previews := a.Store.Get("folder_previews") == "true" && r.URL.Query().Get("transfers") != "0"
	tx, e := a.Store.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	q := r.URL.Query()
	view := q.Get("view")
	parent := q.Get("parent")
	if parent == "" {
		parent = "root"
	}
	where := `n.id<>'root'`
	args := []any{account, account}
	if view == "trash" {
		where += ` AND n.trashed=1 AND NOT EXISTS(SELECT 1 FROM nodes p WHERE p.id=n.parent_id AND p.trashed=1)`
	} else {
		where += ` AND n.trashed=0`
	}
	switch view {
	case "favorites":
		where += ` AND n.favorite=1`
	case "recent":
		where += ` AND n.opened>0`
	case "video", "audio", "image":
		where += ` AND n.mime LIKE ?`
		args = append(args, view+"/%")
	case "documents":
		where += ` AND n.kind='file' AND n.mime NOT LIKE 'video/%' AND n.mime NOT LIKE 'audio/%' AND n.mime NOT LIKE 'image/%'`
	case "missing":
		where += ` AND ` + recoveryNeededSQL
	case "folders":
		where += ` AND n.kind='folder'`
	case "trash":
	default:
		if q.Get("search") == "" {
			where += ` AND n.parent_id=?`
			args = append(args, parent)
		}
	}
	if search := q.Get("search"); search != "" {
		where += ` AND instr(lower(n.name),lower(?))>0`
		args = append(args, search)
	}
	order := `n.kind DESC,n.name COLLATE NOCASE ASC,n.id`
	sortCol := map[string]string{"name": "n.name COLLATE NOCASE", "size": "n.size", "modified": "n.modified", "created": "n.created"}[q.Get("sort")]
	if sortCol != "" {
		direction := "ASC"
		if q.Get("direction") == "desc" {
			direction = "DESC"
		}
		order = "n.kind DESC," + sortCol + " " + direction + ",n.id"
	}
	if view == "recent" {
		order = "n.opened DESC,n.id"
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 0)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit == 0 {
		limit = 100
	}
	limit = min(max(limit, 1), 500)
	pending, transfers, e := fileTransfers(tx, account, parent, q)
	if e != nil {
		return e
	}
	if q.Get("transfers") == "0" {
		pending = nil
	}
	operations, e := nodeJobs(tx, account)
	if e != nil {
		return e
	}
	base := ` FROM nodes n LEFT JOIN bindings b ON b.node_id=n.id AND b.account_id=? WHERE ` + where
	var total int
	if e := tx.QueryRow(resourceJobsCTE+`SELECT COUNT(*)`+base, args...).Scan(&total); e != nil {
		return e
	}
	// Pin unfinished imports ahead of saved files while retaining one consistent
	// page size and total for virtual scrolling, including pages of only imports.
	start := min(page*limit, len(pending))
	end := min(page*limit+limit, len(pending))
	out := append([]Node{}, pending[start:end]...)
	queryArgs := append(append([]any{}, args...), limit-len(out), max(0, page*limit-len(pending)))
	rows, e := tx.Query(resourceJobsCTE+`SELECT `+nodeCols+base+` ORDER BY `+order+` LIMIT ? OFFSET ?`, queryArgs...)
	if e != nil {
		return e
	}
	for rows.Next() {
		n, e := nodeScan(rows)
		if e != nil {
			rows.Close()
			return e
		}
		if !n.Trashed {
			if operation, ok := operations[n.ID]; ok && operation.Kind != "recover" {
				if operation.Kind == "sync" {
					// Real folders remain navigable while their remote creation is
					// queued, so users can inspect transfers within the local tree.
					if n.State != "present" {
						n.State = operation.State
					}
				} else {
					n.Transfer = &operation.FileTransfer
					n.State = "transferring"
				}
			}
		}
		out = append(out, n)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	breadcrumbs := []map[string]string{}
	id := parent
	seen := map[string]bool{}
	for id != "root" && id != "" && !seen[id] {
		seen[id] = true
		n, e := nodeScan(tx.QueryRow(`SELECT `+nodeCols+` FROM nodes n LEFT JOIN bindings b ON b.node_id=n.id AND b.account_id=? WHERE n.id=?`, account, id))
		if e != nil {
			break
		}
		breadcrumbs = append([]map[string]string{{"id": n.ID, "name": n.Name}}, breadcrumbs...)
		id = n.ParentID
	}
	if previews {
		if e = attachFolderPreviews(tx, account, out); e != nil {
			return e
		}
	}
	writeJSON(w, 200, map[string]any{"files": out, "total": total + len(pending), "page": page, "limit": limit, "breadcrumbs": breadcrumbs, "transferring": len(transfers)})
	return nil
}
func enqueueTx(tx *sql.Tx, account, kind, title string, d JobData) (Job, error) {
	j := Job{ID: ID(), AccountID: account, Kind: kind, Title: title, State: "queued", Data: json.RawMessage(jsonText(d)), Created: now(), Updated: now()}
	if account == "" {
		j.State = "paused"
	}
	_, e := tx.Exec(`INSERT INTO jobs(id,account_id,kind,title,state,data,created,updated) VALUES(?,?,?,?,?,?,?,?)`, j.ID, account, kind, title, j.State, string(j.Data), j.Created, j.Updated)
	return j, e
}
func (a *App) target(id string) error {
	if id == "" {
		return fail(400, "Select a target folder")
	}
	n, e := a.Store.Node(id, a.active())
	if e != nil {
		return e
	}
	if n.Kind != "folder" || n.Trashed {
		return fail(400, "Target is not an active directory")
	}
	return nil
}
func (a *App) folderCreate(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Name     string `json:"name"`
		ParentID string `json:"parent_id"`
	}
	if e := decode(r, &v); e != nil {
		return e
	}
	if e := ValidName(v.Name); e != nil {
		return fail(400, e.Error())
	}
	if v.ParentID == "" {
		v.ParentID = "root"
	}
	if e := a.target(v.ParentID); e != nil {
		return e
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var count int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent_id=? AND name=? AND trashed=0`, v.ParentID, v.Name).Scan(&count)
	if count > 0 {
		return fail(409, "A file or folder with this name already exists")
	}
	id := ID()
	_, e = tx.Exec(`INSERT INTO nodes(id,parent_id,name,kind,created,modified) VALUES(?,?,?,'folder',?,?)`, id, v.ParentID, v.Name, now(), now())
	if e != nil {
		return e
	}
	j, e := enqueueTx(tx, a.activeFromTx(tx), "sync", "Create folder · "+v.Name, JobData{NodeIDs: []string{id}})
	if e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	a.Store.Event("files", id)
	a.notify()
	writeJSON(w, 201, map[string]any{"id": id, "job": j})
	return nil
}
func (a *App) activeFromTx(tx *sql.Tx) string {
	var id string
	_ = tx.QueryRow(`SELECT value FROM settings WHERE key='active_account'`).Scan(&id)
	return id
}
func (a *App) fileDetail(w http.ResponseWriter, r *http.Request) error {
	n, e := a.Store.Node(r.PathValue("id"), a.active())
	if e != nil {
		return e
	}
	p, e := a.Store.Path(n.ID)
	if e != nil {
		return e
	}
	var src any
	if n.SourceID != "" {
		s, e := a.Store.Source(n.SourceID)
		if e == nil {
			src = s
		}
	}
	writeJSON(w, 200, map[string]any{"file": n, "path": p, "source": src})
	return nil
}
func (a *App) filesAction(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		IDs      []string `json:"ids"`
		Action   string   `json:"action"`
		Name     string   `json:"name"`
		ParentID string   `json:"parent_id"`
		Favorite bool     `json:"favorite"`
		Confirm  bool     `json:"confirm"`
	}
	if e := decode(r, &v); e != nil {
		return e
	}
	if len(v.IDs) == 0 || len(v.IDs) > 500 {
		return fail(400, "Select between 1 and 500 items")
	}
	if contains(v.IDs, "root") {
		return fail(400, "Cannot modify the library root")
	}
	selected := map[string]bool{}
	for _, id := range v.IDs {
		if _, e := a.Store.Node(id, a.active()); e != nil {
			return e
		}
		selected[id] = true
	}
	nodes, e := a.Store.Descendants(v.IDs, a.active())
	if e != nil {
		return e
	}
	if v.Action == "rename" {
		if len(v.IDs) != 1 {
			return fail(400, "Rename one item at a time")
		}
		if e = ValidName(v.Name); e != nil {
			return fail(400, e.Error())
		}
	}
	if v.Action == "move" {
		if e = a.target(v.ParentID); e != nil {
			return e
		}
		for _, n := range nodes {
			if n.ID == v.ParentID {
				return fail(400, "Cannot move a directory into itself")
			}
		}
	}
	if v.Action == "purge" {
		if !v.Confirm {
			return fail(400, "Confirm permanent removal of backup records")
		}
		var busy int
		_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state IN ('queued','running','waiting','retry','paused','attention')`).Scan(&busy)
		if busy > 0 {
			return fail(409, "Finish or cancel outstanding tasks before permanently removing records")
		}
		for _, n := range nodes {
			if !n.Trashed {
				return fail(400, "Only recycle-bin records can be permanently removed")
			}
		}
	}
	a.gate.RLock()
	defer a.gate.RUnlock()
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	account := a.activeFromTx(tx)
	for _, n := range nodes {
		switch v.Action {
		case "favorite":
			if selected[n.ID] {
				_, e = tx.Exec(`UPDATE nodes SET favorite=?,modified=? WHERE id=?`, v.Favorite, now(), n.ID)
			}
		case "rename", "move":
			if selected[n.ID] {
				p, name := n.ParentID, n.Name
				if v.Action == "rename" {
					name = v.Name
				} else {
					p = v.ParentID
				}
				var collision int
				_ = tx.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent_id=? AND name=? AND id<>? AND trashed=0`, p, name, n.ID).Scan(&collision)
				if collision > 0 {
					return fail(409, "Destination already contains this name")
				}
				_, e = tx.Exec(`UPDATE nodes SET parent_id=?,name=?,modified=?,revision=revision+1 WHERE id=?`, p, name, now(), n.ID)
				if e == nil {
					_, e = tx.Exec(`UPDATE bindings SET state='drift' WHERE node_id=? AND state='present'`, n.ID)
				}
			}
		case "trash", "restore":
			trashed := v.Action == "trash"
			_, e = tx.Exec(`UPDATE nodes SET trashed=?,modified=?,revision=revision+1 WHERE id=?`, trashed, now(), n.ID)
		case "purge":
			_, e = tx.Exec(`DELETE FROM nodes WHERE id=?`, n.ID)
		default:
			return fail(400, "Unknown file action")
		}
		if e != nil {
			return e
		}
	}
	var j Job
	if v.Action != "favorite" && v.Action != "purge" {
		kind := "sync"
		if v.Action == "trash" {
			kind = "trash"
		}
		if v.Action == "restore" {
			kind = "recover"
		}
		j, e = enqueueTx(tx, account, kind, v.Action+" · "+strconv.Itoa(len(v.IDs))+" items", JobData{NodeIDs: v.IDs})
		if e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	a.Store.Event("files", map[string]any{"action": v.Action, "ids": v.IDs})
	a.notify()
	writeJSON(w, 200, map[string]any{"ok": true, "job": j})
	return nil
}
func (a *App) position(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Position float64 `json:"position"`
	}
	if e := decode(r, &v); e != nil {
		return e
	}
	if math.IsNaN(v.Position) || math.IsInf(v.Position, 0) || v.Position < 0 || v.Position > 1e8 {
		return fail(400, "Invalid playback position")
	}
	_, e := a.Store.DB.Exec(`UPDATE nodes SET position=?,opened=? WHERE id=? AND trashed=0`, v.Position, now(), r.PathValue("id"))
	if e != nil {
		return e
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
	return nil
}

type importInput struct {
	Preview  []Entry  `json:"preview,omitempty"`
	Link     string   `json:"link"`
	PassCode string   `json:"pass_code"`
	ParentID string   `json:"parent_id"`
	Selected []string `json:"selected"`
}

func (a *App) sharePreview(w http.ResponseWriter, r *http.Request) error {
	var v importInput
	if e := decode(r, &v); e != nil {
		return e
	}
	s, e := ParseSource(v.Link, v.PassCode)
	if e != nil {
		return fail(400, e.Error())
	}
	if s.Kind != "share" {
		return fail(400, "Preview is available for PikPak shares")
	}
	c, e := a.client(a.active())
	if e != nil {
		return e
	}
	entries, _, e := shareTree(r.Context(), c, s, v.PassCode)
	if e != nil {
		return e
	}
	writeJSON(w, 200, map[string]any{"entries": entries, "source": s})
	return nil
}
func (a *App) importCreate(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Items []importInput `json:"items"`
	}
	if e := decode(r, &v); e != nil {
		return e
	}
	if len(v.Items) == 0 || len(v.Items) > 100 {
		return fail(400, "Add between 1 and 100 links")
	}
	a.gate.RLock()
	defer a.gate.RUnlock()
	account := a.active()
	if account == "" {
		return fail(409, "Connect a PikPak account first")
	}
	ac, e := a.Store.Account(account)
	if e != nil {
		return e
	}
	if ac.Status != "ready" {
		return fail(409, "Verify the active account first")
	}
	sources := []Source{}
	for i := range v.Items {
		item := &v.Items[i]
		if item.ParentID == "" {
			item.ParentID = "root"
		}
		if e = a.target(item.ParentID); e != nil {
			return e
		}
		s, e := ParseSource(item.Link, item.PassCode)
		if e != nil {
			return fail(400, e.Error())
		}
		s.Selected = item.Selected
		s.Secret, e = a.Store.Seal(item.PassCode)
		if e != nil {
			return e
		}
		sources = append(sources, s)
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	jobs := []Job{}
	for i, s := range sources {
		_, e = tx.Exec(`INSERT INTO sources(id,kind,link,share_id,share_parent,secret,selected,manifest,created) VALUES(?,?,?,?,?,?,?,?,?)`, s.ID, s.Kind, s.Link, s.ShareID, s.ShareParent, s.Secret, jsonText(s.Selected), "[]", s.Created)
		if e != nil {
			return e
		}
		title := "Save PikPak share"
		if s.Kind == "magnet" {
			title = "Save magnet link"
			u, _ := url.Parse(s.Link)
			if name := u.Query().Get("dn"); name != "" {
				title = name
			}
		}
		preview := []Entry{}
		for _, entry := range v.Items[i].Preview {
			if s.Kind == "share" && contains(s.Selected, entry.ID) && entry.Name != "" && !strings.ContainsAny(entry.Name, "/\\") && (entry.Kind == "folder" || entry.Kind == "file") {
				preview = append(preview, Entry{ID: entry.ID, Name: entry.Name, Path: entry.Name, Kind: entry.Kind, Size: max(0, entry.Size)})
			}
		}
		j, e := enqueueTx(tx, account, "import", title, JobData{SourceID: s.ID, ParentID: v.Items[i].ParentID, Preview: preview})
		if e != nil {
			return e
		}
		jobs = append(jobs, j)
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 202, map[string]any{"jobs": jobs})
	return nil
}
func (a *App) sourceUpdate(w http.ResponseWriter, r *http.Request) error {
	var v importInput
	if e := decode(r, &v); e != nil {
		return e
	}
	s, e := a.Store.Source(r.PathValue("id"))
	if e != nil {
		return e
	}
	if s.Kind == "teldrive" {
		return fail(400, "请在 TelDrive 同步中更新连接认证；文件来源由监控记录管理")
	}
	newSource, e := ParseSource(v.Link, v.PassCode)
	if e != nil {
		return fail(400, e.Error())
	}
	newSource.Secret, e = a.Store.Seal(v.PassCode)
	if e != nil {
		return e
	}
	newSource.Selected = v.Selected
	newSource.ID = s.ID
	newSource.Manifest = s.Manifest
	newSource.Created = s.Created
	var busy int
	_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state IN ('queued','running','waiting','retry')`).Scan(&busy)
	if busy > 0 {
		return fail(409, "Pause running tasks before replacing a source")
	}
	_, e = a.Store.DB.Exec(`UPDATE sources SET kind=?,link=?,share_id=?,share_parent=?,secret=?,selected=? WHERE id=?`, newSource.Kind, newSource.Link, newSource.ShareID, newSource.ShareParent, newSource.Secret, jsonText(newSource.Selected), s.ID)
	if e != nil {
		return e
	}
	a.Store.Event("source_replaced", s.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
	return nil
}
func (a *App) jobsList(w http.ResponseWriter, r *http.Request) error {
	jobs, e := a.Store.Jobs()
	if e != nil {
		return e
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
	return nil
}
func (a *App) jobDetail(w http.ResponseWriter, r *http.Request) error {
	j, e := a.Store.Job(r.PathValue("id"))
	if e != nil {
		return e
	}
	var d JobData
	_ = json.Unmarshal(j.Data, &d)
	writeJSON(w, 200, map[string]any{"job": j, "problems": d.Problems, "transfers": d.Transfers, "cleanup_results": d.CleanupResults})
	return nil
}
func (a *App) jobAction(w http.ResponseWriter, r *http.Request) error {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	j, e := a.Store.Job(r.PathValue("id"))
	if e != nil {
		return e
	}
	action := r.PathValue("action")
	var d JobData
	if e = json.Unmarshal(j.Data, &d); e != nil {
		return e
	}
	d.init()
	switch action {
	case "pause":
		j.State = "paused"
		j.Message = "Paused by administrator"
	case "cancel":
		j.State = "cancelled"
		j.Message = "Cancelled; saved sources and files are retained"
	case "retry":
		if j.State == "running" {
			return fail(409, "Task is already running")
		}
		if j.AccountID != a.active() && j.Kind != "verify" {
			return fail(409, "Switch to this task's account first")
		}
		if j.Kind != "verify" {
			account, err := a.Store.Account(j.AccountID)
			if err != nil {
				return err
			}
			if account.Status != "ready" {
				return fail(409, "该账号尚未就绪，请在账号管理中完成验证后重试")
			}
		}
		if (j.State == "cancelled" || j.State == "completed") && (j.Kind == "teldrive_upload" || j.Kind == "recover" || (j.Kind == "sync" && d.MonitorID != "")) {
			operations, err := nodeJobs(a.Store.DB, j.AccountID)
			if err != nil {
				return err
			}
			nodes, err := a.Store.Descendants(d.NodeIDs, j.AccountID)
			if err != nil {
				return err
			}
			for _, n := range nodes {
				if operation, ok := operations[n.ID]; ok && operation.JobID != j.ID {
					return fail(409, "资源已由其他传输或恢复任务接管，请继续该任务，避免重复上传")
				}
			}
		}
		j.State = "queued"
		j.Message = "已收到重试请求，等待继续原任务"
		if j.Kind == "import" {
			j.Message = "已收到重试请求，将重新核对云端保存结果"
		}
		j.Attempts = 0
		j.NextRun = 0
		d.Problems = map[string]string{}
		for _, transfer := range d.Transfers {
			transfer.Polls = 0
		}
	case "associate":
		if j.AccountID != a.active() {
			return fail(409, "请先切换到该任务所属账号")
		}
		if j.State == "completed" {
			return fail(409, "已完成任务无需重新关联")
		}
		if j.State == "running" {
			return fail(409, "Pause the task before associating results")
		}
		var v struct {
			SourceID  string   `json:"source_id"`
			RemoteIDs []string `json:"remote_ids"`
		}
		if e = decode(r, &v); e != nil {
			return e
		}
		if len(v.RemoteIDs) == 0 || len(v.RemoteIDs) > 100 {
			return fail(400, "Enter between 1 and 100 result IDs")
		}
		if v.SourceID == "" {
			v.SourceID = d.SourceID
		}
		t := d.Transfers[v.SourceID]
		if t == nil {
			return fail(400, "Transfer not found")
		}
		t.OutputIDs = v.RemoteIDs
		t.Fingerprint, t.StableSince = "", 0
		t.Entries = nil
		t.Phase = "submitted"
		j.State = "queued"
		j.NextRun = 0
	default:
		return fail(400, "Unknown task action")
	}
	if e = a.Store.SaveJob(&j, &d); e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 200, j)
	return nil
}
func (a *App) scanCreate(w http.ResponseWriter, r *http.Request) error {
	if a.active() == "" {
		return fail(409, "Connect an account first")
	}
	j, e := a.Store.NewJob(a.active(), "scan", "Check library availability", JobData{})
	if e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 202, j)
	return nil
}

type recoveryInput struct {
	AccountID string   `json:"account_id"`
	IDs       []string `json:"ids"`
	Confirm   bool     `json:"confirm"`
}

func (a *App) recoveryPreview(w http.ResponseWriter, r *http.Request) error {
	var v recoveryInput
	if e := decode(r, &v); e != nil {
		return e
	}
	a.gate.RLock()
	defer a.gate.RUnlock()
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	account := a.active()
	if account == "" {
		return fail(409, "Connect an account first")
	}
	nodes, e := a.Store.Descendants(v.IDs, account)
	if e != nil {
		return e
	}
	ac, e := a.Store.Account(account)
	if e != nil {
		return e
	}
	operations, e := nodeJobs(a.Store.DB, account)
	if e != nil {
		return e
	}
	skipped := 0
	var total int64
	sourceSizes := map[string]int64{}
	var standaloneBytes int64
	items := []map[string]any{}
	for _, n := range nodes {
		if n.Trashed || n.State == "present" {
			continue
		}
		if _, ok := operations[n.ID]; ok {
			skipped++
			continue
		}
		method := "source"
		if n.Kind == "folder" {
			method = "directory"
		} else if n.RemoteID != "" {
			method = "verify_or_untrash"
		} else if n.Hash != "" {
			method = "instant_or_source"
		}
		p, _ := a.Store.Path(n.ID)
		if n.Kind != "folder" {
			total += n.Size
			if n.SourceID == "" {
				standaloneBytes += n.Size
			} else {
				sourceSizes[n.SourceID] += n.Size
			}
		}
		items = append(items, map[string]any{"id": n.ID, "name": n.Name, "path": p, "state": n.State, "size": n.Size, "method": method, "has_source": n.SourceID != "", "conflict": n.State == "conflict"})
	}
	transferBytes := standaloneBytes
	for sourceID, selectedSize := range sourceSizes {
		source, err := a.Store.Source(sourceID)
		if err != nil {
			return err
		}
		var batchSize int64
		for _, entry := range source.Manifest {
			if entry.Kind != "folder" {
				batchSize += entry.Size
			}
		}
		transferBytes += max(selectedSize, batchSize)
	}
	writeJSON(w, 200, map[string]any{"account": ac, "items": items, "skipped_pending": skipped, "bytes": total, "transfer_bytes": transferBytes, "available": max(int64(0), ac.Limit-ac.Used), "capacity_warning": ac.Limit > 0 && transferBytes > ac.Limit-ac.Used})
	return nil
}
func (a *App) recoveryCreate(w http.ResponseWriter, r *http.Request) error {
	var v recoveryInput
	if e := decode(r, &v); e != nil {
		return e
	}
	if !v.Confirm {
		return fail(400, "Review and confirm the recovery preview")
	}
	a.gate.RLock()
	defer a.gate.RUnlock()
	// Serialize reservation checks with TelDrive registration and task controls.
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	account := a.active()
	if account == "" {
		return fail(409, "Connect an account first")
	}
	if v.AccountID == "" || v.AccountID != account {
		return fail(409, "The active account changed; preview and confirm recovery again")
	}
	nodes, e := a.Store.Descendants(v.IDs, account)
	if e != nil {
		return e
	}
	ids := []string{}
	operations, e := nodeJobs(a.Store.DB, account)
	if e != nil {
		return e
	}
	skipped := 0
	for _, n := range nodes {
		if !n.Trashed && n.State != "present" {
			if _, ok := operations[n.ID]; ok {
				skipped++
				continue
			}
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		if skipped > 0 {
			return fail(409, "所选资源已有传输或恢复任务，请在传输任务中继续或重试原任务；取消后才能另行恢复")
		}
		return fail(409, "No selected items need recovery")
	}
	j, e := a.Store.NewJob(account, "recover", "Restore library · "+strconv.Itoa(len(ids))+" items", JobData{NodeIDs: ids})
	if e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 202, j)
	return nil
}
func (a *App) settingsGet(w http.ResponseWriter, r *http.Request) error {
	minutes, _ := strconv.Atoi(a.Store.Get("scan_minutes"))
	account := a.active()
	var rootJob *Job
	if job, err := jobScan(a.Store.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE account_id=? AND kind='root' AND json_extract(data,'$.root_path')=? ORDER BY created DESC,rowid DESC LIMIT 1`, account, a.rootPath())); err == nil {
		rootJob = &job
	}
	writeJSON(w, 200, map[string]any{"import_review_required": a.Store.Get("import_review_required") == "true", "scan_minutes": minutes, "proxy_default": a.Store.Get("proxy_default") == "true", "folder_previews": a.Store.Get("folder_previews") == "true", "instance": a.Store.Get("instance"), "version": Version, "root_path": a.rootPath(), "root_path_applied": a.Store.Get("root_path:" + account), "root_job": rootJob, "active_account": account})
	return nil
}
func (a *App) settingsUpdate(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		ScanMinutes     int     `json:"scan_minutes"`
		ProxyDefault    bool    `json:"proxy_default"`
		FolderPreviews  *bool   `json:"folder_previews"`
		CurrentPassword string  `json:"current_password"`
		NewPassword     string  `json:"new_password"`
		RootPath        *string `json:"root_path"`
	}
	if e := decode(r, &v); e != nil {
		return e
	}
	if v.ScanMinutes < 0 || v.ScanMinutes > 10080 || (v.ScanMinutes > 0 && v.ScanMinutes < 5) {
		return fail(400, "Scan interval must be 0 or between 5 and 10080 minutes")
	}
	desired := ""
	if v.RootPath != nil {
		var err error
		desired, err = normalizeRootPath(*v.RootPath)
		if err != nil {
			return fail(400, err.Error())
		}
	}
	newPassword := ""
	if v.NewPassword != "" {
		if !passwordOK(v.CurrentPassword, a.Store.Get("password")) {
			return fail(403, "Current password is incorrect")
		}
		if len(v.NewPassword) < 10 || len(v.NewPassword) > 256 {
			return fail(400, "Password must have 10–256 characters")
		}
		newPassword = passwordHash(v.NewPassword)
	}
	a.gate.Lock()
	defer a.gate.Unlock()
	account := a.active()
	needsMove := v.RootPath != nil && (desired != a.rootPath() || a.Store.Get("root_path:"+account) != desired)
	ac, _ := a.Store.Account(account)
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	values := map[string]string{"scan_minutes": strconv.Itoa(v.ScanMinutes), "proxy_default": strconv.FormatBool(v.ProxyDefault)}
	if v.FolderPreviews != nil {
		values["folder_previews"] = strconv.FormatBool(*v.FolderPreviews)
	}
	if v.RootPath != nil {
		values["root_path"] = desired
	}
	if newPassword != "" {
		values["password"] = newPassword
	}
	for key, value := range values {
		if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	if newPassword != "" {
		current := r.Context().Value(sessionKey{}).(session)
		if _, err = tx.Exec(`DELETE FROM sessions WHERE id<>?`, current.ID); err != nil {
			return err
		}
	}
	var job *Job
	if needsMove && ac.Identity != "" && ac.Status == "ready" {
		value, e := enqueueTx(tx, account, "root", "调整专用目录 · "+desired, JobData{RootPath: desired})
		if e != nil {
			return e
		}
		job = &value
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.Store.Event("settings", true)
	a.notify()
	writeJSON(w, 200, map[string]any{"ok": true, "job": job})
	return nil
}
func (a *App) export(w http.ResponseWriter, r *http.Request) error {
	nodes, e := a.Store.AllNodes(a.active())
	if e != nil {
		return e
	}
	sources := map[string]Source{}
	for _, n := range nodes {
		if n.SourceID != "" {
			if _, ok := sources[n.SourceID]; !ok {
				s, e := a.Store.Source(n.SourceID)
				if e != nil {
					return e
				}
				sources[n.SourceID] = s
			}
		}
	}
	w.Header().Set("Content-Disposition", `attachment; filename="pikpak-vault-sources.json"`)
	writeJSON(w, 200, map[string]any{"version": 1, "exported": now(), "nodes": nodes, "sources": sources})
	return nil
}
func (s *Store) Backup(filename string) error {
	temp, e := os.CreateTemp(s.Dir, "snapshot-*.db")
	if e != nil {
		return e
	}
	snapshot := temp.Name()
	temp.Close()
	os.Remove(snapshot)
	defer os.Remove(snapshot)
	if _, e = s.DB.Exec(`VACUUM INTO ?`, snapshot); e != nil {
		return e
	}
	f, e := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	z := zip.NewWriter(f)
	for _, item := range []struct{ name, path string }{{"vault.db", snapshot}, {"master.key", filepath.Join(s.Dir, "master.key")}} {
		entry, err := z.Create(item.name)
		if err != nil {
			e = err
			break
		}
		src, err := os.Open(item.path)
		if err != nil {
			e = err
			break
		}
		_, e = io.Copy(entry, src)
		src.Close()
		if e != nil {
			break
		}
	}
	if closeErr := z.Close(); e == nil {
		e = closeErr
	}
	if e == nil {
		e = f.Sync()
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		os.Remove(filename)
	}
	return e
}
func (a *App) backup(w http.ResponseWriter, r *http.Request) error {
	name := filepath.Join(a.Store.Dir, "download-"+ID()+".zip")
	if e := a.Store.Backup(name); e != nil {
		return e
	}
	defer os.Remove(name)
	w.Header().Set("Content-Disposition", `attachment; filename="pikpak-vault-backup.zip"`)
	http.ServeFile(w, r, name)
	return nil
}
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	f, ok := w.(http.Flusher)
	if !ok {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	last := ""
	for {
		a.dataMu.RLock()
		var valid int
		identity := r.Context().Value(sessionKey{}).(session)
		_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id=? AND expires>?`, identity.ID, now()).Scan(&valid)
		if valid != 1 {
			a.dataMu.RUnlock()
			fmt.Fprint(w, "event: unauthorized\ndata: {}\n\n")
			f.Flush()
			return
		}
		var revision int64
		_ = a.Store.DB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM events`).Scan(&revision)
		var jobUpdate int64
		_ = a.Store.DB.QueryRow(`SELECT COALESCE(MAX(updated),0) FROM jobs`).Scan(&jobUpdate)
		current := fmt.Sprintf("%d-%d-%s", revision, jobUpdate, a.active())
		a.dataMu.RUnlock()
		if current != last {
			fmt.Fprintf(w, "event: change\ndata: %s\n\n", jsonText(map[string]any{"revision": current}))
			last = current
		} else {
			fmt.Fprint(w, ": keepalive\n\n")
		}
		f.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// Stable, root-scoped media entry points never accept an arbitrary upstream URL.
func (a *App) remoteFile(ctx context.Context, id string) (Node, pikpak.File, error) {
	n, e := a.Store.Node(id, a.active())
	if e != nil {
		return n, pikpak.File{}, e
	}
	if n.Trashed || n.Kind == "folder" {
		return n, pikpak.File{}, fail(404, "File is not available")
	}
	if n.RemoteID == "" {
		return n, pikpak.File{}, fail(409, "Restore this file to the active account first")
	}
	c, e := a.client(a.active())
	if e != nil {
		return n, pikpak.File{}, e
	}
	f, e := c.Get(ctx, n.RemoteID)
	if e != nil {
		return n, f, e
	}
	if f.Trashed || !compatible(n, f) {
		return n, f, fail(409, "The remote file is missing or has changed")
	}
	return n, f, nil
}
func mediaURL(f pikpak.File, id string) string {
	if id != "" {
		for _, m := range f.Medias {
			if m.ID == id {
				return m.Link.URL
			}
		}
		return ""
	}
	if f.WebContentLink != "" {
		return f.WebContentLink
	}
	if l, ok := f.Links["application/octet-stream"]; ok {
		return l.URL
	}
	for _, m := range f.Medias {
		if m.Original && m.Link.URL != "" {
			return m.Link.URL
		}
	}
	return ""
}
func (a *App) media(w http.ResponseWriter, r *http.Request) error {
	n, f, e := a.remoteFile(r.Context(), r.PathValue("id"))
	if e != nil {
		return e
	}
	_, _ = a.Store.DB.Exec(`UPDATE nodes SET opened=? WHERE id=?`, now(), n.ID)
	options := []map[string]string{}
	if u := mediaURL(f, ""); u != "" {
		options = append(options, map[string]string{"id": "", "label": "原始文件", "url": u})
	}
	for _, m := range f.Medias {
		if m.Link.URL != "" {
			label := m.Resolution
			if label == "" {
				label = m.Name
			}
			options = append(options, map[string]string{"id": m.ID, "label": label, "url": m.Link.URL})
		}
	}
	writeJSON(w, 200, map[string]any{"file": n, "options": options, "proxy_default": a.Store.Get("proxy_default") == "true"})
	return nil
}

func sortStrings(xs []string) { sort.Strings(xs) }

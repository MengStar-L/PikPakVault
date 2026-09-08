package vault

import (
	"archive/zip"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pikpakvault/internal/pikpak"
)

const maxBackupUpload = 512 << 20

var importTables = []string{"settings", "accounts", "sources", "nodes", "bindings", "jobs", "events"}

type BackupPreview struct {
	ID       string `json:"id"`
	Accounts int    `json:"accounts"`
	Files    int    `json:"files"`
	Sources  int    `json:"sources"`
	Jobs     int    `json:"jobs"`
	Owner    string `json:"-"`
}

// ExtractBackup only accepts the two original backup members. Never open an
// uploaded database with Open(), which would run migrations before validation.
func ExtractBackup(archive, dir string) (*Store, error) {
	// SQLite file URIs need an absolute path, including when serve uses ./data.
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	dir = abs
	z, e := zip.OpenReader(archive)
	if e != nil {
		return nil, fmt.Errorf("无法读取 ZIP 备份")
	}
	defer z.Close()
	seen := map[string]bool{}
	for _, f := range z.File {
		if (f.Name != "vault.db" && f.Name != "master.key") || seen[f.Name] || !f.Mode().IsRegular() {
			return nil, fmt.Errorf("备份只能包含 vault.db 与 master.key，不能含重复文件或符号链接")
		}
		seen[f.Name] = true
		limit := int64(2 << 30)
		if f.Name == "master.key" {
			limit = 32
		}
		if f.UncompressedSize64 > uint64(limit) {
			return nil, fmt.Errorf("备份解压大小超出限制")
		}
		src, e := f.Open()
		if e != nil {
			return nil, e
		}
		dst, e := os.OpenFile(filepath.Join(dir, f.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			src.Close()
			return nil, e
		}
		n, e := io.Copy(dst, io.LimitReader(src, limit+1))
		closeErr := dst.Close()
		src.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if n > limit {
			return nil, fmt.Errorf("备份过大")
		}
	}
	if !seen["vault.db"] || !seen["master.key"] {
		return nil, fmt.Errorf("缺少数据库或配套密钥")
	}
	key, e := os.ReadFile(filepath.Join(dir, "master.key"))
	if e != nil || len(key) != 32 {
		return nil, fmt.Errorf("备份密钥长度不正确")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(dir, "vault.db")), RawQuery: "mode=ro&immutable=1"}
	if filepath.VolumeName(filepath.Join(dir, "vault.db")) != "" {
		u.Path = "/" + u.Path
	}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Dir: dir, key: key}
	if e = validateBackup(s); e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}

func validateBackup(s *Store) error {
	if _, e := s.DB.Exec(`PRAGMA trusted_schema=OFF`); e != nil {
		return e
	}
	var version int
	var integrity string
	if e := s.DB.QueryRow(`PRAGMA user_version`).Scan(&version); e != nil || version != 1 {
		return fmt.Errorf("不支持该备份数据库版本，请先升级程序")
	}
	if e := s.DB.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); e != nil || integrity != "ok" {
		return fmt.Errorf("备份数据库完整性检查失败")
	}
	var unsafe int
	if e := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('trigger','view') OR sql LIKE '%VIRTUAL TABLE%'`).Scan(&unsafe); e != nil || unsafe != 0 {
		return fmt.Errorf("备份含不支持的数据库对象")
	}
	rows, e := s.DB.Query(`PRAGMA foreign_key_check`)
	if e != nil {
		return e
	}
	bad := rows.Next()
	rows.Close()
	if bad {
		return fmt.Errorf("备份数据引用关系不完整")
	}
	for _, table := range append(append([]string{}, importTables...), "sessions") {
		var count int
		if e = s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); e != nil {
			return fmt.Errorf("备份缺少必要的数据表：%s", table)
		}
	}
	password := s.Get("password")
	if password != "" {
		parts := strings.Split(password, ".")
		if len(parts) != 2 || len(password) > 256 {
			return fmt.Errorf("备份中没有有效的管理员密码，请先完成原实例初始化")
		}
		salt, se := base64.RawStdEncoding.DecodeString(parts[0])
		hash, he := base64.RawStdEncoding.DecodeString(parts[1])
		if se != nil || he != nil || len(salt) != 16 || len(hash) != 32 {
			return fmt.Errorf("备份中的密码哈希无效")
		}
	}
	var root int
	if e = s.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id='root' AND kind='folder' AND parent_id=''`).Scan(&root); e != nil || root != 1 {
		return fmt.Errorf("备份的根目录记录无效")
	}
	for _, table := range []string{"accounts", "sources"} {
		rows, e := s.DB.Query(`SELECT secret FROM ` + table)
		if e != nil {
			return e
		}
		for rows.Next() {
			var secret string
			if e = rows.Scan(&secret); e != nil {
				break
			}
			var value json.RawMessage
			e = s.Unseal(secret, &value)
			if e != nil {
				break
			}
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return fmt.Errorf("备份密钥无法解密 %s 中的认证信息", table)
		}
	}
	return nil
}

func (a *App) importOwner(r *http.Request) (string, error) {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
		s, ok := r.Context().Value(sessionKey{}).(session)
		if !ok {
			return "", fail(401, "请先登录")
		}
		return s.ID, nil
	}
	if a.Store.Get("password") != "" {
		return "", fail(409, "已完成初始化，请登录后导入")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, e := url.Parse(origin)
		if e != nil || !strings.EqualFold(u.Host, r.Host) {
			return "", fail(403, "禁止跨站导入")
		}
	}
	if a.SetupToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Setup-Token")), []byte(a.SetupToken)) != 1 {
		return "", fail(403, "初始化代码不正确")
	}
	return tokenHash(a.SetupToken), nil
}

func (a *App) importBackupPreview(w http.ResponseWriter, r *http.Request) error {
	owner, e := a.importOwner(r)
	if e != nil {
		return e
	}
	base := filepath.Join(a.Store.Dir, "imports")
	if e = os.MkdirAll(base, 0700); e != nil {
		return e
	}
	// Expired uploads contain credentials; remove them on the next upload.
	entries, _ := os.ReadDir(base)
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && entry.IsDir() && time.Since(info.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(base, entry.Name()))
		}
	}
	id := ID()
	dir := filepath.Join(base, id)
	if e = os.Mkdir(dir, 0700); e != nil {
		return e
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupUpload)
	f, e := os.OpenFile(filepath.Join(dir, "backup.zip"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(f, r.Body)
	closeErr := f.Close()
	if e != nil {
		return fail(400, "上传失败或备份超过 512 MiB")
	}
	if closeErr != nil {
		return closeErr
	}
	s, e := ExtractBackup(filepath.Join(dir, "backup.zip"), dir)
	if e != nil {
		return fail(400, e.Error())
	}
	defer s.DB.Close()
	if s.Get("password") == "" {
		return fail(400, "该备份尚未初始化管理员密码，不能通过网页导入")
	}
	p := BackupPreview{ID: id, Owner: owner}
	for table, count := range map[string]*int{"accounts": &p.Accounts, "nodes": &p.Files, "sources": &p.Sources, "jobs": &p.Jobs} {
		if e = s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(count); e != nil {
			return e
		}
	}
	p.Files = max(0, p.Files-1)
	if e = os.WriteFile(filepath.Join(dir, "owner"), []byte(owner), 0600); e != nil {
		return e
	}
	keep = true
	writeJSON(w, 200, p)
	return nil
}

func (a *App) importBackupCommit(w http.ResponseWriter, r *http.Request) error {
	owner, e := a.importOwner(r)
	if e != nil {
		return e
	}
	var v struct {
		ID       string `json:"id"`
		Confirm  bool   `json:"confirm"`
		Password string `json:"current_password"`
	}
	if e = decode(r, &v); e != nil {
		return e
	}
	if !v.Confirm {
		return fail(400, "请确认使用备份替换全部数据")
	}
	if a.Store.Get("password") != "" && !passwordOK(v.Password, a.Store.Get("password")) {
		return fail(403, "当前管理员密码不正确")
	}
	if len(v.ID) != 32 || strings.Trim(v.ID, "0123456789abcdef") != "" {
		return fail(400, "导入编号无效")
	}
	dir := filepath.Join(a.Store.Dir, "imports", v.ID)
	info, e := os.Stat(dir)
	if e != nil || time.Since(info.ModTime()) > time.Hour {
		return fail(410, "预览已过期，请重新上传")
	}
	stored, e := os.ReadFile(filepath.Join(dir, "owner"))
	if e != nil || string(stored) != owner {
		return fail(403, "该备份预览不属于当前登录")
	}
	check, e := os.MkdirTemp(dir, "verify-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(check)
	s, e := ExtractBackup(filepath.Join(dir, "backup.zip"), check)
	if e != nil {
		return fail(400, e.Error())
	}
	defer func() { s.DB.Close(); os.RemoveAll(dir) }()
	if s.Get("password") == "" {
		return fail(400, "该备份尚未初始化管理员密码，不能通过网页导入")
	}
	backup := filepath.Join(a.Store.Dir, "before-import-"+ID()+".zip")
	if e = a.Store.Backup(backup); e != nil {
		return fmt.Errorf("导入前备份失败：%w", e)
	}
	if e = replaceData(a.Store, s); e != nil {
		return e
	}
	a.clientsMu.Lock()
	a.clients = map[string]*pikpak.Client{}
	a.clientsMu.Unlock()
	_ = os.Remove(filepath.Join(dir, "backup.zip"))
	a.Store.Event("data_import", map[string]string{"backup": filepath.Base(backup)})
	http.SetCookie(w, &http.Cookie{Name: "vault_session", Value: "", Path: "/", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true, "backup": filepath.Base(backup), "login_required": true})
	return nil
}

func replaceData(dst, src *Store) error {
	tx, e := dst.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, table := range []string{"sessions", "bindings", "accounts", "sources", "nodes", "jobs", "events", "settings"} {
		if _, e = tx.Exec(`DELETE FROM ` + table); e != nil {
			return e
		}
	}
	for _, table := range importTables {
		rows, err := src.DB.Query(`SELECT * FROM ` + table)
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		quoted := []string{}
		for _, col := range cols {
			quoted = append(quoted, `"`+strings.ReplaceAll(col, `"`, `""`)+`"`)
		}
		stmt := `INSERT INTO ` + table + ` (` + strings.Join(quoted, ",") + `) VALUES (` + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + `)`
		for rows.Next() {
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range values {
				pointers[i] = &values[i]
			}
			if e = rows.Scan(pointers...); e != nil {
				break
			}
			for i, col := range cols {
				if col == "secret" && (table == "accounts" || table == "sources") {
					var raw json.RawMessage
					secret, ok := values[i].(string)
					if !ok {
						e = fmt.Errorf("认证信息类型无效")
						break
					}
					if e = src.Unseal(secret, &raw); e != nil {
						break
					}
					values[i], e = dst.Seal(raw)
					if e != nil {
						break
					}
				}
			}
			if e != nil {
				break
			}
			if _, e = tx.Exec(stmt, values...); e != nil {
				break
			}
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
	}
	if _, e = tx.Exec(`UPDATE jobs SET state='paused',message='数据已导入，请核对账号后手动继续任务' WHERE state IN ('queued','running','waiting','retry')`); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT INTO settings(key,value) VALUES('import_review_required','true') ON CONFLICT(key) DO UPDATE SET value='true'`); e != nil {
		return e
	}
	return tx.Commit()
}

func (a *App) resumeAfterImport(w http.ResponseWriter, r *http.Request) error {
	if e := a.Store.Set("import_review_required", "false"); e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 200, map[string]bool{"ok": true})
	return nil
}

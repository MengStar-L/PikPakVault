package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB  *sql.DB
	Dir string
	key []byte
}

func ID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func now() int64            { return time.Now().Unix() }
func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "vault.db")
	keyPath := filepath.Join(dir, "master.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := os.Stat(path); e == nil {
			return nil, fmt.Errorf("master.key is missing for existing database; restore the matching key")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(key)
		f.Close()
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master.key must contain 32 bytes")
	}
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 1 {
		db.Close()
		return nil, fmt.Errorf("database schema %d requires a newer PikPak Vault", version)
	}
	s := &Store{DB: db, Dir: dir, key: key}
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0600)
	if s.Get("instance") == "" {
		if err = s.Set("instance", ID()); err != nil {
			return nil, err
		}
	}
	if s.Get("scan_minutes") == "" {
		if err = s.Set("scan_minutes", "30"); err != nil {
			return nil, err
		}
	}
	if s.Get("root_path") == "" {
		if err = s.Set("root_path", DefaultRootPath); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS accounts (id TEXT PRIMARY KEY,name TEXT NOT NULL,identity TEXT NOT NULL DEFAULT '',secret TEXT NOT NULL,root_id TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT 'unverified',error TEXT NOT NULL DEFAULT '',verification_url TEXT NOT NULL DEFAULT '',quota_limit INTEGER NOT NULL DEFAULT 0,quota_used INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS account_identity ON accounts(identity) WHERE identity<>'';
CREATE TABLE IF NOT EXISTS sources (id TEXT PRIMARY KEY,kind TEXT NOT NULL,link TEXT NOT NULL,share_id TEXT NOT NULL DEFAULT '',share_parent TEXT NOT NULL DEFAULT '',secret TEXT NOT NULL,selected TEXT NOT NULL DEFAULT '[]',manifest TEXT NOT NULL DEFAULT '[]',created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS nodes (id TEXT PRIMARY KEY,parent_id TEXT NOT NULL DEFAULT 'root',name TEXT NOT NULL,kind TEXT NOT NULL,size INTEGER NOT NULL DEFAULT 0,hash TEXT NOT NULL DEFAULT '',mime TEXT NOT NULL DEFAULT '',source_id TEXT NOT NULL DEFAULT '',source_path TEXT NOT NULL DEFAULT '',source_key TEXT NOT NULL DEFAULT '',favorite INTEGER NOT NULL DEFAULT 0,trashed INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL,modified INTEGER NOT NULL,opened INTEGER NOT NULL DEFAULT 0,position REAL NOT NULL DEFAULT 0,revision INTEGER NOT NULL DEFAULT 1);
INSERT OR IGNORE INTO nodes(id,parent_id,name,kind,created,modified) VALUES('root','','My files','folder',0,0);
CREATE INDEX IF NOT EXISTS nodes_parent ON nodes(parent_id,trashed);
CREATE INDEX IF NOT EXISTS nodes_source ON nodes(source_id,source_path);
CREATE TABLE IF NOT EXISTS bindings (account_id TEXT NOT NULL REFERENCES accounts(id),node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,remote_id TEXT NOT NULL,state TEXT NOT NULL,remote_name TEXT NOT NULL DEFAULT '',remote_parent TEXT NOT NULL DEFAULT '',thumbnail TEXT NOT NULL DEFAULT '',checked INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(account_id,node_id));
CREATE UNIQUE INDEX IF NOT EXISTS binding_remote ON bindings(account_id,remote_id) WHERE remote_id<>'';
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY,account_id TEXT NOT NULL,kind TEXT NOT NULL,title TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'queued',data TEXT NOT NULL,progress INTEGER NOT NULL DEFAULT 0,message TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,next_run INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL,updated INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS jobs_schedule ON jobs(state,next_run,created);
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,detail TEXT NOT NULL,created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,csrf TEXT NOT NULL,expires INTEGER NOT NULL);
PRAGMA user_version=1;
`

func (s *Store) Get(k string) string {
	var v string
	_ = s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, k).Scan(&v)
	return v
}
func (s *Store) Set(k, v string) error {
	_, err := s.DB.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return err
}
func (s *Store) Seal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	n := make([]byte, g.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(g.Seal(n, n, b, []byte("pikpak-vault-v1"))), nil
}
func (s *Store) Unseal(v string, out any) error {
	b, e := base64.RawStdEncoding.DecodeString(v)
	if e != nil {
		return e
	}
	block, e := aes.NewCipher(s.key)
	if e != nil {
		return e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return e
	}
	if len(b) < g.NonceSize() {
		return fmt.Errorf("invalid encrypted data")
	}
	p, e := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte("pikpak-vault-v1"))
	if e != nil {
		return fmt.Errorf("unable to decrypt data: verify master.key")
	}
	return json.Unmarshal(p, out)
}
func (s *Store) Event(kind string, v any) {
	_, _ = s.DB.Exec(`INSERT INTO events(kind,detail,created) VALUES(?,?,?)`, kind, jsonText(v), now())
}

type Account struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Identity        string `json:"identity"`
	Secret          string `json:"-"`
	RootID          string `json:"root_id"`
	Status          string `json:"status"`
	Error           string `json:"error"`
	VerificationURL string `json:"verification_url"`
	Limit           int64  `json:"quota_limit"`
	Used            int64  `json:"quota_used"`
	Created         int64  `json:"created"`
}

const accountCols = `id,name,identity,secret,root_id,status,error,verification_url,quota_limit,quota_used,created`

type scanner interface{ Scan(...any) error }

func accountScan(r scanner) (a Account, e error) {
	e = r.Scan(&a.ID, &a.Name, &a.Identity, &a.Secret, &a.RootID, &a.Status, &a.Error, &a.VerificationURL, &a.Limit, &a.Used, &a.Created)
	return
}
func (s *Store) Account(id string) (Account, error) {
	return accountScan(s.DB.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id=?`, id))
}
func (s *Store) Accounts() ([]Account, error) {
	rows, e := s.DB.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY created`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, e := accountScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type Node struct {
	FolderPreviews []FolderPreview `json:"folder_previews,omitempty"`
	Transfer       *FileTransfer   `json:"transfer,omitempty"` // Read-only projection of a durable import job.
	ID             string          `json:"id"`
	ParentID       string          `json:"parent_id"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	Size           int64           `json:"size"`
	Hash           string          `json:"hash"`
	Mime           string          `json:"mime"`
	SourceID       string          `json:"source_id"`
	SourcePath     string          `json:"source_path"`
	SourceKey      string          `json:"source_key"`
	Favorite       bool            `json:"favorite"`
	Trashed        bool            `json:"trashed"`
	Created        int64           `json:"created"`
	Modified       int64           `json:"modified"`
	Opened         int64           `json:"opened"`
	Position       float64         `json:"position"`
	Revision       int64           `json:"revision"`
	State          string          `json:"state"`
	RemoteID       string          `json:"remote_id,omitempty"`
	Thumbnail      string          `json:"thumbnail,omitempty"`
	Checked        int64           `json:"checked"`
}

const nodeCols = `n.id,n.parent_id,n.name,n.kind,n.size,n.hash,n.mime,n.source_id,n.source_path,n.source_key,n.favorite,n.trashed,n.created,n.modified,n.opened,n.position,n.revision,COALESCE(b.state,'unbound'),COALESCE(b.remote_id,''),COALESCE(b.thumbnail,''),COALESCE(b.checked,0)`

func nodeScan(r scanner) (n Node, e error) {
	e = r.Scan(&n.ID, &n.ParentID, &n.Name, &n.Kind, &n.Size, &n.Hash, &n.Mime, &n.SourceID, &n.SourcePath, &n.SourceKey, &n.Favorite, &n.Trashed, &n.Created, &n.Modified, &n.Opened, &n.Position, &n.Revision, &n.State, &n.RemoteID, &n.Thumbnail, &n.Checked)
	return
}
func (s *Store) Node(id, account string) (Node, error) {
	return nodeScan(s.DB.QueryRow(`SELECT `+nodeCols+` FROM nodes n LEFT JOIN bindings b ON b.node_id=n.id AND b.account_id=? WHERE n.id=?`, account, id))
}
func (s *Store) AllNodes(account string) ([]Node, error) {
	rows, e := s.DB.Query(`SELECT `+nodeCols+` FROM nodes n LEFT JOIN bindings b ON b.node_id=n.id AND b.account_id=? WHERE n.id<>'root' ORDER BY n.created,n.id`, account)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		n, e := nodeScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func ValidName(n string) error {
	if n == "" || n == "." || n == ".." || strings.TrimSpace(n) == "" || !utf8.ValidString(n) || strings.ContainsAny(n, "/\\\x00\r\n") || len(n) > 1024 {
		return fmt.Errorf("invalid file name")
	}
	return nil
}
func (s *Store) InsertNode(n Node) error {
	_, e := s.DB.Exec(`INSERT INTO nodes(id,parent_id,name,kind,size,hash,mime,source_id,source_path,source_key,created,modified) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, n.ID, n.ParentID, n.Name, n.Kind, n.Size, n.Hash, n.Mime, n.SourceID, n.SourcePath, n.SourceKey, n.Created, n.Modified)
	return e
}
func (s *Store) Path(id string) (string, error) {
	parts := []string{}
	seen := map[string]bool{}
	for id != "root" && id != "" {
		if seen[id] {
			return "", fmt.Errorf("directory cycle")
		}
		seen[id] = true
		var name, parent string
		if e := s.DB.QueryRow(`SELECT parent_id,name FROM nodes WHERE id=?`, id).Scan(&parent, &name); e != nil {
			return "", e
		}
		parts = append([]string{name}, parts...)
		id = parent
	}
	return "/" + strings.Join(parts, "/"), nil
}
func (s *Store) Bind(account, node, remote, state, name, parent, thumbnail string) error {
	_, e := s.DB.Exec(`INSERT INTO bindings(account_id,node_id,remote_id,state,remote_name,remote_parent,thumbnail,checked) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(account_id,node_id) DO UPDATE SET remote_id=excluded.remote_id,state=excluded.state,remote_name=excluded.remote_name,remote_parent=excluded.remote_parent,thumbnail=excluded.thumbnail,checked=excluded.checked`, account, node, remote, state, name, parent, thumbnail, now())
	return e
}
func (s *Store) State(account, node, state string) error {
	_, e := s.DB.Exec(`UPDATE bindings SET state=?,checked=? WHERE account_id=? AND node_id=?`, state, now(), account, node)
	return e
}

type Source struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Link        string   `json:"link"`
	ShareID     string   `json:"share_id"`
	ShareParent string   `json:"share_parent"`
	Secret      string   `json:"-"`
	Selected    []string `json:"selected"`
	Manifest    []Entry  `json:"manifest"`
	Created     int64    `json:"created"`
}
type Entry struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
}

func (s *Store) Source(id string) (v Source, e error) {
	var selected, manifest string
	e = s.DB.QueryRow(`SELECT id,kind,link,share_id,share_parent,secret,selected,manifest,created FROM sources WHERE id=?`, id).Scan(&v.ID, &v.Kind, &v.Link, &v.ShareID, &v.ShareParent, &v.Secret, &selected, &manifest, &v.Created)
	if e != nil {
		return
	}
	e = json.Unmarshal([]byte(selected), &v.Selected)
	if e == nil {
		e = json.Unmarshal([]byte(manifest), &v.Manifest)
	}
	return
}
func (s *Store) SaveSource(v Source) error {
	_, e := s.DB.Exec(`INSERT INTO sources(id,kind,link,share_id,share_parent,secret,selected,manifest,created) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET link=excluded.link,share_id=excluded.share_id,share_parent=excluded.share_parent,secret=excluded.secret,selected=excluded.selected,manifest=excluded.manifest`, v.ID, v.Kind, v.Link, v.ShareID, v.ShareParent, v.Secret, jsonText(v.Selected), jsonText(v.Manifest), v.Created)
	return e
}

type Job struct {
	ID        string          `json:"id"`
	AccountID string          `json:"account_id"`
	Kind      string          `json:"kind"`
	Title     string          `json:"title"`
	State     string          `json:"state"`
	Data      json.RawMessage `json:"-"`
	Progress  int             `json:"progress"`
	Message   string          `json:"message"`
	Attempts  int             `json:"attempts"`
	NextRun   int64           `json:"next_run"`
	Created   int64           `json:"created"`
	Updated   int64           `json:"updated"`
}

const jobCols = `id,account_id,kind,title,state,data,progress,message,attempts,next_run,created,updated`

func jobScan(r scanner) (j Job, e error) {
	var d string
	e = r.Scan(&j.ID, &j.AccountID, &j.Kind, &j.Title, &j.State, &d, &j.Progress, &j.Message, &j.Attempts, &j.NextRun, &j.Created, &j.Updated)
	j.Data = json.RawMessage(d)
	return
}
func (s *Store) Job(id string) (Job, error) {
	return jobScan(s.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}
func (s *Store) Jobs() ([]Job, error) {
	rows, e := s.DB.Query(`SELECT ` + jobCols + ` FROM jobs ORDER BY created DESC LIMIT 500`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, e := jobScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Store) NewJob(account, kind, title string, data any) (Job, error) {
	j := Job{ID: ID(), AccountID: account, Kind: kind, Title: title, State: "queued", Data: json.RawMessage(jsonText(data)), Created: now(), Updated: now()}
	_, e := s.DB.Exec(`INSERT INTO jobs(id,account_id,kind,title,state,data,created,updated) VALUES(?,?,?,?,?,?,?,?)`, j.ID, account, kind, title, j.State, string(j.Data), j.Created, j.Updated)
	return j, e
}
func (s *Store) SaveJob(j *Job, data any) error {
	if data != nil {
		j.Data = json.RawMessage(jsonText(data))
	}
	j.Updated = now()
	_, e := s.DB.Exec(`UPDATE jobs SET state=?,data=?,progress=?,message=?,attempts=?,next_run=?,updated=? WHERE id=?`, j.State, string(j.Data), j.Progress, j.Message, j.Attempts, j.NextRun, j.Updated, j.ID)
	return e
}
func (s *Store) Descendants(ids []string, account string) ([]Node, error) {
	all, e := s.AllNodes(account)
	if e != nil {
		return nil, e
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if len(ids) == 0 || set["root"] {
		return all, nil
	}
	changed := true
	for changed {
		changed = false
		for _, n := range all {
			if set[n.ParentID] && !set[n.ID] {
				set[n.ID] = true
				changed = true
			}
		}
	}
	out := []Node{}
	for _, n := range all {
		if set[n.ID] {
			out = append(out, n)
		}
	}
	return out, nil
}

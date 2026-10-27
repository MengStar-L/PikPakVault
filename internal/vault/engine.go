package vault

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"pikpakvault/internal/pikpak"
)

type App struct {
	Store         *Store
	SetupToken    string
	SecureCookies bool
	Factory       func(Account) (pikpak.Provider, error)
	MediaHTTP     *http.Client
	TelDriveHTTP  *http.Client
	RSSHTTP       *http.Client
	gate          sync.RWMutex
	dataMu        sync.RWMutex // Import replaces the logical database as one transaction.
	jobMu         sync.Mutex
	executing     map[string]bool // Protected by jobMu, including paused requests still unwinding.
	structureMu   sync.Mutex      // Short directory/path operations, never file download/upload.
	clientsMu     sync.Mutex
	clients       map[string]*pikpak.Client
	wake          chan struct{}
}

func NewApp(s *Store) *App {
	return &App{Store: s, clients: map[string]*pikpak.Client{}, wake: make(chan struct{}, 1)}
}
func (a *App) notify() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}
func (a *App) active() string { return a.Store.Get("active_account") }
func (a *App) client(id string) (pikpak.Provider, error) {
	ac, e := a.Store.Account(id)
	if e != nil {
		return nil, e
	}
	if a.Factory != nil {
		return a.Factory(ac)
	}
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	if c := a.clients[id]; c != nil {
		return c, nil
	}
	ac, e = a.Store.Account(id)
	if e != nil {
		return nil, e
	}
	var creds pikpak.Credentials
	if e = a.Store.Unseal(ac.Secret, &creds); e != nil {
		return nil, e
	}
	if ac.Identity != "" {
		creds.UserID = ac.Identity
	}
	c := pikpak.New(creds)
	var credentialMu sync.Mutex
	expectedSecret := ac.Secret
	c.OnError = func(err error) {
		credentialMu.Lock()
		secret := expectedSecret
		credentialMu.Unlock()
		a.observeAccountErrorIfCurrent(id, secret, err)
	}
	c.Save = func(v pikpak.Credentials) error {
		credentialMu.Lock()
		defer credentialMu.Unlock()
		secret, e := a.Store.Seal(v)
		if e != nil {
			return e
		}
		result, e := a.Store.DB.Exec(`UPDATE accounts SET secret=? WHERE id=? AND secret=?`, secret, id, expectedSecret)
		if e != nil {
			return e
		}
		changed, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if changed != 1 {
			return block("Account credentials were updated; retry with the new credentials")
		}
		expectedSecret = secret
		return nil
	}
	c.BeforeRequest = func(ctx context.Context) error {
		credentialMu.Lock()
		current, e := a.Store.Account(id)
		credentialsMatch := e == nil && current.Secret == expectedSecret
		credentialMu.Unlock()
		if !credentialsMatch {
			return errPaused
		}
		if ctx.Value(accountProbeKey{}) == id || current.Status == "ready" || current.Status == "quota_exceeded" {
			return nil
		}
		return block(accountErrorMessage(current))
	}
	c.BeforeWrite = func() (func(), error) {
		a.gate.RLock()
		if a.active() != id {
			a.gate.RUnlock()
			return nil, errPaused
		}
		credentialMu.Lock()
		current, e := a.Store.Account(id)
		credentialsMatch := e == nil && current.Secret == expectedSecret
		credentialMu.Unlock()
		if !credentialsMatch {
			a.gate.RUnlock()
			return nil, block("Account credentials changed; this request was paused")
		}
		return a.gate.RUnlock, nil
	}
	a.clients[id] = c
	return c, nil
}

var errPaused = errors.New("account switched; task paused")

type pending struct {
	message string
	delay   int64
}

func (e *pending) Error() string { return e.message }

type attention struct{ message string }

func (e *attention) Error() string { return e.message }
func wait(message string) error    { return &pending{message, 15} }
func block(message string) error   { return &attention{message} }

type RemoteEntry struct {
	File pikpak.File `json:"file"`
	Path string      `json:"path"`
}
type TransferState struct {
	Mode            string          `json:"mode,omitempty"`
	TargetID        string          `json:"target_id,omitempty"`
	BeforeIDs       []string        `json:"before_ids,omitempty"`
	BeforeTasks     []string        `json:"before_tasks,omitempty"`
	StrictTarget    bool            `json:"strict_target,omitempty"`
	ShareParentID   string          `json:"share_parent_id,omitempty"`
	ShareTrace      json.RawMessage `json:"share_trace,omitempty"`
	Expected        []Entry         `json:"expected,omitempty"`
	Display         []Entry         `json:"display,omitempty"` // UI only; never used to verify or recover content.
	Polls           int             `json:"polls,omitempty"`
	Fingerprint     string          `json:"fingerprint,omitempty"`
	StableSince     int64           `json:"stable_since,omitempty"`
	VerifiedByFiles bool            `json:"verified_by_files,omitempty"`
	StageParent     string          `json:"stage_parent,omitempty"`
	StageName       string          `json:"stage_name,omitempty"`
	StageID         string          `json:"stage_id"`
	Phase           string          `json:"phase"`
	TaskID          string          `json:"task_id"`
	OutputIDs       []string        `json:"output_ids"`
	Entries         []RemoteEntry   `json:"entries"`
	Started         int64           `json:"started"`
}
type JobData struct {
	WaitReason     string                     `json:"wait_reason,omitempty"`
	WaitSpace      *diskSpaceWait             `json:"wait_space,omitempty"`
	RecoveryLane   string                     `json:"recovery_lane,omitempty"`
	OperationID    string                     `json:"operation_id,omitempty"` // Original remote intent names after partitioning a legacy recovery.
	RSSID          string                     `json:"rss_id,omitempty"`
	RSSManual      bool                       `json:"rss_manual,omitempty"`
	MonitorID      string                     `json:"monitor_id,omitempty"`
	ExactNodes     bool                       `json:"exact_nodes,omitempty"` // Automatic repair must not expand into healthy descendants.
	Uploads        map[string]*TelDriveUpload `json:"uploads,omitempty"`
	Preview        []Entry                    `json:"preview,omitempty"` // Untrusted display hints from the share picker.
	InstantStates  map[string]*InstantState   `json:"instant_states,omitempty"`
	CleanupJobs    []string                   `json:"cleanup_jobs,omitempty"`
	CleanupResults map[string]string          `json:"cleanup_results,omitempty"`
	Note           string                     `json:"note,omitempty"`
	RootPath       string                     `json:"root_path,omitempty"`
	SourceID       string                     `json:"source_id,omitempty"`
	ParentID       string                     `json:"parent_id,omitempty"`
	NodeIDs        []string                   `json:"node_ids,omitempty"`
	Transfers      map[string]*TransferState  `json:"transfers,omitempty"`
	Done           map[string]bool            `json:"done,omitempty"`
	Problems       map[string]string          `json:"problems,omitempty"`
	Instant        map[string]string          `json:"instant,omitempty"`
	InstantTried   map[string]bool            `json:"instant_tried,omitempty"`
}

func (d *JobData) init() {
	if d.InstantStates == nil {
		d.InstantStates = map[string]*InstantState{}
	}
	if d.Transfers == nil {
		d.Transfers = map[string]*TransferState{}
	}
	if d.Done == nil {
		d.Done = map[string]bool{}
	}
	if d.Problems == nil {
		d.Problems = map[string]string{}
	}
	if d.Instant == nil {
		d.Instant = map[string]string{}
	}
	if d.InstantTried == nil {
		d.InstantTried = map[string]bool{}
	}
}
func (a *App) checkpoint(j *Job, d *JobData) error {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	current, e := a.Store.Job(j.ID)
	if e != nil {
		return e
	}
	if current.State == "cancelled" || current.State == "paused" {
		return errPaused
	}
	if j.AccountID != a.active() && j.Kind != "verify" {
		return errPaused
	}
	if ac, err := a.Store.Account(j.AccountID); err == nil && !accountStatusAllows(ac.Status, j.Kind) {
		return block(accountErrorMessage(ac))
	}
	return a.Store.SaveJob(j, d)
}

// updateJobProgress publishes long-running read-only work without changing
// the task state. The running predicate is important: a user may pause or
// cancel a scan while a provider call is unwinding, and that late callback
// must not make the task look active again.
func (a *App) updateJobProgress(j *Job, progress int, message string) {
	progress = min(max(progress, 0), 99)
	updated := now()
	result, err := a.Store.DB.Exec(`UPDATE jobs SET progress=?,message=?,updated=? WHERE id=? AND state='running'`, progress, message, updated, j.ID)
	if err != nil {
		return
	}
	if changed, err := result.RowsAffected(); err == nil && changed == 1 {
		j.Progress = progress
		j.Message = message
	}
}
func (a *App) scheduleScan() {
	minutes, _ := strconv.Atoi(a.Store.Get("scan_minutes"))
	last, _ := strconv.ParseInt(a.Store.Get("last_auto_scan"), 10, 64)
	if minutes <= 0 || !a.accountJobAllowed(a.active(), "scan") || now()-last < int64(minutes)*60 {
		return
	}
	var count int
	_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE account_id=? AND kind='scan' AND state IN ('queued','running','waiting','retry')`, a.active()).Scan(&count)
	if count > 0 {
		return
	}
	_, e := a.Store.NewJob(a.active(), "scan", "Scheduled library check", JobData{})
	if e == nil {
		_ = a.Store.Set("last_auto_scan", strconv.FormatInt(now(), 10))
	}
}
func (a *App) Execute(ctx context.Context, j *Job) {
	a.dataMu.RLock()
	defer a.dataMu.RUnlock()
	if a.Store.Get("import_review_required") == "true" || updatePending() {
		return
	}
	ctx = context.WithValue(ctx, operationKey{}, &operationCache{folders: map[string]resolvedFolder{}})
	a.jobMu.Lock()
	if a.executing[j.ID] {
		a.jobMu.Unlock()
		return
	}
	current, e := a.Store.Job(j.ID)
	if e != nil || current.State == "paused" || current.State == "cancelled" || current.State == "completed" {
		a.jobMu.Unlock()
		return
	}
	if ctx.Value(scheduledExecutionKey{}) == true &&
		(current.NextRun > now() || (current.Kind != "verify" && current.AccountID != a.active()) ||
			(current.State != "queued" && current.State != "waiting" && current.State != "retry")) {
		a.jobMu.Unlock()
		return
	}
	*j = current
	startAccount, accountErr := a.Store.Account(j.AccountID)
	if accountErr == nil && !accountStatusAllows(startAccount.Status, j.Kind) {
		j.State, j.Message = "paused", accountErrorMessage(startAccount)
		_ = a.Store.SaveJob(j, nil)
		a.jobMu.Unlock()
		a.Store.Event("job", map[string]string{"id": j.ID, "state": j.State})
		return
	}
	j.State = "running"
	j.Message = ""
	if e := a.Store.SaveJob(j, nil); e != nil {
		a.jobMu.Unlock()
		return
	}
	if a.executing == nil {
		a.executing = make(map[string]bool)
	}
	a.executing[j.ID] = true
	defer func() {
		a.jobMu.Lock()
		delete(a.executing, j.ID)
		a.jobMu.Unlock()
		a.notify()
	}()
	a.jobMu.Unlock()
	var d JobData
	var c pikpak.Provider
	e = json.Unmarshal(j.Data, &d)
	d.init()
	if e == nil {
		e = a.recheckDownloadSpace(j, &d)
	}
	if e == nil {
		c, e = a.client(j.AccountID)
		if e == nil {
			switch j.Kind {
			case "rss_scan":
				e = a.scanRSS(ctx, j, &d)
			case "teldrive_scan":
				e = a.scanTelDrive(ctx, c, j, &d)
			case "teldrive_upload":
				e = a.uploadTelDriveJob(ctx, c, j, &d)
			case "verify":
				e = a.verify(ctx, c, j)
			case "root":
				e = a.prepareRoot(ctx, c, j.AccountID)
			case "scan":
				e = a.scan(ctx, c, j)
			case "adopt":
				e = a.adoptCloud(ctx, c, j, &d)
			case "cleanup":
				e = a.cleanup(ctx, c, j, &d)
			case "import":
				e = a.importSource(ctx, c, j, &d)
			case "recover":
				e = a.recover(ctx, c, j, &d)
			case "sync", "trash":
				e = a.syncNodes(ctx, c, j, &d)
			default:
				e = fmt.Errorf("unknown task type")
			}
		}
	}
	// Client callbacks also catch failures hidden by recovery fallbacks. Observe
	// final/synthetic errors here too, without applying a stale credential result.
	if e != nil {
		failureSecret := startAccount.Secret
		staleClient := false
		if a.Factory == nil && c != nil {
			if latest, err := a.currentClientAccount(j.AccountID, c); err == nil {
				failureSecret = latest.Secret
			} else {
				staleClient = true
			}
		}
		if !staleClient {
			a.observeAccountErrorIfCurrent(j.AccountID, failureSecret, e)
			if j.Kind == "verify" {
				a.observeVerificationFailure(j.AccountID, failureSecret, e)
			}
		}
	}
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	current, ce := a.Store.Job(j.ID)
	if ce == nil && (current.State == "cancelled" || current.State == "paused") {
		j.State = current.State
		j.Message = current.Message
	} else if errors.Is(e, errPaused) {
		j.State = "paused"
		j.Message = errPaused.Error()
	} else if e != nil {
		var p *pending
		var at *attention
		accountStatus, accountMessage, _ := classifyAccountError(e)
		switch {
		case errors.Is(e, context.Canceled):
			j.State, j.Message = "queued", "服务停止，重启后先核对已提交结果"
		case accountStatus != "":
			j.State, j.Message = "paused", accountMessage
		case j.Kind == "verify":
			j.State, j.Message = "failed", "账号验证未完成，请检查网络和账号连接后重试"
		case !a.accountJobAllowed(j.AccountID, j.Kind):
			ac, _ := a.Store.Account(j.AccountID)
			j.State, j.Message = "paused", accountErrorMessage(ac)
		case errors.As(e, &p):
			j.State = "waiting"
			j.NextRun = now() + p.delay
			j.Message = p.message
		case errors.As(e, &at):
			j.State = "attention"
			j.Message = at.message
		case pikpak.Temporary(e) && j.Attempts < 6:
			j.Attempts++
			j.State = "retry"
			delay := min(int64(5*(1<<j.Attempts)), 300)
			var up *pikpak.APIError
			if errors.As(e, &up) && up.RetryAfter > 0 {
				delay = max(delay, int64(up.RetryAfter))
			}
			j.NextRun = now() + delay
			j.Message = e.Error()
		default:
			j.State = "failed"
			j.Message = e.Error()
		}
		var up *pikpak.APIError
		if errors.As(e, &up) && up.Status == 429 {
			until := now() + int64(max(up.RetryAfter, 60))
			_, _ = a.Store.DB.Exec(`UPDATE jobs SET next_run=MAX(next_run,?) WHERE account_id=? AND state IN ('queued','waiting','retry')`, until, j.AccountID)
			j.NextRun = max(j.NextRun, until)
		}
	} else {
		j.State = "completed"
		j.Progress = 100
		j.Message = d.Note
		if len(d.Problems) > 0 {
			j.State = "partial"
			j.Message = fmt.Sprintf("%d item(s) need attention", len(d.Problems))
		}
	}
	_ = a.Store.SaveJob(j, &d)
	a.Store.Event("job", map[string]string{"id": j.ID, "state": j.State})
}
func (a *App) verify(ctx context.Context, c pikpak.Provider, j *Job) error {
	ctx = context.WithValue(ctx, accountProbeKey{}, j.AccountID)
	me, e := c.Me(ctx)
	if e != nil {
		return e
	}
	if me.Sub == "" {
		return block("PikPak returned no account identity")
	}
	ac, e := a.currentClientAccount(j.AccountID, c)
	if e != nil {
		return e
	}
	if ac.Identity != "" && ac.Identity != me.Sub {
		return &pikpak.APIError{Status: 409, Code: "identity_changed"}
	}
	q, e := c.Quota(ctx)
	if e != nil {
		return e
	}
	ac, e = a.currentClientAccount(j.AccountID, c)
	if e != nil {
		return e
	}
	result, e := a.Store.DB.Exec(`UPDATE accounts SET identity=?,quota_limit=?,quota_used=? WHERE id=? AND secret=?`, me.Sub, int64(q.Limit), int64(q.Usage), j.AccountID, ac.Secret)
	if e != nil {
		return e
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errPaused
	}
	if !q.Unlimited && q.Limit > 0 && q.Usage >= q.Limit {
		// Identity and quota were verified, so a previous login/freeze fault is
		// resolved. Keep the capacity restriction and do not create directories.
		result, e = a.Store.DB.Exec(`UPDATE accounts SET status='quota_exceeded',error='',verification_url='' WHERE id=? AND secret=?`, j.AccountID, ac.Secret)
		if e != nil {
			return e
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return errPaused
		}
		return &pikpak.APIError{Code: "quota_exceeded"}
	}
	if a.active() == j.AccountID {
		e = a.prepareRoot(ctx, c, j.AccountID)
	}
	if e != nil {
		return e
	}
	ac, e = a.currentClientAccount(j.AccountID, c)
	if e != nil {
		return e
	}
	result, e = a.Store.DB.Exec(`UPDATE accounts SET status='ready',error='',verification_url='' WHERE id=? AND secret=?`, j.AccountID, ac.Secret)
	if e == nil {
		if count, _ := result.RowsAffected(); count != 1 {
			return errPaused
		}
	}
	if e == nil {
		a.Store.Event("account", j.AccountID)
	}
	return e
}

func listAll(ctx context.Context, c pikpak.Provider, parent string) ([]pikpak.File, error) {
	out := []pikpak.File{}
	next := ""
	seen := map[string]bool{}
	ids := map[string]bool{}
	for {
		p, e := c.List(ctx, parent, next)
		if e != nil {
			return nil, e
		}
		for _, f := range p.Files {
			if f.ID == "" {
				return nil, block("Invalid remote file listing")
			}
			if !ids[f.ID] {
				out = append(out, f)
				ids[f.ID] = true
			}
		}
		if p.Next == "" {
			return out, nil
		}
		if seen[p.Next] {
			return nil, block("Repeated pagination token; scan is incomplete")
		}
		seen[p.Next] = true
		next = p.Next
	}
}
func (a *App) folder(ctx context.Context, c pikpak.Provider, account, nodeID string, seen map[string]bool) (result string, err error) {
	ctx, unlock := a.lockStructure(ctx)
	defer unlock()
	if nodeID == "" || nodeID == "root" {
		return a.ensureRoot(ctx, c, account)
	}
	if seen[nodeID] {
		return "", block("Local directory cycle")
	}
	seen[nodeID] = true
	defer delete(seen, nodeID)
	n, e := a.Store.Node(nodeID, account)
	if e != nil {
		return "", e
	}
	if n.Kind != "folder" || n.Trashed {
		return "", block("Target folder is missing or in the recycle bin")
	}
	if n.SourceID == "" && n.SourceKey != "" {
		defer func() {
			if err == nil {
				_, _ = a.Store.DB.Exec(`DELETE FROM settings WHERE key=?`, "teldrive_folder:"+account+":"+n.ID)
			}
		}()
	}
	if cache := operation(ctx); cache != nil {
		if saved, ok := cache.folders[nodeID]; ok && saved.revision == n.Revision && saved.parent == n.ParentID && saved.id == n.RemoteID {
			return saved.id, nil
		}
		defer func() {
			if err == nil {
				cache.folders[nodeID] = resolvedFolder{result, n.ParentID, n.Revision}
			}
		}()
	}
	parent, e := a.folder(ctx, c, account, n.ParentID, seen)
	if e != nil {
		return "", e
	}
	if n.RemoteID != "" {
		f, e := c.Get(ctx, n.RemoteID)
		if e == nil {
			if !f.Trashed && !f.Folder() {
				return "", block("Directory binding points to a file")
			}
			if f.Trashed {
				if e = c.Untrash(ctx, []string{f.ID}); e != nil {
					return "", e
				}
				f, e = c.Get(ctx, f.ID)
				if e != nil {
					return "", e
				}
				if f.Trashed {
					return "", wait("等待 PikPak 确认目录已从回收站还原")
				}
			}
			if e = a.align(ctx, c, account, n, f, parent); e != nil {
				return "", e
			}
			return f.ID, nil
		}
		if !pikpak.Missing(e) {
			return "", e
		}
	}
	files, e := listAll(ctx, c, parent)
	if e != nil {
		return "", e
	}
	directKey := "teldrive_folder:" + account + ":" + n.ID
	directRequest := jsonText(map[string]string{"parent": parent, "name": n.Name})
	if saved := a.Store.Get(directKey); n.SourceID == "" && n.SourceKey != "" && saved != "" {
		var original struct{ Parent, Name string }
		if e = json.Unmarshal([]byte(saved), &original); e != nil || original.Parent == "" || original.Name == "" {
			return "", block("TelDrive 文件夹创建记录无效，请核对远端结果")
		}
		results := files
		if original.Parent != parent {
			results, e = listAll(ctx, c, original.Parent)
			if e != nil {
				return "", e
			}
		}
		matches := []pikpak.File{}
		for _, f := range results {
			if f.Name == original.Name {
				matches = append(matches, f)
			}
		}
		if len(matches) == 1 && matches[0].Folder() {
			f := matches[0]
			// Bind the attributable result before aligning, so another path change
			// or a lost move response can resume using its ID.
			if e = a.Store.Bind(account, n.ID, f.ID, "pending", f.Name, f.ParentID, ""); e != nil {
				return "", e
			}
			if e = a.align(ctx, c, account, n, f, parent); e != nil {
				return "", e
			}
			_, _ = a.Store.DB.Exec(`DELETE FROM settings WHERE key=?`, directKey)
			return f.ID, nil
		}
	}
	for _, f := range files {
		if f.Name == n.Name {
			return "", block("An untracked item already uses directory name: " + n.Name)
		}
	}
	if n.SourceID == "" && n.SourceKey != "" {
		// TelDrive directories have stable source IDs. Create at their final name
		// once; an uncertain response requires review instead of folder churn.
		key := directKey
		if a.Store.Get(key) != "" {
			return "", block("TelDrive 文件夹创建响应不确定，请先核对远端目录后重试")
		}
		if e = a.Store.Set(key, directRequest); e != nil {
			return "", e
		}
		f, e := c.Mkdir(ctx, parent, n.Name)
		if e != nil {
			var up *pikpak.APIError
			if errors.Is(e, errPaused) || (errors.As(e, &up) && (up.Status == 400 || up.Status == 401 || up.Status == 403 || up.Status == 429)) {
				_, _ = a.Store.DB.Exec(`DELETE FROM settings WHERE key=?`, key)
			}
			return "", e
		}
		if e = a.Store.Bind(account, n.ID, f.ID, "pending", f.Name, f.ParentID, ""); e != nil {
			return "", e
		}
		if e = a.align(ctx, c, account, n, f, parent); e != nil {
			return "", e
		}
		_, _ = a.Store.DB.Exec(`DELETE FROM settings WHERE key=?`, key)
		return f.ID, nil
	}
	// Persist an unpredictable creation name before dispatch, so a lost response can be reconciled.
	marker := ".vault-folder-" + n.ID
	for _, f := range files {
		if f.Name == marker && f.Folder() {
			if e = a.align(ctx, c, account, n, f, parent); e != nil {
				return "", e
			}
			return f.ID, nil
		}
	}
	f, e := c.Mkdir(ctx, parent, marker)
	if e != nil {
		return "", e
	}
	if e = a.Store.Bind(account, n.ID, f.ID, "drift", f.Name, f.ParentID, ""); e != nil {
		return "", e
	}
	if e = a.align(ctx, c, account, n, f, parent); e != nil {
		return "", e
	}
	return f.ID, nil
}
func compatible(n Node, f pikpak.File) bool {
	if (n.Kind == "folder") != f.Folder() {
		return false
	}
	if n.Kind == "folder" {
		return true
	}
	return n.Size == int64(f.Size) && (n.Hash == "" || f.Hash == "" || strings.EqualFold(n.Hash, f.Hash))
}
func (a *App) align(ctx context.Context, c pikpak.Provider, account string, n Node, f pikpak.File, parent string) error {
	ctx, unlock := a.lockStructure(ctx)
	defer unlock()
	latest, e := a.Store.Node(n.ID, account)
	if e != nil {
		return e
	}
	if latest.Trashed || latest.Revision != n.Revision {
		return wait("Local file changed during the operation; reconciling its latest path")
	}
	if !compatible(n, f) {
		return block("Remote content differs from the saved file: " + n.Name)
	}
	if f.ParentID == parent && f.Name == n.Name && !f.Trashed && f.Complete() {
		return a.Store.Bind(account, n.ID, f.ID, "present", f.Name, f.ParentID, f.Thumbnail)
	}
	files, e := listAll(ctx, c, parent)
	if e != nil {
		return e
	}
	for _, other := range files {
		if other.ID != f.ID && other.Name == n.Name {
			return block("Name conflict: " + n.Name)
		}
	}
	if f.ParentID != parent {
		if e = c.Move(ctx, f.ID, parent); e != nil {
			return e
		}
	}
	if f.Name != n.Name {
		if e = c.Rename(ctx, f.ID, n.Name); e != nil {
			return e
		}
	}
	check, e := c.Get(ctx, f.ID)
	if e != nil {
		return e
	}
	if check.Trashed || !check.Complete() || check.ParentID != parent || check.Name != n.Name || !compatible(n, check) {
		return block("Restored file could not be verified: " + n.Name)
	}
	latest, e = a.Store.Node(n.ID, account)
	if e != nil {
		return e
	}
	state := "present"
	if latest.Trashed || latest.Revision != n.Revision {
		state = "drift"
	}
	if e = a.Store.Bind(account, n.ID, check.ID, state, check.Name, check.ParentID, check.Thumbnail); e != nil {
		return e
	}
	if state == "drift" {
		return wait("Local file changed during the operation; reconciling its latest path")
	}
	return nil
}
func (a *App) scan(ctx context.Context, c pikpak.Provider, j *Job) error {
	if e := a.checkLibrary(ctx, c, j); e != nil {
		return e
	}
	if !a.accountJobAllowed(j.AccountID, "recover") {
		return nil
	}
	_, e := a.enqueueTelDriveMissing(ctx, j, "")
	return e
}

func (a *App) checkLibrary(ctx context.Context, c pikpak.Provider, j *Job) error {
	// A listing and its final state commit must observe one directory layout.
	// Payload downloads/uploads do not hold this lock; only their final
	// reconciliation waits, so a stale scan cannot overwrite a completed bind.
	ctx, unlock := a.lockStructure(ctx)
	defer unlock()
	protected, e := scanProtectedNodes(a.Store.DB, j.AccountID)
	if e != nil {
		return e
	}
	ac, e := a.Store.Account(j.AccountID)
	if e != nil {
		return e
	}
	if ac.RootID == "" {
		return block("The account's library root is not prepared; verify the account first")
	}
	a.updateJobProgress(j, 2, "正在确认云端专用目录")
	me, e := c.Me(ctx)
	if e != nil {
		a.unknown(j.AccountID, protected)
		return e
	}
	if me.Sub != ac.Identity {
		a.unknown(j.AccountID, protected)
		return &pikpak.APIError{Status: 409, Code: "identity_changed"}
	}
	nodes, e := a.Store.AllNodes(j.AccountID)
	if e != nil {
		return e
	}
	root, e := c.Get(ctx, ac.RootID)
	if e != nil && !pikpak.Missing(e) {
		a.unknown(j.AccountID, protected)
		return e
	}
	rootMissing := pikpak.Missing(e) || (e == nil && root.Trashed)
	remote := map[string]pikpak.File{}
	byPath := map[string][]pikpak.File{}
	adopting := a.Store.Get("root_scan_pending:"+j.AccountID) == ac.RootID
	if !rootMissing {
		if !root.Folder() {
			return block("专用目录不是文件夹，未修改文件映射")
		}
		var walk func(string, string) error
		seen := map[string]bool{}
		walk = func(id, parentPath string) error {
			if seen[id] {
				return block("Remote directory cycle")
			}
			seen[id] = true
			files, e := listAll(ctx, c, id)
			if e != nil {
				return e
			}
			for _, f := range files {
				if _, exists := remote[f.ID]; exists || f.ID == ac.RootID || f.ParentID != id || f.Trashed {
					return block("云端目录树不一致，未修改文件映射")
				}
				if e = ValidName(f.Name); e != nil {
					return e
				}
				remote[f.ID] = f
				if len(remote)%20 == 0 {
					a.updateJobProgress(j, min(28, 3+len(remote)/4), fmt.Sprintf("正在扫描云端文件（已发现 %d 项）", len(remote)))
				}
				p := parentPath + "/" + f.Name
				byPath[p] = append(byPath[p], f)
				if f.Folder() {
					if e = walk(f.ID, p); e != nil {
						return e
					}
				}
			}
			return nil
		}
		if e = walk(ac.RootID, ""); e != nil {
			a.unknown(j.AccountID, protected)
			return e
		}
	}
	a.updateJobProgress(j, 30, fmt.Sprintf("云端目录扫描完成，共 %d 项，开始匹配本地记录", len(remote)))
	adopted, states, e := scanUnbound(nodes, byPath, ac.RootID)
	if e != nil {
		return e
	}
	for _, f := range adopted {
		if e = ctx.Err(); e != nil {
			return e
		}
		check, err := c.Get(ctx, f.ID)
		if err != nil {
			a.unknown(j.AccountID, protected)
			return err
		}
		if check.Trashed || !check.Complete() || check.Name != f.Name || check.ParentID != f.ParentID ||
			check.Kind != f.Kind || check.Size != f.Size || !strings.EqualFold(check.Hash, f.Hash) {
			return wait("扫描期间云端文件发生变化，正在重新核对全部文件")
		}
	}
	a.updateJobProgress(j, 45, fmt.Sprintf("已匹配 %d 项，开始核对文件状态", len(adopted)))
	for i, n := range nodes {
		if n.Trashed || n.RemoteID == "" || protected[n.ID] {
			continue
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		f, ok := remote[n.RemoteID]
		if !ok {
			f, e = c.Get(ctx, n.RemoteID)
			if e != nil {
				if pikpak.Missing(e) {
					states[n.ID] = "missing"
					continue
				}
				a.unknown(j.AccountID, protected)
				return e
			}
		}
		if f.Trashed {
			states[n.ID] = "missing"
			continue
		}
		parent := ac.RootID
		if n.ParentID != "root" {
			p, pe := a.Store.Node(n.ParentID, j.AccountID)
			if pe != nil {
				return pe
			}
			parent = p.RemoteID
		}
		state := "present"
		if !f.Complete() {
			state = "pending"
		} else if !compatible(n, f) {
			state = "conflict"
		} else if f.Name != n.Name || f.ParentID != parent || rootMissing {
			state = "drift"
		}
		states[n.ID] = state
		progress := 45 + (i+1)*50/max(len(nodes), 1)
		a.updateJobProgress(j, min(progress, 95), fmt.Sprintf("正在核对文件（%d/%d）", i+1, len(nodes)))
	}
	a.updateJobProgress(j, 95, "核对完成，正在保存结果")
	// No missing conclusions are committed until the entire relevant scan has succeeded.
	a.gate.RLock()
	defer a.gate.RUnlock()
	if a.active() != j.AccountID {
		return errPaused
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// Path matches include ancestor names. Any concurrent local edit invalidates
	// the proposed new bindings, including a rename of an ancestor alone.
	if len(adopted) > 0 || adopting {
		for _, n := range nodes {
			var revision int64
			if e = tx.QueryRow(`SELECT revision FROM nodes WHERE id=?`, n.ID).Scan(&revision); e != nil {
				return e
			}
			if revision != n.Revision {
				return wait("扫描期间本地目录发生变化，正在重新核对全部文件")
			}
		}
	}
	currentProtected, e := scanProtectedNodes(tx, j.AccountID)
	if e != nil {
		return e
	}
	for _, n := range nodes {
		state, ok := states[n.ID]
		if !ok || (protected[n.ID] || currentProtected[n.ID]) && !(adopting && n.RemoteID == "") {
			continue
		}
		if n.RemoteID == "" {
			f := adopted[n.ID]
			if _, e = tx.Exec(`INSERT INTO bindings(account_id,node_id,remote_id,state,remote_name,remote_parent,thumbnail,checked)
				SELECT ?,?,?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM nodes WHERE id=? AND revision=?)
				ON CONFLICT(account_id,node_id) DO UPDATE SET remote_id=excluded.remote_id,state=excluded.state,
				remote_name=excluded.remote_name,remote_parent=excluded.remote_parent,thumbnail=excluded.thumbnail,checked=excluded.checked
				WHERE bindings.remote_id=''`, j.AccountID, n.ID, f.ID, state, f.Name, f.ParentID, f.Thumbnail, now(), n.ID, n.Revision); e != nil {
				return e
			}
			continue
		}
		if _, e = tx.Exec(`UPDATE bindings SET state=?,checked=? WHERE account_id=? AND node_id=? AND remote_id=?
			AND EXISTS (SELECT 1 FROM nodes WHERE id=? AND revision=?)`, state, now(), j.AccountID, n.ID, n.RemoteID, n.ID, n.Revision); e != nil {
			return e
		}
	}
	if adopting {
		if _, e = tx.Exec(`DELETE FROM settings WHERE key=? AND value=?`, "root_scan_pending:"+j.AccountID, ac.RootID); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	_ = a.Store.Set("last_scan", strconv.FormatInt(now(), 10))
	if q, qe := c.Quota(ctx); qe == nil {
		current, err := a.currentClientAccount(j.AccountID, c)
		if err != nil {
			return err
		}
		a.recordAccountQuotaIfCurrent(j.AccountID, current.Secret, q)
	} else if status, _, _ := classifyAccountError(qe); status != "" {
		return qe
	}
	return nil
}
func (a *App) unknown(account string, protected map[string]bool) {
	// Called while checkLibrary holds the structure lock. Keep resumable
	// transfers and their implicitly rebuilt parents out of a failed scan too.
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return
	}
	defer tx.Rollback()
	currentProtected, e := scanProtectedNodes(tx, account)
	if e != nil {
		return
	}
	for id := range protected {
		currentProtected[id] = true
	}
	if e = scanUnknown(tx, account, currentProtected); e == nil {
		_ = tx.Commit()
	}
}

func ParseSource(link, pass string) (Source, error) {
	u, e := url.Parse(strings.TrimSpace(link))
	if e != nil {
		return Source{}, fmt.Errorf("invalid source link")
	}
	s := Source{ID: ID(), Created: now(), Selected: []string{}, Manifest: []Entry{}}
	if strings.EqualFold(u.Scheme, "magnet") {
		valid := false
		for _, xt := range u.Query()["xt"] {
			if strings.HasPrefix(strings.ToLower(xt), "urn:btih:") {
				hash := xt[9:]
				if len(hash) == 40 {
					_, err := hex.DecodeString(hash)
					valid = err == nil
				} else if len(hash) == 32 {
					valid = true
					for _, r := range strings.ToUpper(hash) {
						if !(r >= 'A' && r <= 'Z' || r >= '2' && r <= '7') {
							valid = false
						}
					}
				}
			} else if strings.HasPrefix(strings.ToLower(xt), "urn:btmh:1220") && len(xt) == 77 {
				valid = true
			}
		}
		if !valid {
			return s, fmt.Errorf("magnet link has no valid torrent hash")
		}
		s.Kind = "magnet"
		s.Link = u.String()
		return s, nil
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || (host != "mypikpak.com" && host != "www.mypikpak.com" && host != "mypikpak.net") {
		return s, fmt.Errorf("use a magnet link or a PikPak HTTPS share link")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "s" || parts[1] == "" {
		return s, fmt.Errorf("expected a PikPak /s/ share link")
	}
	s.Kind = "share"
	s.ShareID = parts[1]
	if len(parts) > 2 {
		s.ShareParent = parts[len(parts)-1]
	}
	u.RawQuery = ""
	u.Fragment = ""
	s.Link = u.String()
	return s, nil
}
func shareTree(ctx context.Context, c pikpak.Provider, s Source, pass string) ([]Entry, string, error) {
	entries := []Entry{}
	token := ""
	seen := map[string]bool{}
	var walk func(string, string, bool) error
	walk = func(parent, prefix string, top bool) error {
		if seen[parent] {
			return block("Share directory cycle")
		}
		seen[parent] = true
		next := ""
		pages := map[string]bool{}
		for {
			page, e := c.Share(ctx, s.ShareID, pass, token, parent, next)
			if e != nil {
				return e
			}
			if page.PassCodeToken != "" {
				token = page.PassCodeToken
			}
			for _, f := range page.Files {
				if top && len(s.Selected) > 0 && !contains(s.Selected, f.ID) {
					continue
				}
				if e = ValidName(f.Name); e != nil {
					return e
				}
				p := path.Join(prefix, f.Name)
				kind := "file"
				if f.Folder() {
					kind = "folder"
				}
				entries = append(entries, Entry{f.ID, p, f.Name, kind, int64(f.Size), f.Hash})
				if f.Folder() {
					if e = walk(f.ID, p, false); e != nil {
						return e
					}
				}
			}
			if page.Next == "" {
				break
			}
			if pages[page.Next] {
				return block("Incomplete share pagination")
			}
			pages[page.Next] = true
			next = page.Next
		}
		return nil
	}
	e := walk(s.ShareParent, "", true)
	return entries, token, e
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func (a *App) stage(ctx context.Context, c pikpak.Provider, account, name string) (string, error) {
	ctx, unlock := a.lockStructure(ctx)
	defer unlock()
	root, e := a.ensureRoot(ctx, c, account)
	if e != nil {
		return "", e
	}
	files, e := listAll(ctx, c, root)
	if e != nil {
		return "", e
	}
	matches := []pikpak.File{}
	for _, f := range files {
		if f.Name == name {
			matches = append(matches, f)
		}
	}
	if len(matches) > 1 {
		return "", block("Ambiguous task staging directory")
	}
	if len(matches) == 1 {
		if !matches[0].Folder() {
			return "", block("Task staging name is occupied")
		}
		return matches[0].ID, nil
	}
	f, e := c.Mkdir(ctx, root, name)
	return f.ID, e
}
func stableNode(source, p string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + p))
	return hex.EncodeToString(sum[:16])
}
func (a *App) importSource(ctx context.Context, c pikpak.Provider, j *Job, d *JobData) error {
	s, e := a.Store.Source(d.SourceID)
	if e != nil {
		return e
	}
	entries, e := a.materialize(ctx, c, j, d, s)
	if e != nil {
		return e
	}
	// Share manifests are captured before dispatch and contain publisher IDs.
	// Reload after materialization instead of replacing them with saved-copy IDs.
	s, e = a.Store.Source(d.SourceID)
	if e != nil {
		return e
	}
	if len(s.Manifest) == 0 {
		for _, r := range entries {
			kind := "file"
			if r.File.Folder() {
				kind = "folder"
			}
			s.Manifest = append(s.Manifest, Entry{r.File.ID, r.Path, r.File.Name, kind, int64(r.File.Size), r.File.Hash})
		}
		if e = a.Store.SaveSource(s); e != nil {
			return e
		}
	}
	parent := d.ParentID
	if parent == "" {
		parent = "root"
	}
	// Register the entire logical tree before moving any remote object.
	for _, r := range entries {
		id := stableNode(s.ID, r.Path)
		existing, err := a.Store.Node(id, j.AccountID)
		if err == nil {
			if existing.RemoteID == "" {
				if e = a.Store.Bind(j.AccountID, id, r.File.ID, "pending", r.File.Name, r.File.ParentID, r.File.Thumbnail); e != nil {
					return e
				}
			}
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		p := parent
		if dir := path.Dir(r.Path); dir != "." {
			p = stableNode(s.ID, dir)
		}
		kind := "file"
		if r.File.Folder() {
			kind = "folder"
		}
		n := Node{ID: id, ParentID: p, Name: r.File.Name, Kind: kind, Size: int64(r.File.Size), Hash: r.File.Hash, Mime: r.File.MimeType, SourceID: s.ID, SourcePath: r.Path, SourceKey: r.File.ID, Created: now(), Modified: now()}
		for _, m := range s.Manifest {
			if m.Path == r.Path {
				n.SourceKey = m.ID
			}
		}
		var count int
		_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent_id=? AND name=? AND trashed=0`, p, n.Name).Scan(&count)
		if count > 0 {
			ext := path.Ext(n.Name)
			n.Name = strings.TrimSuffix(n.Name, ext) + " (" + s.ID[:6] + ")" + ext
		}
		if e = a.Store.InsertNode(n); e != nil {
			return e
		}
		if e = a.Store.Bind(j.AccountID, id, r.File.ID, "pending", r.File.Name, r.File.ParentID, r.File.Thumbnail); e != nil {
			return e
		}
	}
	for i, r := range entries {
		n, e := a.Store.Node(stableNode(s.ID, r.Path), j.AccountID)
		if e != nil {
			return e
		}
		if n.Trashed {
			continue
		}
		p, e := a.folder(ctx, c, j.AccountID, n.ParentID, map[string]bool{})
		if e != nil {
			return e
		}
		f, e := c.Get(ctx, r.File.ID)
		if e != nil {
			return e
		}
		if e = a.align(ctx, c, j.AccountID, n, f, p); e != nil {
			return e
		}
		if n.Kind == "folder" {
			if cache := operation(ctx); cache != nil {
				cache.folders[n.ID] = resolvedFolder{f.ID, n.ParentID, n.Revision}
			}
		}
		j.Progress = (i + 1) * 100 / max(len(entries), 1)
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
	}
	return nil
}
func (a *App) jobNodes(j *Job, d *JobData) ([]Node, error) {
	if !d.ExactNodes {
		return a.Store.Descendants(d.NodeIDs, j.AccountID)
	}
	nodes := make([]Node, 0, len(d.NodeIDs))
	for _, id := range d.NodeIDs {
		n, e := a.Store.Node(id, j.AccountID)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, e
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func (a *App) recover(ctx context.Context, c pikpak.Provider, j *Job, d *JobData) error {
	nodes, e := a.jobNodes(j, d)
	if e != nil {
		return e
	}
	operations, e := nodeJobs(a.Store.DB, j.AccountID)
	if e != nil {
		return e
	}
	skipped := 0
	resumeFirst := map[string]bool{}
	if jobLane(*j) == telDriveLane {
		for _, n := range nodes {
			resumeFirst[n.ID] = a.telDriveResumeNode(*j, d, n.ID)
		}
	}
	sort.SliceStable(nodes, func(i, k int) bool {
		if (nodes[i].Kind == "folder") != (nodes[k].Kind == "folder") {
			return nodes[i].Kind == "folder"
		}
		return resumeFirst[nodes[i].ID] && !resumeFirst[nodes[k].ID]
	})
	for i, n := range nodes {
		if n.Trashed || d.Done[n.ID] {
			continue
		}
		active, e := localNodeActive(a.Store.DB, n.ID)
		if e != nil {
			return e
		}
		if !active {
			continue
		}
		// Recheck after folder expansion: old or previously previewed recovery
		// requests can contain children now owned by a resumable upload.
		if operation, ok := operations[n.ID]; ok && operation.JobID != j.ID {
			skipped++
			delete(d.Problems, n.ID)
			continue
		}
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
		err := a.restoreNode(ctx, c, j, d, n)
		if err != nil {
			var p *pending
			if errors.As(err, &p) || pikpak.Temporary(err) || errors.Is(err, errPaused) {
				return err
			}
			if status, _, _ := classifyAccountError(err); status != "" {
				return err
			}
			d.Problems[n.ID] = err.Error()
		} else {
			d.Done[n.ID] = true
			delete(d.Problems, n.ID)
		}
		j.Progress = (i + 1) * 100 / max(len(nodes), 1)
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
	}
	if skipped > 0 {
		d.Note = fmt.Sprintf("已跳过 %d 项已有传输或恢复任务的资源，请在原任务中继续或重试", skipped)
	}
	return nil
}
func (a *App) restoreNode(ctx context.Context, c pikpak.Provider, j *Job, d *JobData, n Node) error {
	if n.Kind == "folder" {
		_, e := a.folder(ctx, c, j.AccountID, n.ID, map[string]bool{})
		return e
	}
	parent, e := a.folder(ctx, c, j.AccountID, n.ParentID, map[string]bool{})
	if e != nil {
		return e
	}
	if n.RemoteID != "" {
		f, e := c.Get(ctx, n.RemoteID)
		if e == nil {
			if f.Trashed {
				if e = c.Untrash(ctx, []string{f.ID}); e != nil && !pikpak.Missing(e) {
					return e
				}
				if e == nil {
					f, e = c.Get(ctx, f.ID)
					if e != nil {
						return e
					}
					if f.Trashed {
						return wait("等待 PikPak 确认文件已从回收站还原")
					}
				}
			}
			if e == nil && !f.Trashed {
				return a.align(ctx, c, j.AccountID, n, f, parent)
			}
		} else if !pikpak.Missing(e) {
			return e
		}
	}
	// TelDrive upload tickets already attempt GCID deduplication. Keep their
	// upload session instead of creating a disposable instant-upload placeholder.
	if n.SourceID != "" {
		if source, err := a.Store.Source(n.SourceID); err == nil && source.Kind == "teldrive" {
			return a.uploadTelDrive(ctx, c, j, d, n)
		}
	}
	_, legacyInstant := d.InstantTried[n.ID]
	batchReady := d.Transfers[n.SourceID] != nil && d.Transfers[n.SourceID].Phase == "complete"
	if n.Hash != "" && !legacyInstant && !batchReady {
		id, err := a.instantDirect(ctx, c, j, d, n, parent)
		if err != nil {
			return err
		}
		if id != "" {
			f, err := c.Get(ctx, id)
			if err != nil {
				return err
			}
			return a.align(ctx, c, j.AccountID, n, f, parent)
		}
	}
	if n.Hash != "" && legacyInstant && !batchReady && d.Instant[n.ID] == "" {
		stage, e := a.stage(ctx, c, j.AccountID, ".vault-instant-"+operationJobID(j, d))
		if e != nil {
			return e
		}
		files, e := listAll(ctx, c, stage)
		if e != nil {
			return e
		}
		for _, f := range files {
			if f.Name == n.ID {
				d.InstantTried[n.ID] = true
				if compatible(n, f) && f.Complete() {
					d.Instant[n.ID] = f.ID
					break
				}
				if e = c.Trash(ctx, []string{f.ID}); e != nil {
					return e
				}
			}
		}
		if d.Instant[n.ID] == "" && !d.InstantTried[n.ID] {
			d.InstantTried[n.ID] = true
			if e = a.checkpoint(j, d); e != nil {
				return e
			}
			r, e := c.Instant(ctx, stage, pikpak.File{Name: n.ID, Size: pikpak.Number(n.Size), Hash: n.Hash})
			if e != nil {
				if pikpak.Temporary(e) {
					return e
				}
				if status, _, _ := classifyAccountError(e); status != "" {
					d.InstantTried[n.ID] = false
					return e
				}
			}
			if r.File != nil {
				d.Instant[n.ID] = r.File.ID
				if e = a.checkpoint(j, d); e != nil {
					return e
				}
				if !r.File.Complete() {
					if e = c.Trash(ctx, []string{r.File.ID}); e != nil {
						return e
					}
					delete(d.Instant, n.ID)
				}
			}
		}
	}
	if id := d.Instant[n.ID]; id != "" {
		f, e := c.Get(ctx, id)
		if e != nil {
			return e
		}
		if f.Complete() && !f.Trashed {
			return a.align(ctx, c, j.AccountID, n, f, parent)
		}
	}
	if n.SourceID == "" {
		return block("No recoverable source is saved for this file")
	}
	s, e := a.Store.Source(n.SourceID)
	if e != nil {
		return e
	}
	entries, e := a.materialize(ctx, c, j, d, s)
	if e != nil {
		return e
	}
	matches := []pikpak.File{}
	for _, entry := range entries {
		if entry.Path == n.SourcePath && compatible(n, entry.File) {
			matches = append(matches, entry.File)
		}
	}
	if len(matches) == 0 && n.Hash != "" {
		for _, entry := range entries {
			if !entry.File.Folder() && strings.EqualFold(n.Hash, entry.File.Hash) && n.Size == int64(entry.File.Size) {
				matches = append(matches, entry.File)
			}
		}
	}
	if len(matches) != 1 {
		return block("Source no longer uniquely matches the saved file: " + n.Name)
	}
	f, e := c.Get(ctx, matches[0].ID)
	if e != nil {
		return e
	}
	return a.align(ctx, c, j.AccountID, n, f, parent)
}
func (a *App) syncNodes(ctx context.Context, c pikpak.Provider, j *Job, d *JobData) error {
	nodes, e := a.jobNodes(j, d)
	if e != nil {
		return e
	}
	for _, n := range nodes {
		if d.Done[n.ID] {
			continue
		}
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
		if n.Trashed {
			if n.RemoteID != "" {
				f, e := c.Get(ctx, n.RemoteID)
				if e != nil && !pikpak.Missing(e) {
					return e
				}
				if e == nil && !f.Trashed {
					if e = c.Trash(ctx, []string{n.RemoteID}); e != nil {
						return e
					}
				}
			}
			_ = a.Store.State(j.AccountID, n.ID, "trashed")
		} else if n.Kind == "folder" {
			if _, e = a.folder(ctx, c, j.AccountID, n.ID, map[string]bool{}); e != nil {
				return e
			}
		} else if n.RemoteID != "" {
			p, e := a.folder(ctx, c, j.AccountID, n.ParentID, map[string]bool{})
			if e != nil {
				return e
			}
			f, e := c.Get(ctx, n.RemoteID)
			if e != nil {
				return e
			}
			if f.Trashed {
				if e = c.Untrash(ctx, []string{f.ID}); e != nil {
					return e
				}
				f, e = c.Get(ctx, f.ID)
				if e != nil {
					return e
				}
			}
			if e = a.align(ctx, c, j.AccountID, n, f, p); e != nil {
				return e
			}
		}
		d.Done[n.ID] = true
	}
	return nil
}

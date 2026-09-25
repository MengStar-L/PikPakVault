package vault

import (
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"pikpakvault/internal/rss"
)

type RSSSubscription struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	URL             string         `json:"url"`
	Secret          string         `json:"-"`
	ParentID        string         `json:"parent_id"`
	TargetPath      string         `json:"target_path"`
	IntervalMinutes int            `json:"interval_minutes"`
	Enabled         bool           `json:"enabled"`
	ImportExisting  bool           `json:"import_existing"`
	Initialized     bool           `json:"initialized"`
	LastChecked     int64          `json:"last_checked"`
	NextCheck       int64          `json:"next_check"`
	LastError       string         `json:"last_error"`
	ETag            string         `json:"-"`
	LastModified    string         `json:"-"`
	Created         int64          `json:"created"`
	LastJob         *Job           `json:"last_job"`
	Counts          map[string]int `json:"counts"`
}
type rssSecret struct {
	URL string `json:"url"`
}
type RSSEntry struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Published  int64  `json:"published"`
	Discovered int64  `json:"discovered"`
	State      string `json:"state"`
	Message    string `json:"message"`
	JobID      string `json:"job_id"`
	AccountID  string `json:"account_id"`
	SourceID   string `json:"source_id"`
}

const rssCols = `id,name,secret,parent_id,interval_minutes,enabled,import_existing,initialized,last_checked,next_check,last_error,etag,last_modified,created`
const rssStateSQL = `CASE WHEN e.job_id='' THEN e.state WHEN j.state='completed' THEN 'saved' WHEN j.state IN ('queued','running','waiting','retry') THEN 'pending' ELSE 'failed' END`

func rssScan(r scanner) (s RSSSubscription, err error) {
	err = r.Scan(&s.ID, &s.Name, &s.Secret, &s.ParentID, &s.IntervalMinutes, &s.Enabled, &s.ImportExisting, &s.Initialized, &s.LastChecked, &s.NextCheck, &s.LastError, &s.ETag, &s.LastModified, &s.Created)
	return
}
func (s *Store) rssSubscription(id string) (RSSSubscription, error) {
	return rssScan(s.DB.QueryRow(`SELECT `+rssCols+` FROM rss_subscriptions WHERE id=?`, id))
}
func (s *Store) rssSubscriptions() ([]RSSSubscription, error) {
	rows, err := s.DB.Query(`SELECT ` + rssCols + ` FROM rss_subscriptions ORDER BY created,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RSSSubscription{}
	for rows.Next() {
		v, e := rssScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (a *App) rssList(w http.ResponseWriter, r *http.Request) error {
	subs, err := a.Store.rssSubscriptions()
	if err != nil {
		return err
	}
	for i := range subs {
		s := &subs[i]
		var secret rssSecret
		if err = a.Store.Unseal(s.Secret, &secret); err != nil {
			return err
		}
		s.URL = secret.URL
		s.TargetPath, _ = a.Store.Path(s.ParentID)
		if s.TargetPath == "" {
			s.TargetPath = "目标目录已删除"
		}
		if j, e := jobScan(a.Store.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE kind='rss_scan' AND json_extract(data,'$.rss_id')=? ORDER BY created DESC,rowid DESC LIMIT 1`, s.ID)); e == nil {
			s.LastJob = &j
		}
		s.Counts = map[string]int{"total": 0, "saved": 0, "pending": 0, "failed": 0, "skipped": 0}
		rows, e := a.Store.DB.Query(`SELECT `+rssStateSQL+`,COUNT(*) FROM rss_entries e LEFT JOIN jobs j ON j.id=e.job_id WHERE e.subscription_id=? GROUP BY 1`, s.ID)
		if e != nil {
			return e
		}
		for rows.Next() {
			var state string
			var count int
			if e = rows.Scan(&state, &count); e != nil {
				break
			}
			s.Counts[state] = count
			s.Counts["total"] += count
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
	}
	writeJSON(w, 200, map[string]any{"subscriptions": subs, "active_account": a.active()})
	return nil
}
func (a *App) rssEntries(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if _, err := a.Store.rssSubscription(id); err != nil {
		return err
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(0, min(page, 1000000))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}
	limit = min(max(limit, 1), 100)
	var total int
	if err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM rss_entries WHERE subscription_id=?`, id).Scan(&total); err != nil {
		return err
	}
	rows, err := a.Store.DB.Query(`SELECT e.id,e.title,e.published,e.discovered,`+rssStateSQL+`,CASE WHEN e.job_id='' THEN e.message ELSE COALESCE(NULLIF(j.message,''),j.state,'原任务不存在') END,e.job_id,e.account_id,e.source_id FROM rss_entries e LEFT JOIN jobs j ON j.id=e.job_id WHERE e.subscription_id=? ORDER BY e.discovered DESC,e.rowid DESC LIMIT ? OFFSET ?`, id, limit, page*limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	entries := []RSSEntry{}
	for rows.Next() {
		var e RSSEntry
		if err = rows.Scan(&e.ID, &e.Title, &e.Published, &e.Discovered, &e.State, &e.Message, &e.JobID, &e.AccountID, &e.SourceID); err != nil {
			return err
		}
		entries = append(entries, e)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"entries": entries, "total": total, "page": page, "limit": limit})
	return nil
}

type rssInput struct {
	Name            *string `json:"name"`
	URL             *string `json:"url"`
	ParentID        *string `json:"parent_id"`
	IntervalMinutes *int    `json:"interval_minutes"`
	Enabled         *bool   `json:"enabled"`
	ImportExisting  *bool   `json:"import_existing"`
}

func (a *App) rssSave(w http.ResponseWriter, r *http.Request) error {
	var v rssInput
	if err := decode(r, &v); err != nil {
		return err
	}
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	id := r.PathValue("id")
	create := id == ""
	s := RSSSubscription{ID: ID(), ParentID: "root", IntervalMinutes: 30, Enabled: true, Created: now()}
	var oldURL string
	if !create {
		var err error
		s, err = a.Store.rssSubscription(id)
		if err != nil {
			return err
		}
		var secret rssSecret
		if err = a.Store.Unseal(s.Secret, &secret); err != nil {
			return err
		}
		oldURL = secret.URL
		s.URL = oldURL
	}
	if v.Name != nil {
		s.Name = strings.TrimSpace(*v.Name)
	}
	if v.URL != nil {
		s.URL = *v.URL
	}
	if v.ParentID != nil {
		s.ParentID = *v.ParentID
	}
	if v.IntervalMinutes != nil {
		s.IntervalMinutes = *v.IntervalMinutes
	}
	if v.Enabled != nil {
		s.Enabled = *v.Enabled
	}
	if v.ImportExisting != nil {
		s.ImportExisting = *v.ImportExisting
	}
	if ValidName(s.Name) != nil || len([]rune(s.Name)) > 200 {
		return fail(400, "请输入不超过 200 个字符的订阅名称")
	}
	var err error
	s.URL, err = rss.NormalizeURL(s.URL)
	if err != nil {
		return fail(400, err.Error())
	}
	if !create && s.URL != oldURL {
		return fail(409, "订阅地址已固定；更换来源请新建订阅")
	}
	if s.IntervalMinutes < 5 || s.IntervalMinutes > 10080 {
		return fail(400, "检查间隔应为 5–10080 分钟")
	}
	if s.ParentID == "" {
		s.ParentID = "root"
	}
	// Disabling a subscription must remain possible after its folder is removed.
	if create || v.ParentID != nil || s.Enabled {
		if err = a.rssTarget(s.ParentID); err != nil {
			return err
		}
	}
	s.Secret, err = a.Store.Seal(rssSecret{s.URL})
	if err != nil {
		return err
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if create {
		_, err = tx.Exec(`INSERT INTO rss_subscriptions(id,name,secret,parent_id,interval_minutes,enabled,import_existing,created) VALUES(?,?,?,?,?,?,?,?)`, s.ID, s.Name, s.Secret, s.ParentID, s.IntervalMinutes, s.Enabled, s.ImportExisting, s.Created)
	} else {
		_, err = tx.Exec(`UPDATE rss_subscriptions SET name=?,secret=?,parent_id=?,interval_minutes=?,enabled=?,import_existing=?,next_check=0 WHERE id=?`, s.Name, s.Secret, s.ParentID, s.IntervalMinutes, s.Enabled, s.ImportExisting, s.ID)
		if err == nil && !s.Enabled {
			_, err = tx.Exec(`UPDATE jobs SET state='cancelled',message='RSS 订阅已暂停' WHERE kind='rss_scan' AND json_extract(data,'$.rss_id')=? AND state IN ('queued','running','waiting','retry')`, s.ID)
		}
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.Store.Event("rss", map[string]string{"id": s.ID, "action": "save"})
	a.notify()
	writeJSON(w, 200, map[string]string{"id": s.ID})
	return nil
}
func (a *App) rssTarget(id string) error {
	if err := a.target(id); err != nil {
		return fail(400, "请选择存在的目标文件夹")
	}
	active, err := localNodeActive(a.Store.DB, id)
	if err != nil {
		return err
	}
	if !active {
		return fail(409, "目标文件夹或上级目录已在回收站中，请先还原或更换目录")
	}
	return nil
}
func (a *App) rssDelete(w http.ResponseWriter, r *http.Request) error {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	id := r.PathValue("id")
	if _, err := a.Store.rssSubscription(id); err != nil {
		return err
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE jobs SET state='cancelled',message='RSS 订阅已移除' WHERE kind='rss_scan' AND json_extract(data,'$.rss_id')=? AND state IN ('queued','running','waiting','retry','paused')`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM rss_entries WHERE subscription_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM rss_subscriptions WHERE id=?`, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.Store.Event("rss", map[string]string{"id": id, "action": "delete"})
	writeJSON(w, 200, map[string]bool{"ok": true})
	return nil
}
func (a *App) newRSSCheck(id, account string, manual bool) (Job, error) {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	s, err := a.Store.rssSubscription(id)
	if err != nil {
		return Job{}, err
	}
	if !manual && (!s.Enabled || s.NextCheck > now()) {
		return Job{}, nil
	}
	if j, e := jobScan(a.Store.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE kind='rss_scan' AND json_extract(data,'$.rss_id')=? AND account_id=? AND state IN ('queued','running','waiting','retry') LIMIT 1`, id, account)); e == nil {
		return j, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return Job{}, e
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	j, err := enqueueTx(tx, account, "rss_scan", "检查 RSS · "+s.Name, JobData{RSSID: id, RSSManual: manual})
	if err != nil {
		return j, err
	}
	if _, err = tx.Exec(`UPDATE rss_subscriptions SET next_check=? WHERE id=?`, now()+int64(s.IntervalMinutes)*60, id); err != nil {
		return j, err
	}
	return j, tx.Commit()
}
func (a *App) rssCheck(w http.ResponseWriter, r *http.Request) error {
	a.gate.RLock()
	defer a.gate.RUnlock()
	ac, err := a.Store.Account(a.active())
	if err != nil || ac.Status != "ready" {
		return fail(409, "请先连接并验证当前 PikPak 账号")
	}
	j, err := a.newRSSCheck(r.PathValue("id"), ac.ID, true)
	if err != nil {
		return err
	}
	a.notify()
	writeJSON(w, 202, j)
	return nil
}
func (a *App) scheduleRSS() {
	a.gate.RLock()
	defer a.gate.RUnlock()
	ac, err := a.Store.Account(a.active())
	if err != nil || ac.Status != "ready" {
		return
	}
	subs, err := a.Store.rssSubscriptions()
	if err != nil {
		return
	}
	for _, s := range subs {
		if s.Enabled && s.NextCheck <= now() {
			_, _ = a.newRSSCheck(s.ID, ac.ID, false)
		}
	}
}

func (a *App) scanRSS(ctx context.Context, j *Job, d *JobData) (err error) {
	s, err := a.Store.rssSubscription(d.RSSID)
	if err != nil {
		return err
	}
	if !s.Enabled && !d.RSSManual {
		return errPaused
	}
	var secret rssSecret
	if err = a.Store.Unseal(s.Secret, &secret); err != nil {
		return err
	}
	if err = a.checkpoint(j, d); err != nil {
		return err
	}
	result, err := rss.Fetch(ctx, a.RSSHTTP, secret.URL, s.ETag, s.LastModified)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, _ = a.Store.DB.Exec(`UPDATE rss_subscriptions SET last_checked=?,last_error=? WHERE id=?`, now(), err.Error(), s.ID)
		return err
	}
	// Network requests never hold the account/config locks. Commit all dedupe records,
	// sources and jobs together after checking current intent and account ownership.
	a.gate.RLock()
	defer a.gate.RUnlock()
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	current, err := a.Store.Job(j.ID)
	if err != nil {
		return err
	}
	if current.State == "cancelled" || current.State == "paused" || a.active() != j.AccountID {
		return errPaused
	}
	s, err = a.Store.rssSubscription(d.RSSID)
	if err != nil {
		return err
	}
	if !s.Enabled && !d.RSSManual {
		return errPaused
	}
	if err = a.rssTarget(s.ParentID); err != nil {
		_, _ = a.Store.DB.Exec(`UPDATE rss_subscriptions SET last_checked=?,last_error=? WHERE id=?`, now(), err.Error(), s.ID)
		return block(err.Error())
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	tx, err := a.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// File moves/trash use the database transaction rather than jobMu. Check
	// ancestry in this same snapshot so new tasks cannot race a local deletion.
	active, err := localNodeActive(tx, s.ParentID)
	if err != nil {
		return err
	}
	if !active {
		return block("目标文件夹或上级目录已删除，请还原或更换目录后重试")
	}
	queued, skipped, existing := 0, 0, 0
	for _, entry := range result.Entries {
		var count int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM rss_entries WHERE subscription_id=? AND entry_key=?`, s.ID, entry.Key).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			existing++
			continue
		}
		source, pass, resource := rssSource(entry)
		e := RSSEntry{ID: ID(), Title: rssTitle(entry.Title), Published: entry.Published, Discovered: now(), State: "skipped", Message: "未找到支持的磁链、PikPak 分享或下载附件"}
		duplicate := false
		if resource != "" {
			if err = tx.QueryRow(`SELECT COUNT(*) FROM rss_entries WHERE subscription_id=? AND resource_key=?`, s.ID, resource).Scan(&count); err != nil {
				return err
			}
			duplicate = count > 0
		}
		switch {
		case !s.Initialized && !s.ImportExisting:
			e.Message = "首次检查已记录历史条目，仅自动保存后续新增"
		case duplicate:
			e.Message = "相同资源已登记，跳过重复条目"
		case resource != "":
			source.Secret, err = a.Store.Seal(pass)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(`INSERT INTO sources(id,kind,link,share_id,share_parent,secret,selected,manifest,created) VALUES(?,?,?,?,?,?,?,?,?)`, source.ID, source.Kind, source.Link, source.ShareID, source.ShareParent, source.Secret, "[]", "[]", source.Created); err != nil {
				return err
			}
			job, e2 := enqueueTx(tx, j.AccountID, "import", e.Title, JobData{SourceID: source.ID, ParentID: s.ParentID, RSSID: s.ID})
			if e2 != nil {
				return e2
			}
			e.State, e.Message, e.JobID, e.AccountID, e.SourceID = "pending", "已加入传输任务", job.ID, j.AccountID, source.ID
			queued++
		}
		if e.State == "skipped" {
			skipped++
		}
		if _, err = tx.Exec(`INSERT INTO rss_entries(id,subscription_id,entry_key,resource_key,title,published,discovered,state,message,job_id,account_id,source_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, s.ID, entry.Key, resource, e.Title, e.Published, e.Discovered, e.State, e.Message, e.JobID, e.AccountID, e.SourceID); err != nil {
			return err
		}
	}
	if result.NotModified {
		if !s.Initialized {
			return block("首次读取订阅未返回内容，请重试")
		}
		d.Note = "订阅未更新，没有新增条目"
	} else {
		if _, err = tx.Exec(`UPDATE rss_subscriptions SET initialized=1,etag=?,last_modified=? WHERE id=?`, result.ETag, result.LastModified, s.ID); err != nil {
			return err
		}
		d.Note = fmt.Sprintf("新增 %d 个保存任务，跳过 %d 条，已登记 %d 条", queued, skipped, existing)
	}
	if _, err = tx.Exec(`UPDATE rss_subscriptions SET last_checked=?,next_check=?,last_error='' WHERE id=?`, now(), now()+int64(s.IntervalMinutes)*60, s.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.notify()
	return nil
}
func rssTitle(raw string) string {
	text := strings.Join(strings.Fields(raw), " ")
	r := []rune(text)
	if len(r) > 300 {
		text = string(r[:300]) + "…"
	}
	if text == "" {
		text = "RSS 新资源"
	}
	return text
}
func rssSource(entry rss.Entry) (Source, string, string) {
	for _, link := range entry.Links {
		u, err := url.Parse(link.URL)
		if err != nil {
			continue
		}
		pass := u.Query().Get("pass_code")
		if pass == "" {
			pass = u.Query().Get("pwd")
		}
		if pass == "" {
			pass = u.Query().Get("password")
		}
		s, err := ParseSource(link.URL, pass)
		if err == nil {
			identity := s.Link
			if s.Kind == "magnet" {
				identity = torrentIdentity(s.Link)
			}
			return s, pass, stableNode("rss-resource", identity)
		}
		if link.Attachment {
			if attachment, e := rssAttachmentSource(link.URL); e == nil {
				return attachment, "", stableNode("rss-resource", attachment.Link)
			}
		}
	}
	return Source{}, "", ""
}
func rssAttachmentSource(raw string) (Source, error) {
	link, err := rss.NormalizeURL(raw)
	if err != nil {
		return Source{}, err
	}
	return Source{ID: ID(), Kind: "url", Link: link, Selected: []string{}, Manifest: []Entry{}, Created: now()}, nil
}
func torrentIdentity(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	identities := []string{}
	for _, xt := range u.Query()["xt"] {
		x := strings.ToLower(xt)
		if strings.HasPrefix(x, "urn:btih:") && len(x) == 41 {
			if decoded, e := base32.StdEncoding.DecodeString(strings.ToUpper(x[9:])); e == nil {
				x = "urn:btih:" + hex.EncodeToString(decoded)
			}
		}
		if strings.HasPrefix(x, "urn:btih:") {
			return x
		}
		identities = append(identities, x)
	}
	sort.Strings(identities)
	return strings.Join(identities, ";")
}
func sameOfflineSource(a, b string) bool {
	if strings.HasPrefix(strings.ToLower(a), "magnet:") && strings.HasPrefix(strings.ToLower(b), "magnet:") {
		return sameMagnet(a, b) || torrentIdentity(a) == torrentIdentity(b)
	}
	one, e1 := rss.NormalizeURL(a)
	two, e2 := rss.NormalizeURL(b)
	return e1 == nil && e2 == nil && one == two
}

package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"pikpakvault/internal/aria2"
	"pikpakvault/internal/pikpak"
	"pikpakvault/internal/teldrive"
)

type TelDriveMonitor struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	FolderID    string `json:"folder_id"`
	FolderPath  string `json:"folder_path"`
	ParentID    string `json:"parent_id"`
	Secret      string `json:"-"`
	AutoMinutes int    `json:"auto_minutes"`
	LastRun     int64  `json:"last_run"`
	Created     int64  `json:"created"`
	TargetPath  string `json:"target_path"`
	LastJob     *Job   `json:"last_job"`
}
type telDriveAuth struct {
	Token string `json:"token"`
}
type telDriveSource struct {
	MonitorID string        `json:"monitor_id"`
	File      teldrive.File `json:"file"`
}
type TelDriveUpload struct {
	Attempted bool     `json:"attempted"`
	BeforeIDs []string `json:"before_ids,omitempty"`
	Parent    string   `json:"parent"`
	Name      string   `json:"name"`
	GCID      string   `json:"gcid"`
	RemoteID  string   `json:"remote_id"`
	Secret    string   `json:"secret,omitempty"` // Sealed UploadSession, including storage credentials.
	Polls     int      `json:"polls"`
}

const monitorCols = `id,name,base_url,folder_id,folder_path,parent_id,secret,auto_minutes,last_run,created`

func monitorScan(r scanner) (m TelDriveMonitor, e error) {
	e = r.Scan(&m.ID, &m.Name, &m.BaseURL, &m.FolderID, &m.FolderPath, &m.ParentID, &m.Secret, &m.AutoMinutes, &m.LastRun, &m.Created)
	return
}
func (s *Store) monitor(id string) (TelDriveMonitor, error) {
	return monitorScan(s.DB.QueryRow(`SELECT `+monitorCols+` FROM teldrive_monitors WHERE id=?`, id))
}
func (s *Store) monitors() ([]TelDriveMonitor, error) {
	rows, e := s.DB.Query(`SELECT ` + monitorCols + ` FROM teldrive_monitors ORDER BY created,id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []TelDriveMonitor{}
	for rows.Next() {
		m, e := monitorScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (a *App) telDriveClient(m TelDriveMonitor) (*teldrive.Client, error) {
	var auth telDriveAuth
	if e := a.Store.Unseal(m.Secret, &auth); e != nil {
		return nil, e
	}
	c, e := teldrive.New(m.BaseURL, auth.Token)
	if e == nil && a.TelDriveHTTP != nil {
		c.HTTP = a.TelDriveHTTP
	}
	return c, e
}
func (a *App) telDriveList(w http.ResponseWriter, r *http.Request) error {
	ms, e := a.Store.monitors()
	if e != nil {
		return e
	}
	for i := range ms {
		ms[i].TargetPath, _ = a.Store.Path(ms[i].ParentID)
		if j, e := jobScan(a.Store.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE kind='teldrive_scan' AND json_extract(data,'$.monitor_id')=? ORDER BY created DESC,rowid DESC LIMIT 1`, ms[i].ID)); e == nil {
			ms[i].LastJob = &j
		}
	}
	writeJSON(w, 200, map[string]any{"monitors": ms, "active_account": a.active()})
	return nil
}

type telDriveInput struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"base_url"`
	Token       string `json:"token"`
	FolderID    string `json:"folder_id"`
	FolderPath  string `json:"folder_path"`
	ParentID    string `json:"parent_id"`
	AutoMinutes int    `json:"auto_minutes"`
	Page        int    `json:"page"`
}

func (a *App) inputTelDrive(v telDriveInput) (*teldrive.Client, error) {
	if v.ID != "" && v.Token == "" {
		m, e := a.Store.monitor(v.ID)
		if e != nil {
			return nil, e
		}
		base, e := teldrive.NormalizeBase(v.BaseURL)
		if e != nil || base != m.BaseURL {
			return nil, fail(400, "更换站点需要重新填写认证信息")
		}
		return a.telDriveClient(m)
	}
	c, e := teldrive.New(v.BaseURL, v.Token)
	if e == nil && a.TelDriveHTTP != nil {
		c.HTTP = a.TelDriveHTTP
	}
	return c, e
}
func (a *App) telDriveBrowse(w http.ResponseWriter, r *http.Request) error {
	var v telDriveInput
	if e := decode(r, &v); e != nil {
		return e
	}
	c, e := a.inputTelDrive(v)
	if e != nil {
		return fail(400, e.Error())
	}
	if v.Page < 1 {
		v.Page = 1
	}
	if v.Page > 10000 {
		return fail(400, "页码超出范围")
	}
	p, e := c.List(r.Context(), v.FolderID, v.Page)
	if e != nil {
		return fail(502, e.Error())
	}
	writeJSON(w, 200, p)
	return nil
}
func (a *App) telDriveSave(w http.ResponseWriter, r *http.Request) error {
	var v telDriveInput
	if e := decode(r, &v); e != nil {
		return e
	}
	v.ID = r.PathValue("id")
	v.Name = strings.TrimSpace(v.Name)
	if e := ValidName(v.Name); e != nil {
		return fail(400, "请输入有效的监控名称")
	}
	if v.AutoMinutes != 0 && (v.AutoMinutes < 5 || v.AutoMinutes > 10080) {
		return fail(400, "自动同步间隔必须为 0 或 5–10080 分钟")
	}
	if len(v.FolderPath) > 4096 || len(v.FolderID) > 200 || len(v.Token) > 16384 {
		return fail(400, "配置过长")
	}
	if v.ParentID == "" {
		v.ParentID = "root"
	}
	n, e := a.Store.Node(v.ParentID, a.active())
	if e != nil || n.Kind != "folder" || n.Trashed {
		return fail(400, "目标文件夹不存在或已删除")
	}
	c, e := a.inputTelDrive(v)
	if e != nil {
		return fail(400, e.Error())
	}
	if v.ID != "" {
		old, e := a.Store.monitor(v.ID)
		if e != nil {
			return e
		}
		if c.Base != old.BaseURL || v.FolderID != old.FolderID || v.ParentID != old.ParentID {
			return fail(409, "已有监控的来源和目标已固定；如需更换路径，请关闭此规则并新建监控")
		}
	}
	if v.FolderID != "" {
		f, e := c.Get(r.Context(), v.FolderID)
		if e != nil {
			return fail(502, e.Error())
		}
		if f.Type != "folder" {
			return fail(400, "请选择 TelDrive 文件夹")
		}
	}
	if _, e = c.List(r.Context(), v.FolderID, 1); e != nil {
		return fail(502, e.Error())
	}
	secret, e := a.Store.Seal(telDriveAuth{c.Token})
	if e != nil {
		return e
	}
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	if v.ID == "" {
		var count int
		if e = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM teldrive_monitors WHERE base_url=? AND folder_id=? AND parent_id=?`, c.Base, v.FolderID, v.ParentID).Scan(&count); e != nil {
			return e
		}
		if count > 0 {
			return fail(409, "该来源和目标已有监控规则")
		}
		v.ID = ID()
		_, e = a.Store.DB.Exec(`INSERT INTO teldrive_monitors(`+monitorCols+`) VALUES(?,?,?,?,?,?,?,?,?,?)`, v.ID, v.Name, c.Base, v.FolderID, v.FolderPath, v.ParentID, secret, v.AutoMinutes, 0, now())
	} else {
		_, e = a.Store.DB.Exec(`UPDATE teldrive_monitors SET name=?,secret=?,auto_minutes=? WHERE id=?`, v.Name, secret, v.AutoMinutes, v.ID)
	}
	if e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 200, map[string]string{"id": v.ID})
	return nil
}
func (a *App) newTelDriveScan(m TelDriveMonitor, account string) (Job, error) {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	if j, e := jobScan(a.Store.DB.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE kind='teldrive_scan' AND account_id=? AND json_extract(data,'$.monitor_id')=? AND state IN ('queued','running','waiting','retry') LIMIT 1`, account, m.ID)); e == nil {
		return j, nil
	}
	j, e := a.Store.NewJob(account, "teldrive_scan", "扫描 TelDrive · "+m.Name, JobData{MonitorID: m.ID})
	if e == nil {
		_, e = a.Store.DB.Exec(`UPDATE teldrive_monitors SET last_run=? WHERE id=?`, now(), m.ID)
	}
	return j, e
}
func (a *App) telDriveSync(w http.ResponseWriter, r *http.Request) error {
	a.gate.RLock()
	defer a.gate.RUnlock()
	ac, e := a.Store.Account(a.active())
	if e != nil || ac.Status != "ready" {
		return fail(409, "请先连接并验证当前 PikPak 账号")
	}
	m, e := a.Store.monitor(r.PathValue("id"))
	if e != nil {
		return e
	}
	j, e := a.newTelDriveScan(m, ac.ID)
	if e != nil {
		return e
	}
	a.notify()
	writeJSON(w, 202, j)
	return nil
}
func (a *App) scheduleTelDrive() {
	ac, e := a.Store.Account(a.active())
	if e != nil || ac.Status != "ready" {
		return
	}
	ms, e := a.Store.monitors()
	if e != nil {
		return
	}
	for _, m := range ms {
		if m.AutoMinutes > 0 && now()-m.LastRun >= int64(m.AutoMinutes)*60 {
			_, _ = a.newTelDriveScan(m, ac.ID)
		}
	}
}
func sameTelDriveFile(a, b teldrive.File) bool {
	if a.ID != b.ID || a.Type != b.Type || a.Size != b.Size {
		return false
	}
	if a.Hash != "" {
		return a.Hash == b.Hash
	}
	return a.Updated != "" && a.Updated == b.Updated
}
func (a *App) scanTelDrive(ctx context.Context, remote pikpak.Provider, j *Job, d *JobData) error {
	m, e := a.Store.monitor(d.MonitorID)
	if e != nil {
		return e
	}
	c, e := a.telDriveClient(m)
	if e != nil {
		return e
	}
	d.Problems = map[string]string{}
	recovered := 0
	ac, e := a.Store.Account(j.AccountID)
	if e != nil {
		return e
	}
	if ac.RootID != "" {
		j.Message = "正在核对已登记资源的 PikPak 副本"
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
		if e = a.checkLibrary(ctx, remote, j); e != nil {
			return e
		}
		// Existing sources remain repairable even if their old local folder or
		// the TelDrive listing no longer contains them.
		recovered, e = a.enqueueTelDriveMissing(ctx, j, m.ID)
		if e != nil {
			return e
		}
	}
	j.Message = "正在完整读取 TelDrive 目录"
	if e = a.checkpoint(j, d); e != nil {
		return e
	}
	entries, e := c.Tree(ctx, m.FolderID)
	if e != nil {
		return e
	}
	// Discover new resources only after all source pages and directories are read.
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	current, e := a.Store.Job(j.ID)
	if e != nil {
		return e
	}
	if current.State == "paused" || current.State == "cancelled" || a.active() != j.AccountID {
		return errPaused
	}
	tx, e := a.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	operations, e := nodeJobs(tx, j.AccountID)
	if e != nil {
		return e
	}
	moving, e := pathJobs(tx, j.AccountID)
	if e != nil {
		return e
	}
	parents := map[string]string{}
	active, e := localNodeActive(tx, m.ParentID)
	if e != nil {
		return e
	}
	if active {
		parents[m.FolderID] = m.ParentID
	} else {
		d.Problems["target"] = "原同步目标已删除，暂停新资源登记；已移出资源仍按当前位置检查和恢复"
	}
	queued, existing := 0, 0
	directories := []string{}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		parent, mapped := parents[entry.ParentID]
		id := stableNode(m.ID, entry.ID)
		sourceID := stableNode("teldrive-source:"+m.ID, entry.ID)
		var foundID, kind, name string
		var deleted int
		err := tx.QueryRow(`SELECT id,kind,trashed,parent_id,name FROM nodes WHERE id=?`, id).Scan(&foundID, &kind, &deleted, &parent, &name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if deleted != 0 {
			continue
		}
		if foundID == "" && !mapped {
			continue
		}
		active, e := localNodeActive(tx, parent)
		if e != nil {
			return e
		}
		if !active {
			continue
		}
		if foundID == "" {
			if e = ValidName(entry.Name); e != nil {
				d.Problems[entry.Path] = e.Error()
				continue
			}
			name = entry.Name
			var conflict int
			if e = tx.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent_id=? AND name=? AND trashed=0`, parent, entry.Name).Scan(&conflict); e != nil {
				return e
			}
			if conflict > 0 {
				d.Problems[entry.Path] = "本地已有同名项目，请更换目标或整理重名后重试"
				continue
			}
			nodeSource := sourceID
			if entry.Type == "folder" {
				nodeSource = ""
			}
			if _, e = tx.Exec(`INSERT INTO nodes(id,parent_id,name,kind,size,mime,source_id,source_path,source_key,created,modified) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, parent, entry.Name, entry.Type, entry.Size, entry.Mime, nodeSource, entry.Path, entry.ID, now(), now()); e != nil {
				return e
			}
		} else if kind != entry.Type {
			d.Problems[entry.Path] = "来源项目类型发生变化"
			continue
		}
		// A pending folder recovery also owns newly discovered descendants.
		if operation, ok := operations[parent]; ok && operation.Kind == "recover" && !operation.Exact {
			if _, reserved := operations[id]; !reserved {
				operations[id] = operation
			}
		}
		if moving[parent] {
			moving[id] = true
		}
		if entry.Type == "folder" {
			parents[entry.ID] = id
			var bound int
			if e = tx.QueryRow(`SELECT COUNT(*) FROM bindings WHERE account_id=? AND node_id=? AND remote_id<>''`, j.AccountID, id).Scan(&bound); e != nil {
				return e
			}
			if _, pending := operations[id]; bound == 0 && !pending && !moving[id] {
				directories = append(directories, id)
			}
			continue
		}
		var oldSecret string
		err = tx.QueryRow(`SELECT secret FROM sources WHERE id=?`, sourceID).Scan(&oldSecret)
		if err == nil {
			var old telDriveSource
			if e = a.Store.Unseal(oldSecret, &old); e != nil {
				return e
			}
			if !sameTelDriveFile(old.File, entry.File) {
				d.Problems[entry.Path] = "TelDrive 源内容已变化，已保留原副本；请在来源使用新文件名/新文件 ID 保存新版本"
				continue
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			secret, e := a.Store.Seal(telDriveSource{m.ID, entry.File})
			if e != nil {
				return e
			}
			manifest := []Entry{{ID: entry.ID, Name: entry.Name, Path: entry.Path, Kind: "file", Size: entry.Size}}
			if _, e = tx.Exec(`INSERT INTO sources(id,kind,link,secret,manifest,created) VALUES(?,'teldrive',?,?,?,?)`, sourceID, m.BaseURL+"/files/"+entry.ID, secret, jsonText(manifest), now()); e != nil {
				return e
			}
		} else {
			return err
		}
		var remoteID string
		err = tx.QueryRow(`SELECT remote_id FROM bindings WHERE account_id=? AND node_id=?`, j.AccountID, id).Scan(&remoteID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if remoteID != "" {
			existing++
			continue
		}
		if _, pending := operations[id]; pending || moving[id] {
			existing++
			continue
		}
		if _, e = tx.Exec(`INSERT INTO jobs(id,account_id,kind,title,state,data,created,updated) VALUES(?,?,'teldrive_upload',?,'queued',?,?,?)`, ID(), j.AccountID, "上传 · "+name, jsonText(JobData{MonitorID: m.ID, SourceID: sourceID, ParentID: parent, NodeIDs: []string{id}}), now(), now()); e != nil {
			return e
		}
		queued++
	}
	// A single directory job also preserves empty folders. Existing folders are
	// resolved through their bindings without repeated moves or renames.
	if len(directories) > 0 {
		var unfinished int
		if e = tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE account_id=? AND kind='sync' AND json_extract(data,'$.monitor_id')=? AND state NOT IN ('completed','cancelled')`, j.AccountID, m.ID).Scan(&unfinished); e != nil {
			return e
		}
		if unfinished == 0 {
			if _, e = tx.Exec(`INSERT INTO jobs(id,account_id,kind,title,state,data,created,updated) VALUES(?,?,'sync',?,'queued',?,?,?)`, ID(), j.AccountID, "同步 TelDrive 文件夹 · "+m.Name, jsonText(JobData{MonitorID: m.ID, ExactNodes: true, NodeIDs: directories}), now(), now()); e != nil {
				return e
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	d.Note = fmt.Sprintf("已扫描 %d 项，新增上传 %d 项，缺失恢复 %d 项，已保存或已有任务跳过 %d 项，待处理 %d 项", len(entries), queued, recovered, existing, len(d.Problems))
	return nil
}

func (a *App) uploadTelDriveJob(ctx context.Context, c pikpak.Provider, j *Job, d *JobData) error {
	if len(d.NodeIDs) != 1 {
		return block("上传任务缺少本地文件记录")
	}
	n, e := a.Store.Node(d.NodeIDs[0], j.AccountID)
	if e != nil {
		return e
	}
	if n.Trashed {
		d.Note = "文件已进入本地回收站，跳过上传"
		return nil
	}
	return a.uploadTelDrive(ctx, c, j, d, n)
}

type progressReader struct {
	io.Reader
	read   int64
	tick   time.Time
	update func(int64) error
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, e := p.Reader.Read(b)
	p.read += int64(n)
	if time.Since(p.tick) > time.Second {
		p.tick = time.Now()
		if err := p.update(p.read); err != nil {
			return n, err
		}
	}
	return n, e
}
func (a *App) uploadTelDrive(ctx context.Context, c pikpak.Provider, j *Job, d *JobData, n Node) (result error) {
	defer func() {
		if result == nil {
			if err := aria2.Remove(a.telDriveCacheDir(j.ID, n.ID)); err != nil {
				d.Note += "；下载缓存清理未完成，可在 TelDrive 页面重试清理"
			}
		}
	}()
	if e := a.telDriveNodeActive(n.ID); e != nil {
		return e
	}
	uploader, ok := c.(pikpak.UploadProvider)
	if !ok {
		return block("当前 PikPak 适配器不支持文件上传")
	}
	s, e := a.Store.Source(n.SourceID)
	if e != nil {
		return e
	}
	var ref telDriveSource
	if e = a.Store.Unseal(s.Secret, &ref); e != nil {
		return e
	}
	m, e := a.Store.monitor(ref.MonitorID)
	if e != nil {
		return block("TelDrive 监控配置不存在，无法读取恢复来源")
	}
	td, e := a.telDriveClient(m)
	if e != nil {
		return e
	}
	if d.Uploads == nil {
		d.Uploads = map[string]*TelDriveUpload{}
	}
	u := d.Uploads[n.ID]
	if u == nil {
		u = &TelDriveUpload{}
		d.Uploads[n.ID] = u
	}
	var parent string
	// A saved result is authoritative even if the upload response was lost or
	// TelDrive later became unavailable. Verify it before touching the source.
	remoteID := u.RemoteID
	if remoteID == "" {
		remoteID = n.RemoteID
	}
	if remoteID != "" {
		f, err := c.Get(ctx, remoteID)
		if err == nil && !f.Trashed && f.Complete() {
			if u.GCID != "" {
				n.Hash = u.GCID
			}
			return a.finishTelDrive(ctx, c, j, d, n, f)
		}
		if err != nil && !pikpak.Missing(err) {
			return err
		}
		if u.RemoteID != "" && (pikpak.Missing(err) || f.Trashed) {
			return block("本次上传的远端文件已被删除；请取消本任务，再从恢复中心恢复")
		}
	}
	current, e := td.Get(ctx, ref.File.ID)
	if e != nil {
		return e
	}
	if !sameTelDriveFile(ref.File, current) {
		return block("TelDrive 源文件内容已变化，不能用新内容替代原资源")
	}
	if current.Size > int64(64<<20)*10000 {
		return block("文件超过当前上传上限（625 GiB）")
	}
	var session pikpak.UploadSession
	if u.Secret != "" {
		if e = a.Store.Unseal(u.Secret, &session); e != nil {
			return e
		}
	}
	var cached *os.File
	if !session.Sent {
		j.Message = "正在准备 aria2 下载缓存"
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
		cachePath, err := a.downloadTelDrive(ctx, j, d, n, td, current)
		if err != nil {
			return err
		}
		cached, e = os.Open(cachePath)
		if e != nil {
			return e
		}
		defer cached.Close()
		j.Message = "下载完成，正在校验本地文件指纹"
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
		pr := &progressReader{Reader: cached, update: func(read int64) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			if e := a.telDriveNodeActive(n.ID); e != nil {
				return e
			}
			j.Progress = 25 + int(read*5/max(current.Size, 1))
			j.Message = fmt.Sprintf("校验下载文件 · %s / %s", humanBytes(read), humanBytes(current.Size))
			return a.checkpoint(j, d)
		}}
		hash, err := pikpak.GCID(pr, current.Size)
		if err != nil {
			return fmt.Errorf("下载文件校验失败：%w", err)
		}
		if (n.Hash != "" && !strings.EqualFold(n.Hash, hash)) || (u.GCID != "" && !strings.EqualFold(u.GCID, hash)) {
			return block("下载文件指纹与已保存内容不符，请核对来源并清理缓存后重试")
		}
		check, err := td.Get(ctx, ref.File.ID)
		if err != nil {
			return err
		}
		if !sameTelDriveFile(current, check) {
			return block("TelDrive 文件在下载期间发生变化，请清理缓存并核对来源")
		}
		u.GCID = hash
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
	}
	n.Hash = u.GCID
	if u.Attempted && u.RemoteID == "" {
		files, e := listAll(ctx, c, u.Parent)
		if e != nil {
			return e
		}
		matches := []pikpak.File{}
		for _, f := range files {
			if !contains(u.BeforeIDs, f.ID) && f.Name == u.Name && compatible(n, f) {
				matches = append(matches, f)
			}
		}
		if len(matches) == 1 {
			u.RemoteID = matches[0].ID
			if e = a.checkpoint(j, d); e != nil {
				return e
			}
			if matches[0].Complete() {
				return a.finishTelDrive(ctx, c, j, d, n, matches[0])
			}
		}
		if len(matches) > 1 {
			return block("上传结果归属不唯一，请在 PikPak 核对同名文件")
		}
		if u.RemoteID == "" {
			u.Polls++
			if u.Polls <= 4 {
				return wait("上传请求响应丢失，正在核对目标目录，暂不重复创建文件")
			}
			return block("上传响应丢失且无法确认远端结果，已暂停；请先核对目标目录")
		}
	}
	if u.RemoteID == "" {
		n, parent, e = a.telDriveTarget(ctx, c, j.AccountID, n)
		if e != nil {
			return e
		}
		files, e := listAll(ctx, c, parent)
		if e != nil {
			return e
		}
		u.BeforeIDs = nil
		for _, f := range files {
			u.BeforeIDs = append(u.BeforeIDs, f.ID)
			if f.Name == n.Name {
				return block("PikPak 目标已有同名项目，已暂停上传以避免覆盖或重复")
			}
		}
		u.Parent, u.Name, u.Attempted = parent, n.Name, true
		j.Progress = 30
		j.Message = "申请 PikPak 上传凭证"
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
		session.Ticket, e = uploader.BeginUpload(ctx, parent, pikpak.File{Name: n.Name, Size: pikpak.Number(n.Size), Hash: n.Hash, MimeType: n.Mime})
		if e != nil {
			var up *pikpak.APIError
			if errors.As(e, &up) && (up.Status == 400 || up.Status == 401 || up.Status == 403 || up.Status == 429) {
				u.Attempted = false
			}
			return e
		}
		if session.Ticket.File == nil || session.Ticket.File.ID == "" {
			return block("PikPak 上传返回空文件 ID")
		}
		u.RemoteID = session.Ticket.File.ID
		u.Secret, e = a.Store.Seal(session)
		if e != nil {
			return e
		}
		if e = a.checkpoint(j, d); e != nil {
			return e
		}
		f := *session.Ticket.File
		if f.Name != n.Name || f.ParentID != parent || !compatible(n, f) {
			return block("PikPak 上传结果的名称、位置或内容不符，请核对远端结果")
		}
		if f.Complete() {
			return a.finishTelDrive(ctx, c, j, d, n, f)
		}
	}
	if u.Secret == "" {
		return block("已找到未完成的远端文件，但上传凭证响应丢失；请先在 PikPak 核对该文件")
	}
	if !session.Sent {
		read := func(ctx context.Context, offset, length int64) ([]byte, error) {
			if e := a.checkpoint(j, d); e != nil {
				return nil, e
			}
			if length == 0 {
				return []byte{}, nil
			}
			if e := ctx.Err(); e != nil {
				return nil, e
			}
			if e := a.telDriveNodeActive(n.ID); e != nil {
				return nil, e
			}
			if cached == nil || offset < 0 || length < 0 || length > 64<<20 || offset > n.Size || length > n.Size-offset {
				return nil, fmt.Errorf("本地上传缓存或读取范围无效")
			}
			b := make([]byte, int(length))
			if _, e := cached.ReadAt(b, offset); e != nil {
				return nil, e
			}
			return b, nil
		}
		save := func(sent int64) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if e := a.telDriveNodeActive(n.ID); e != nil {
				return e
			}
			var err error
			u.Secret, err = a.Store.Seal(session)
			if err != nil {
				return err
			}
			j.Progress = 30 + int(sent*65/max(n.Size, 1))
			j.Message = fmt.Sprintf("上传到 PikPak · %d / %d 字节", sent, n.Size)
			return a.checkpoint(j, d)
		}
		if e = uploader.ContinueUpload(ctx, &session, n.Size, read, save); e != nil {
			return e
		}
	}
	f, e := c.Get(ctx, u.RemoteID)
	if e != nil {
		return e
	}
	if !f.Complete() {
		u.Polls++
		if u.Polls > 20 {
			return block("上传已发送但 PikPak 尚未确认完成，请稍后重试核对")
		}
		return wait("文件内容已发送，正在核对 PikPak 保存结果")
	}
	return a.finishTelDrive(ctx, c, j, d, n, f)
}
func (a *App) finishTelDrive(ctx context.Context, c pikpak.Provider, j *Job, d *JobData, n Node, f pikpak.File) error {
	if e := a.telDriveNodeActive(n.ID); e != nil {
		return e
	}
	if !f.Complete() || f.Trashed || !compatible(n, f) || f.Hash == "" || !strings.EqualFold(f.Hash, n.Hash) {
		return block("上传后文件大小或指纹核对失败")
	}
	n, parent, e := a.telDriveTarget(ctx, c, j.AccountID, n)
	if e != nil {
		return e
	}
	if e := a.align(ctx, c, j.AccountID, n, f, parent); e != nil {
		return e
	}
	if _, e := a.Store.DB.Exec(`UPDATE nodes SET hash=? WHERE id=? AND (hash='' OR hash=?)`, f.Hash, n.ID, n.Hash); e != nil {
		return e
	}
	s, e := a.Store.Source(n.SourceID)
	if e != nil {
		return e
	}
	for i := range s.Manifest {
		s.Manifest[i].Hash = f.Hash
	}
	if e = a.Store.SaveSource(s); e != nil {
		return e
	}
	if u := d.Uploads[n.ID]; u != nil {
		u.Secret = ""
	}
	path, e := a.Store.Path(n.ID)
	if e != nil {
		return e
	}
	d.Note = "TelDrive 文件已保存至 " + path + "，大小与指纹核对通过"
	return nil
}

func (a *App) telDriveNodeActive(id string) error {
	active, e := localNodeActive(a.Store.DB, id)
	if e != nil {
		return e
	}
	if !active {
		return block("本地文件或父目录已删除或进入回收站，上传已暂停")
	}
	return nil
}

func rewrapTelDriveJobs(src, dst *Store, value string) (string, error) {
	var data map[string]json.RawMessage
	if e := json.Unmarshal([]byte(value), &data); e != nil {
		return "", e
	}
	raw := data["uploads"]
	if len(raw) == 0 {
		return value, nil
	}
	var uploads map[string]*TelDriveUpload
	if e := json.Unmarshal(raw, &uploads); e != nil {
		return "", e
	}
	for _, u := range uploads {
		if u == nil || u.Secret == "" {
			continue
		}
		var session pikpak.UploadSession
		if e := src.Unseal(u.Secret, &session); e != nil {
			return "", fmt.Errorf("备份密钥无法解密上传会话")
		}
		sealed, e := dst.Seal(session)
		if e != nil {
			return "", e
		}
		u.Secret = sealed
	}
	data["uploads"] = json.RawMessage(jsonText(uploads))
	return jsonText(data), nil
}

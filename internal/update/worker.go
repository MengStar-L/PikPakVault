package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const StateDir = "/var/lib/pikpak-vault-updater"
const DataDir = "/var/lib/pikpak-vault"
const Binary = "/opt/pikpakvalue/vault"
const Helper = "/usr/local/lib/pikpak-vault/maintenance"
const Unit = "pikpak-vault.service"
const RequestName = "update-request.json"

type Request struct {
	Tag   string `json:"tag"`
	Token string `json:"token"`
}
type Status struct {
	Request
	Phase    string `json:"phase"`
	Message  string `json:"message"`
	Updated  int64  `json:"updated"`
	Previous string `json:"previous"`
	Prepared bool   `json:"prepared"`
	Accepted bool   `json:"accepted"`
}

func (s Status) Busy() bool {
	return s.Phase != "" && s.Phase != "completed" && s.Phase != "failed" && s.Phase != "rolled_back"
}
func ReadStatus(dir string) Status {
	var s Status
	b, e := os.ReadFile(filepath.Join(dir, "status.json"))
	if e == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}
func Nonce() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func Atomic(name string, data []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(name), ".write-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), name); e != nil {
		return e
	}
	if runtime.GOOS == "linux" {
		d, e := os.Open(filepath.Dir(name))
		if e != nil {
			return e
		}
		defer d.Close()
		return d.Sync()
	}
	return nil
}
func Pending() bool {
	s := ReadStatus(StateDir)
	if token := os.Getenv("VAULT_UPDATE_TOKEN"); token != "" && (s.Token != token || !s.Accepted) {
		return true
	}
	return s.Phase == "stopping" || s.Phase == "installing" || s.Phase == "verifying" || s.Phase == "rolling_back"
}
func Available(data string) (bool, string) {
	if runtime.GOOS != "linux" {
		return false, "自动安装需要 Linux systemd；当前环境可检查版本并下载安装包。"
	}
	abs, _ := filepath.Abs(data)
	exe, _ := os.Executable()
	if abs != DataDir || exe != Binary || os.Getenv("VAULT_MANAGED") != "systemd" {
		return false, "请使用 Release 中的 deploy/install.sh 安装 systemd 服务，启用网页更新。"
	}
	for _, p := range []string{Helper, "/etc/systemd/system/pikpak-vault-update.path", "/etc/systemd/system/pikpak-vault-update.service"} {
		if _, e := os.Stat(p); e != nil {
			return false, "systemd 更新组件尚未安装，请执行安装包中的 deploy/enable-updates.sh。"
		}
	}
	return true, ""
}
func Submit(data, tag string) error {
	if !ValidTag(tag) {
		return fmt.Errorf("版本号无效")
	}
	if ReadStatus(StateDir).Busy() {
		return fmt.Errorf("已有更新正在执行")
	}
	b, _ := json.Marshal(Request{Tag: tag, Token: Nonce()})
	// Link publishes a completely written request with no overwrite window.
	f, e := os.CreateTemp(data, ".update-request-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Link(f.Name(), filepath.Join(data, RequestName))
}

// Worker runs in its own root-owned systemd service, outside the app's cgroup.
// All paths and commands are installation constants, never HTTP request fields.
type Worker struct {
	Dir, Data, Executable, HelperPath, Health string
	Client                                    *Client
	Control                                   func(context.Context, string) error
	Backup                                    func(string, string) error
	Restore                                   func(string, string) error
	Ready                                     func(context.Context, string, string, bool) error
}

func NewWorker() *Worker {
	w := &Worker{Dir: StateDir, Data: DataDir, Executable: Binary, HelperPath: Helper, Health: HealthURL(), Client: NewClient()}
	w.Control = func(ctx context.Context, action string) error {
		ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
		defer cancel()
		out, e := exec.CommandContext(ctx, "systemctl", action, Unit).CombinedOutput()
		if e != nil {
			return fmt.Errorf("systemctl %s: %s", action, strings.TrimSpace(string(out)))
		}
		return nil
	}
	w.Ready = w.ready
	return w
}
func (w *Worker) save(s *Status, phase, message string) error {
	s.Phase = phase
	s.Message = message
	s.Updated = time.Now().Unix()
	b, _ := json.Marshal(s)
	return Atomic(filepath.Join(w.Dir, "status.json"), b, 0644)
}
func (w *Worker) journal(s *Status) string { return filepath.Join(w.Dir, "backup-"+s.Token) }
func copyFile(src, dst string, mode os.FileMode) error {
	f, e := os.Open(src)
	if e != nil {
		return e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 512<<20))
	if e != nil {
		return e
	}
	return Atomic(dst, b, mode)
}

func (w *Worker) Run(ctx context.Context) error {
	if e := os.MkdirAll(w.Dir, 0755); e != nil {
		return e
	}
	lock := flock.New(filepath.Join(w.Dir, "worker.lock"))
	ok, e := lock.TryLock()
	if e != nil {
		return e
	}
	if !ok {
		return nil
	}
	defer lock.Unlock()
	s := ReadStatus(w.Dir)
	if s.Busy() {
		// A durable journal survives a killed updater or a reboot. Never guess that
		// an interrupted candidate succeeded based on a listening socket alone.
		_ = os.Remove(filepath.Join(w.Data, RequestName))
		return w.rollback(ctx, &s, fmt.Errorf("上次更新中断，正在恢复原版本"))
	}
	if s.Accepted {
		_ = os.Remove(filepath.Join(w.Dir, "startup.env"))
	}
	requestPath := filepath.Join(w.Data, RequestName)
	b, e := os.ReadFile(requestPath)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var req Request
	if len(b) > 4096 || json.Unmarshal(b, &req) != nil || !ValidTag(req.Tag) || len(req.Token) != 32 || strings.Trim(req.Token, "0123456789abcdef") != "" {
		_ = os.Remove(requestPath)
		return fmt.Errorf("更新请求无效")
	}
	s = Status{Request: req}
	if e = w.save(&s, "downloading", "正在从 GitHub 下载并校验安装包"); e != nil {
		return e
	}
	if e = os.Remove(requestPath); e != nil {
		return e
	}
	if e = w.install(ctx, &s); e != nil {
		return w.rollback(ctx, &s, e)
	}
	return nil
}
func (w *Worker) install(ctx context.Context, s *Status) error {
	r, e := w.Client.Release(ctx, s.Tag)
	if e != nil {
		return e
	}
	s.Previous = w.currentVersion(ctx)
	if !Newer(s.Tag, s.Previous) {
		return fmt.Errorf("目标版本必须高于正在运行的版本（%s）", s.Previous)
	}
	dir := w.journal(s)
	if e = os.Mkdir(dir, 0700); e != nil {
		return e
	}
	candidate := filepath.Join(dir, "vault.next")
	if e = w.Client.Download(ctx, r, candidate); e != nil {
		return e
	}
	if e = w.save(s, "stopping", "安装包校验通过，正在停止服务并备份全部数据"); e != nil {
		return e
	}
	if e = w.Control(ctx, "stop"); e != nil {
		return e
	}
	if e = w.Backup(w.Data, filepath.Join(dir, "data.zip")); e != nil {
		return fmt.Errorf("数据备份失败：%w", e)
	}
	if e = copyFile(w.Executable, filepath.Join(dir, "vault.previous"), 0700); e != nil {
		return e
	}
	s.Prepared = true
	if e = w.save(s, "installing", "备份已保存，正在安装新版本"); e != nil {
		return e
	}
	if e = Atomic(filepath.Join(w.Dir, "startup.env"), []byte("VAULT_UPDATE_TOKEN="+s.Token+"\n"), 0600); e != nil {
		return e
	}
	if e = copyFile(candidate, w.Executable, 0755); e != nil {
		return e
	}
	if e = w.save(s, "verifying", "正在启动新版本并检查版本、数据库与实例身份"); e != nil {
		return e
	}
	if e = w.Control(ctx, "start"); e != nil {
		return e
	}
	if e = w.Ready(ctx, strings.TrimPrefix(s.Tag, "v"), s.Token, true); e != nil {
		return e
	}
	// Upgrade the trusted helper for future schema versions only after the app,
	// which runs unprivileged, has passed the candidate readiness check.
	if e = copyFile(candidate, w.HelperPath, 0755); e != nil {
		return e
	}
	s.Accepted = true
	if e = w.save(s, "completed", "更新完成，服务已恢复；旧版本与数据备份已保留"); e != nil {
		return e
	}
	_ = os.Remove(filepath.Join(w.Dir, "startup.env"))
	return nil
}
func (w *Worker) rollback(ctx context.Context, s *Status, cause error) error {
	originalPhase := s.Phase
	if originalPhase != "downloading" && !s.Prepared {
		if e := w.Control(ctx, "stop"); e != nil {
			return e
		}
	}
	if s.Prepared {
		if e := wRollbackSave(w, s, cause); e != nil {
			return e
		}
		if e := w.Control(ctx, "stop"); e != nil {
			return e
		}
		dir := w.journal(s)
		if e := w.Restore(filepath.Join(dir, "data.zip"), w.Data); e != nil {
			return fmt.Errorf("回滚数据失败（保留维护状态）：%w", e)
		}
		if e := copyFile(filepath.Join(dir, "vault.previous"), w.Executable, 0755); e != nil {
			return e
		}
		if e := copyFile(filepath.Join(dir, "vault.previous"), w.HelperPath, 0755); e != nil {
			return e
		}
	}
	if originalPhase != "downloading" {
		if err := w.save(s, "rolling_back", "正在检查原版本服务："+cause.Error()); err != nil {
			return err
		}
		if err := Atomic(filepath.Join(w.Dir, "startup.env"), []byte("VAULT_UPDATE_TOKEN="+s.Token+"\n"), 0600); err != nil {
			return err
		}
		if err := w.Control(ctx, "start"); err != nil {
			return err
		}
		if err := w.Ready(ctx, s.Previous, s.Token, true); err != nil {
			return err
		}
	}
	s.Accepted = true
	phase := "failed"
	if s.Prepared {
		phase = "rolled_back"
	}
	if e := w.save(s, phase, cause.Error()); e != nil {
		return e
	}
	if e := os.Remove(filepath.Join(w.Dir, "startup.env")); e != nil && !os.IsNotExist(e) {
		return e
	}

	return nil
}
func wRollbackSave(w *Worker, s *Status, cause error) error {
	return w.save(s, "rolling_back", "更新失败，正在回滚："+cause.Error())
}

type health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Token   string `json:"update_token"`
	PID     string `json:"pid"`
	Ready   bool   `json:"ready"`
}

func (w *Worker) health(ctx context.Context) (health, error) {
	var h health
	req, e := http.NewRequestWithContext(ctx, "GET", w.Health, nil)
	if e != nil {
		return h, e
	}
	r, e := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if e != nil {
		return h, e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return h, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	e = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&h)
	return h, e
}
func (w *Worker) currentVersion(ctx context.Context) string {
	h, e := w.health(ctx)
	if e != nil {
		return ""
	}
	return h.Version
}
func (w *Worker) ready(ctx context.Context, version, token string, candidate bool) error {
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		h, e := w.health(ctx)
		if e == nil && h.Status == "ok" && h.Version == version && h.Token == token && h.PID != "" && (h.Ready || candidate) {
			out, e := exec.CommandContext(ctx, "systemctl", "show", Unit, "--property=MainPID", "--value").Output()
			if e == nil && strings.TrimSpace(string(out)) == h.PID {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("服务未通过版本、实例与数据库就绪检查")
		case <-tick.C:
		}
	}
}

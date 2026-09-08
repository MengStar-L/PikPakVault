package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"pikpakvault/internal/update"
)

func updatePending() bool { return update.Pending() }

type updateInfo struct {
	Current    string          `json:"current"`
	Repository string          `json:"repository"`
	Available  bool            `json:"available"`
	CanInstall bool            `json:"can_install"`
	Reason     string          `json:"reason"`
	Checked    int64           `json:"checked"`
	Error      string          `json:"error"`
	Release    *update.Release `json:"release"`
	Status     update.Status   `json:"status"`
}

func (a *App) updateInfo() updateInfo {
	v := updateInfo{Current: Version, Repository: update.Repository(), Status: update.ReadStatus(update.StateDir)}
	v.CanInstall, v.Reason = update.Available(a.Store.Dir)
	if _, e := os.Stat(filepath.Join(a.Store.Dir, update.RequestName)); e == nil && !v.Status.Busy() {
		v.Status.Phase = "queued"
		v.Status.Message = "更新请求已提交，等待 systemd 更新服务启动"
	}
	_ = json.Unmarshal([]byte(a.Store.Get("update_release")), &v.Release)
	if v.Release != nil {
		v.Available = update.Newer(v.Release.Tag, Version)
	}
	_ = json.Unmarshal([]byte(a.Store.Get("update_checked")), &v.Checked)
	v.Error = a.Store.Get("update_error")
	return v
}
func (a *App) updatesStatus(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, 200, a.updateInfo())
	return nil
}
func (a *App) updatesCheck(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	release, e := update.NewClient().Release(ctx, "")
	_ = a.Store.Set("update_checked", jsonText(now()))
	if e != nil {
		_ = a.Store.Set("update_error", e.Error())
	} else {
		_ = a.Store.Set("update_error", "")
		_ = a.Store.Set("update_release", jsonText(release))
	}
	writeJSON(w, 200, a.updateInfo())
	return nil
}
func (a *App) updatesInstall(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Tag     string `json:"tag"`
		Confirm bool   `json:"confirm"`
	}
	if e := decode(r, &v); e != nil {
		return e
	}
	if !v.Confirm {
		return fail(400, "请确认安装并重启服务")
	}
	if ok, reason := update.Available(a.Store.Dir); !ok {
		return fail(409, reason)
	}
	if !update.Newer(v.Tag, Version) {
		return fail(400, "目标版本必须高于当前版本")
	}
	if e := update.Submit(a.Store.Dir, v.Tag); e != nil {
		return fail(409, e.Error())
	}
	a.Store.Event("update_requested", map[string]string{"tag": v.Tag})
	writeJSON(w, 202, a.updateInfo())
	return nil
}

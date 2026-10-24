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
	"path/filepath"

	"pikpakvault/internal/aria2"
	"pikpakvault/internal/teldrive"
	"pikpakvault/internal/update"
)

type cacheOwner struct {
	JobID  string `json:"job_id"`
	NodeID string `json:"node_id"`
}

type diskSpaceWait struct {
	NodeID string `json:"node_id"`
	Size   int64  `json:"size"`
}

func markSpaceWait(d *JobData, nodeID string, size int64, err *aria2.InsufficientSpaceError) error {
	d.WaitReason = "disk_space"
	d.WaitSpace = &diskSpaceWait{NodeID: nodeID, Size: size}
	return &pending{err.Error() + "；等待磁盘空间，释放后自动继续（每 60 秒检查）", 60}
}

// A persisted space wait can be checked using only the local disk. In-flight
// upload intents still reach PikPak so an already-saved result can free cache.
func (a *App) recheckDownloadSpace(j *Job, d *JobData) error {
	waiting, reason := d.WaitSpace, d.WaitReason
	d.WaitReason, d.WaitSpace = "", nil
	if reason != "disk_space" || waiting == nil || (j.Kind != "teldrive_upload" && j.Kind != "recover") || d.RecoveryLane == cloudLane {
		return nil
	}
	if j.AccountID != a.active() {
		return errPaused
	}
	n, err := a.Store.Node(waiting.NodeID, j.AccountID)
	if err != nil {
		return err
	}
	active, err := localNodeActive(a.Store.DB, n.ID)
	if err != nil {
		return err
	}
	if !active || d.Done[n.ID] || n.Size != waiting.Size {
		return nil
	}
	for id := range d.Uploads {
		if a.telDriveResumeNode(*j, d, id) {
			if active, err := localNodeActive(a.Store.DB, id); err == nil && active {
				return nil
			}
		}
	}
	dir := a.telDriveCacheDir(j.ID, n.ID)
	if _, err = os.Stat(dir); err == nil {
		if err = aria2.Validate(dir); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = aria2.CheckSpace(dir, waiting.Size); err != nil {
		var low *aria2.InsufficientSpaceError
		if errors.As(err, &low) {
			return markSpaceWait(d, n.ID, waiting.Size, low)
		}
		return err
	}
	return nil
}

func (a *App) telDriveCacheDir(job, node string) string {
	return filepath.Join(aria2.ProgramDir(), "downloads", a.Store.Get("instance"), stableNode(job, node))
}

func (a *App) downloadTelDrive(ctx context.Context, j *Job, d *JobData, n Node, c *teldrive.Client, f teldrive.File) (string, error) {
	dir := a.telDriveCacheDir(j.ID, n.ID)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	if e := aria2.Validate(dir); e != nil {
		return "", e
	}
	body, _ := json.Marshal(cacheOwner{j.ID, n.ID})
	if e := update.Atomic(filepath.Join(dir, "owner.json"), body, 0600); e != nil {
		return "", e
	}
	if e := aria2.CheckSpace(dir, f.Size); e != nil {
		var low *aria2.InsufficientSpaceError
		if errors.As(e, &low) {
			return "", markSpaceWait(d, n.ID, f.Size, low)
		}
		return "", e
	}
	identity := stableNode(n.SourceID, jsonText(f))
	j.Message = "正在准备专用 aria2，首次使用会自动下载并校验"
	if e := a.checkpoint(j, d); e != nil {
		return "", e
	}
	bin, e := aria2.Ensure(ctx, aria2.ProgramDir())
	if e != nil {
		return "", e
	}
	if e = a.checkpoint(j, d); e != nil {
		return "", e
	}
	file, e := aria2.Fetch(ctx, dir, aria2.Source{Binary: bin, Identity: identity, Size: f.Size, Open: func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		return c.OpenRange(ctx, f, offset, length)
	}}, func(p aria2.Progress) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := a.telDriveNodeActive(n.ID); e != nil {
			return e
		}
		j.Progress = int(p.Completed * 25 / max(p.Total, 1))
		j.Message = fmt.Sprintf("aria2 下载 · %s / %s · %s/s", humanBytes(p.Completed), humanBytes(p.Total), humanBytes(p.Speed))
		return a.checkpoint(j, d)
	})
	if e != nil {
		var low *aria2.InsufficientSpaceError
		if errors.As(e, &low) {
			return "", markSpaceWait(d, n.ID, f.Size, low)
		}
	}
	return file, e
}

func humanBytes(v int64) string {
	if v >= 1<<30 {
		return fmt.Sprintf("%.2f GiB", float64(v)/(1<<30))
	}
	if v >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(v)/(1<<20))
	}
	if v >= 1<<10 {
		return fmt.Sprintf("%.1f KiB", float64(v)/(1<<10))
	}
	return fmt.Sprintf("%d B", v)
}

type downloadCache struct {
	Bytes       int64 `json:"bytes"`
	Files       int   `json:"files"`
	Reclaimable int64 `json:"reclaimable"`
	Available   bool  `json:"aria2_available"`
}

func (a *App) cacheStatus(clean bool) (downloadCache, error) {
	var out downloadCache
	_, err := aria2.Installed(aria2.ProgramDir())
	out.Available = err == nil
	root := filepath.Join(aria2.ProgramDir(), "downloads", a.Store.Get("instance"))
	entries, e := os.ReadDir(root)
	if errors.Is(e, os.ErrNotExist) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if aria2.Validate(dir) != nil {
			continue
		}
		var owner cacheOwner
		b, e := os.ReadFile(filepath.Join(dir, "owner.json"))
		if e != nil || json.Unmarshal(b, &owner) != nil || stableNode(owner.JobID, owner.NodeID) != entry.Name() {
			continue
		}
		j, e := a.Store.Job(owner.JobID)
		// Running includes in-flight requests even after a pause/cancel click;
		// HTTP cleanup also takes dataMu exclusively before touching cache files.
		eligible := errors.Is(e, sql.ErrNoRows) || e == nil && !contains([]string{"queued", "running", "waiting", "retry"}, j.State)
		if clean && eligible {
			if e = aria2.Remove(dir); e != nil {
				return out, e
			}
			continue
		}
		var size int64
		for _, name := range []string{"content", "content.aria2"} {
			if info, e := os.Stat(filepath.Join(dir, name)); e == nil {
				size += info.Size()
			}
		}
		out.Bytes += size
		out.Files++
		if eligible {
			out.Reclaimable += size
		}
	}
	return out, nil
}

func (a *App) telDriveCacheGet(w http.ResponseWriter, r *http.Request) error {
	v, e := a.cacheStatus(false)
	if e != nil {
		return e
	}
	writeJSON(w, 200, v)
	return nil
}

func (a *App) telDriveCacheClear(w http.ResponseWriter, r *http.Request) error {
	// Handler holds dataMu exclusively, proving all upload readers have exited.
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	v, e := a.cacheStatus(true)
	if e != nil {
		return e
	}
	writeJSON(w, 200, v)
	return nil
}

package vault

import (
	"context"
	"errors"
	"strings"

	"pikpakvault/internal/pikpak"
)

type InstantState struct {
	Phase   string   `json:"phase"`
	Parent  string   `json:"parent"`
	Name    string   `json:"name"`
	ID      string   `json:"id"`
	Before  []string `json:"before,omitempty"`
	Started int64    `json:"started"`
}

// Hash hits are created with the final name in the final parent. The durable
// intent precedes the write; no hit creates a transient task directory.
func (a *App) instantDirect(ctx context.Context, c pikpak.Provider, j *Job, d *JobData, n Node, parent string) (string, error) {
	t := d.InstantStates[n.ID]
	if t == nil {
		t = &InstantState{}
		d.InstantStates[n.ID] = t
	}
	if t.Phase == "miss" {
		return "", nil
	}
	if t.Phase == "" {
		files, err := listAll(ctx, c, parent)
		if err != nil {
			return "", err
		}
		t.Before = nil
		for _, f := range files {
			if f.Name == n.Name {
				return "", block("Name conflict: " + n.Name)
			}
			t.Before = append(t.Before, f.ID)
		}
		t.Phase, t.Parent, t.Name, t.Started = "dispatching", parent, n.Name, now()
		if err = a.checkpoint(j, d); err != nil {
			return "", err
		}
		r, err := c.Instant(ctx, parent, pikpak.File{Name: n.Name, Size: pikpak.Number(n.Size), Hash: n.Hash})
		if err != nil {
			var up *pikpak.APIError
			if errors.As(err, &up) && (up.Status == 401 || up.Status == 429 || up.Code == "verification_required") {
				t.Phase = ""
				return "", err
			}
			if errors.As(err, &up) && up.Status >= 400 && up.Status < 500 {
				t.Phase = "miss"
				return "", nil
			}
			return "", err
		}
		if r.File != nil {
			t.ID = r.File.ID
		}
		if t.ID == "" && len(r.Files) == 1 {
			t.ID = r.Files[0].ID
		}
		if t.ID == "" && len(r.FileIDs) == 1 {
			t.ID = r.FileIDs[0]
		}
		if err = a.checkpoint(j, d); err != nil {
			return "", err
		}
	}
	if t.ID == "" {
		files, err := listAll(ctx, c, t.Parent)
		if err != nil {
			return "", err
		}
		matches := []string{}
		for _, f := range files {
			if !contains(t.Before, f.ID) && f.Name == t.Name && !f.Folder() && f.Hash != "" && strings.EqualFold(n.Hash, f.Hash) && int64(f.Size) == n.Size {
				matches = append(matches, f.ID)
			}
		}
		if len(matches) > 1 {
			return "", block("秒传响应丢失且结果不唯一，已停止重复写入，请核对云端文件")
		}
		if len(matches) == 0 {
			if now()-t.Started < 120 {
				return "", wait("正在核对秒传响应，暂不重复转存")
			}
			return "", block("无法确认秒传结果，请核对云端后重新发起恢复")
		}
		t.ID = matches[0]
	}
	if contains(t.Before, t.ID) {
		return "", block("秒传返回了原有对象，已停止处理")
	}
	f, err := c.Get(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if f.Trashed {
		t.Phase = "miss"
		return "", nil
	}
	if f.Complete() {
		if !compatible(n, f) {
			return "", block("秒传结果与记录不一致")
		}
		t.Phase = "complete"
		return f.ID, nil
	}
	// An unfinished resumable-upload placeholder cannot be filled by this app.
	// Only the positively identified new object is recycled, then replay source.
	if err = a.checkpoint(j, d); err != nil {
		return "", err
	}
	if err = c.Trash(ctx, []string{f.ID}); err != nil {
		return "", err
	}
	t.Phase = "miss"
	return "", nil
}

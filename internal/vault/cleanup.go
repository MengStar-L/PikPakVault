package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"pikpakvault/internal/pikpak"
)

// Discover completed jobs locally, at most every 30 minutes. Each job is offered
// for automatic cleanup once; retained directories are not repeatedly probed.
func (a *App) scheduleCleanup() {
	account := a.active()
	ac, err := a.Store.Account(account)
	if err != nil || ac.Status != "ready" {
		return
	}
	last, _ := strconv.ParseInt(a.Store.Get("cleanup_check:"+account), 10, 64)
	if now()-last < 1800 {
		return
	}
	rows, err := a.Store.DB.Query(`SELECT id,data FROM jobs WHERE account_id=? AND state='completed' AND kind IN ('import','recover') AND NOT EXISTS (SELECT 1 FROM jobs c,json_each(c.data,'$.cleanup_jobs') ref WHERE c.kind='cleanup' AND ref.value=jobs.id)`, account)
	if err != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		var raw []byte
		var d JobData
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		if json.Unmarshal(raw, &d) != nil {
			continue
		}
		for _, transfer := range d.Transfers {
			if transfer != nil && transfer.StageID != "" {
				ids = append(ids, id)
				break
			}
		}
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil || readErr != nil {
		return
	}
	if len(ids) > 0 {
		if _, err = a.Store.NewJob(account, "cleanup", "整理已完成任务的空暂存目录", JobData{CleanupJobs: ids}); err != nil {
			return
		}
	}
	_ = a.Store.Set("cleanup_check:"+account, strconv.FormatInt(now(), 10))
}

func (a *App) cleanup(ctx context.Context, c pikpak.Provider, j *Job, d *JobData) error {
	ac, err := a.Store.Account(j.AccountID)
	if err != nil {
		return err
	}
	if d.CleanupResults == nil {
		d.CleanupResults = map[string]string{}
	}
	// Any unfinished job reference (including a paused job) protects the folder.
	protected := map[string]bool{}
	rows, err := a.Store.DB.Query(`SELECT data FROM jobs WHERE account_id=? AND kind<>'cleanup' AND state<>'completed'`, j.AccountID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		var active JobData
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if json.Unmarshal(raw, &active) != nil {
			err = fmt.Errorf("无法读取未完成任务，已停止目录清理")
			break
		}
		for _, t := range active.Transfers {
			if t != nil {
				protected[t.StageID] = true
				protected[t.TargetID] = true
			}
		}
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	ready := []string{}
	for _, sourceJobID := range d.CleanupJobs {
		owner, err := a.Store.Job(sourceJobID)
		if err != nil {
			return err
		}
		if owner.AccountID != j.AccountID || owner.State != "completed" {
			continue
		}
		var source JobData
		if err = json.Unmarshal(owner.Data, &source); err != nil {
			return err
		}
		for sourceID, t := range source.Transfers {
			if t == nil || t.StageID == "" || d.Done[t.StageID] {
				continue
			}
			id := t.StageID
			retain := func(reason string) { d.CleanupResults[id] = reason; d.Done[id] = true }
			if protected[id] || id == ac.RootID {
				retain("保留：仍有任务引用")
				continue
			}
			var bindings int
			if err = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM bindings WHERE account_id=? AND remote_id=?`, j.AccountID, id).Scan(&bindings); err != nil {
				return err
			}
			if bindings != 0 {
				retain("保留：已关联本地资源")
				continue
			}
			f, err := c.Get(ctx, id)
			if pikpak.Missing(err) || err == nil && f.Trashed {
				retain("已不在云端目录中")
				continue
			}
			if err != nil {
				return err
			}
			name, parent := t.StageName, t.StageParent
			if name == "" {
				if len(sourceID) < 8 {
					retain("保留：旧任务记录不完整")
					continue
				}
				name, parent = ".vault-task-"+owner.ID+"-"+sourceID[:8], ac.RootID
			}
			if !f.Folder() || f.Name != name || f.ParentID != parent {
				retain("保留：目录名称或位置已变化")
				continue
			}
			files, err := listAll(ctx, c, id)
			if err != nil {
				return err
			} // incomplete pagination is never treated as empty
			if len(files) > 0 {
				retain("保留：目录中仍有文件")
				continue
			}
			ready = append(ready, id)

		}
	}
	// Send small batches instead of one mutation per folder. A failed response
	// is reconciled by ID/trashed state on the next execution.
	for len(ready) > 0 {
		batch := ready[:min(len(ready), 100)]
		if err = a.checkpoint(j, d); err != nil {
			return err
		}
		if err = c.Trash(ctx, batch); err != nil {
			return err
		}
		for _, id := range batch {
			check, err := c.Get(ctx, id)
			if err != nil && !pikpak.Missing(err) {
				return err
			}
			if err == nil && !check.Trashed {
				return wait("等待 PikPak 确认空目录已移入回收站")
			}
			d.Done[id] = true
			d.CleanupResults[id] = "空目录已移入 PikPak 回收站"
		}
		if err = a.checkpoint(j, d); err != nil {
			return err
		}
		ready = ready[len(batch):]
	}
	cleaned, kept := 0, 0
	for _, result := range d.CleanupResults {
		if result == "空目录已移入 PikPak 回收站" {
			cleaned++
		} else {
			kept++
		}
	}
	d.Note = fmt.Sprintf("已整理 %d 个空目录；另有 %d 项已检查并保留或无需处理。", cleaned, kept)
	return nil
}

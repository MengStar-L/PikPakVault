package vault

import (
	"context"
	"fmt"
	"sort"

	"pikpakvault/internal/pikpak"
)

type PathChange struct {
	ID     string `json:"id"`
	Before string `json:"before"`
	After  string `json:"after"`
}
type ReconcileReport struct {
	AccountID    string            `json:"account_id"`
	RootID       string            `json:"root_id"`
	Scanned      int               `json:"scanned"`
	Matched      int               `json:"matched"`
	AddedParents int               `json:"added_parents"`
	Applied      bool              `json:"applied"`
	Changes      []PathChange      `json:"changes"`
	Skipped      map[string]string `json:"skipped"`
}

// Explicit maintenance only. It reads the account tree and adopts the paths of
// known object IDs; it never moves/renames/creates anything in PikPak. The CLI
// holds the service lock so no job can race this one-time local transaction.
func (a *App) ReconcilePaths(ctx context.Context, apply bool) (ReconcileReport, error) {
	report := ReconcileReport{AccountID: a.active(), Changes: []PathChange{}, Skipped: map[string]string{}}
	c, err := a.client(report.AccountID)
	if err != nil {
		return report, err
	}
	ac, err := a.Store.Account(report.AccountID)
	if err != nil {
		return report, err
	}
	report.RootID = ac.RootID
	me, err := c.Me(ctx)
	if err != nil {
		return report, err
	}
	if ac.Identity == "" || me.Sub != ac.Identity || ac.RootID == "" {
		return report, block("账号或专用目录尚未确认")
	}
	root, err := c.Get(ctx, ac.RootID)
	if err != nil {
		return report, err
	}
	if !root.Folder() || root.Trashed {
		return report, block("专用目录不可用，未修改本地路径")
	}
	remote := map[string]pikpak.File{root.ID: root}
	paths := map[string]string{root.ID: ""}
	var walk func(string) error
	walk = func(id string) error {
		files, err := listAll(ctx, c, id)
		if err != nil {
			return err
		}
		for _, f := range files {
			if _, seen := remote[f.ID]; seen || f.ParentID != id || f.Trashed {
				return block("云端目录树不一致，未修改本地路径")
			}
			if err = ValidName(f.Name); err != nil {
				return err
			}
			remote[f.ID] = f
			paths[f.ID] = paths[id] + "/" + f.Name
			report.Scanned++
			if f.Folder() {
				if err = walk(f.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = walk(root.ID); err != nil {
		return report, err
	}
	nodes, err := a.Store.AllNodes(ac.ID)
	if err != nil {
		return report, err
	}
	byRemote := map[string]Node{}
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
		if n.RemoteID != "" {
			byRemote[n.RemoteID] = n
		}
	}
	targets := map[string]Node{}
	states := map[string]string{}
	for _, n := range nodes {
		if n.Trashed || n.RemoteID == "" {
			continue
		}
		f, ok := remote[n.RemoteID]
		if !ok {
			f, err = c.Get(ctx, n.RemoteID)
			if pikpak.Missing(err) || err == nil && f.Trashed {
				report.Skipped[n.ID] = "远端已删除，保留原路径"
				states[n.ID] = "missing"
				continue
			}
			if err != nil {
				return report, err
			}
			report.Skipped[n.ID] = "文件位于专用目录之外，保留原映射"
			states[n.ID] = "drift"
			continue
		}
		if !compatible(n, f) {
			report.Skipped[n.ID] = "远端内容与原记录不一致"
			states[n.ID] = "conflict"
			continue
		}
		if !f.Complete() {
			report.Skipped[n.ID] = "文件尚未完整保存"
			states[n.ID] = "pending"
			continue
		}
		targets[n.ID] = n
	}
	// Add only the ancestors needed to represent tracked files, not unrelated
	// account contents or staging leftovers. Preserve IDs for known folders.
	newNodes := map[string]Node{}
	var parentID func(string) (string, error)
	parentID = func(id string) (string, error) {
		if id == root.ID {
			return "root", nil
		}
		f, ok := remote[id]
		if !ok || !f.Folder() {
			return "", block("无法确定文件的云端父目录")
		}
		if n, ok := byRemote[id]; ok {
			if n.Trashed {
				return "", block("云端父目录对应本地主动删除的项目，请先核对回收站")
			}
			if n.Kind != "folder" {
				return "", block("云端父目录与本地类型冲突")
			}
			if _, ok = targets[n.ID]; !ok {
				return "", block("云端父目录未通过核验")
			}
			return n.ID, nil
		}
		local := stableNode("adopt-parent:"+ac.ID, id)
		if _, ok := newNodes[local]; ok {
			return local, nil
		}
		p, err := parentID(f.ParentID)
		if err != nil {
			return "", err
		}
		n := Node{ID: local, ParentID: p, Name: f.Name, Kind: "folder", RemoteID: id, Created: now(), Modified: now(), Revision: 1}
		newNodes[local] = n
		return local, nil
	}
	for id, n := range targets {
		f := remote[n.RemoteID]
		p, err := parentID(f.ParentID)
		if err != nil {
			return report, err
		}
		check, err := c.Get(ctx, f.ID)
		if err != nil {
			return report, err
		}
		if check.Trashed || check.Name != f.Name || check.ParentID != f.ParentID || !compatible(n, check) || !check.Complete() {
			return report, block("扫描期间云端文件发生变化，未修改本地路径")
		}
		n.ParentID, n.Name = p, f.Name
		targets[id] = n
		states[id] = "present"
		report.Matched++
	}
	for _, n := range newNodes {
		f := remote[n.RemoteID]
		check, err := c.Get(ctx, f.ID)
		if err != nil {
			return report, err
		}
		if check.Trashed || !check.Folder() || check.ParentID != f.ParentID || check.Name != f.Name {
			return report, block("扫描期间云端父目录发生变化，未修改本地路径")
		}
	}
	// Detect collisions against the complete proposed local tree, including
	// missing/unbound records that the remote scan cannot safely relocate.
	final := map[string]Node{}
	for id, n := range byID {
		final[id] = n
	}
	for id, n := range targets {
		final[id] = n
	}
	for id, n := range newNodes {
		final[id] = n
	}
	names := map[string]string{}
	for id, n := range final {
		if n.Trashed {
			continue
		}
		key := n.ParentID + "\x00" + n.Name
		if other, ok := names[key]; ok && other != id {
			return report, block("校正后会产生同名路径冲突，未修改本地数据：" + n.Name)
		}
		names[key] = id
		seen := map[string]bool{}
		p := id
		for p != "root" && p != "" {
			if seen[p] {
				return report, block("校正后目录循环，未修改本地数据")
			}
			seen[p] = true
			ancestor, ok := final[p]
			if !ok {
				return report, block("校正后父目录缺失")
			}
			p = ancestor.ParentID
		}
	}
	for id, n := range targets {
		before, err := a.Store.Path(id)
		if err != nil {
			return report, err
		}
		after := paths[n.RemoteID]
		if before != after {
			report.Changes = append(report.Changes, PathChange{id, before, after})
		}
	}
	sort.Slice(report.Changes, func(i, j int) bool { return report.Changes[i].After < report.Changes[j].After })
	report.AddedParents = len(newNodes)
	if !apply {
		return report, nil
	}
	if err = ctx.Err(); err != nil {
		return report, err
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	var active string
	if err = tx.QueryRow(`SELECT value FROM settings WHERE key='active_account'`).Scan(&active); err != nil {
		return report, err
	}
	if active != ac.ID {
		return report, errPaused
	}
	for _, n := range nodes {
		var revision int64
		if err = tx.QueryRow(`SELECT revision FROM nodes WHERE id=?`, n.ID).Scan(&revision); err != nil {
			return report, err
		}
		if revision != n.Revision {
			return report, block("校正期间本地资源发生变化，请重试")
		}
	}
	for _, n := range newNodes {
		if _, err = tx.Exec(`INSERT INTO nodes(id,parent_id,name,kind,created,modified) VALUES(?,?,?,'folder',?,?)`, n.ID, n.ParentID, n.Name, n.Created, n.Modified); err != nil {
			return report, err
		}
		f := remote[n.RemoteID]
		if _, err = tx.Exec(`INSERT INTO bindings(account_id,node_id,remote_id,state,remote_name,remote_parent,checked) VALUES(?,?,?,'present',?,?,?)`, ac.ID, n.ID, f.ID, f.Name, f.ParentID, now()); err != nil {
			return report, err
		}
	}
	for id, n := range targets {
		old := byID[id]
		f := remote[n.RemoteID]
		if old.ParentID != n.ParentID || old.Name != n.Name {
			if _, err = tx.Exec(`UPDATE nodes SET parent_id=?,name=?,revision=revision+1,modified=? WHERE id=?`, n.ParentID, n.Name, now(), id); err != nil {
				return report, err
			}
			if _, err = tx.Exec(`UPDATE bindings SET state='unknown' WHERE node_id=? AND account_id<>?`, id, ac.ID); err != nil {
				return report, err
			}
		}
		if _, err = tx.Exec(`UPDATE bindings SET remote_name=?,remote_parent=?,checked=? WHERE account_id=? AND node_id=?`, f.Name, f.ParentID, now(), ac.ID, id); err != nil {
			return report, err
		}
	}
	for id, state := range states {
		if _, err = tx.Exec(`UPDATE bindings SET state=?,checked=? WHERE account_id=? AND node_id=?`, state, now(), ac.ID, id); err != nil {
			return report, err
		}
	}
	// Imports/recoveries retain their source checkpoints; explicit sync jobs
	// from before the maintenance stay paused rather than undoing manual edits.
	if _, err = tx.Exec(`UPDATE jobs SET state='paused',message='云端路径已人工校正，请核对后再继续此任务' WHERE account_id=? AND kind IN ('sync','trash','recover','import') AND state IN ('queued','waiting','retry','running')`, ac.ID); err != nil {
		return report, err
	}
	if _, err = tx.Exec(`INSERT INTO events(kind,detail,created) VALUES('paths_reconciled',?,?)`, jsonText(map[string]any{"account_id": ac.ID, "changes": len(report.Changes), "parents": len(newNodes)}), now()); err != nil {
		return report, err
	}
	if err = tx.Commit(); err != nil {
		return report, err
	}
	report.Applied = true
	return report, nil
}

func (r ReconcileReport) Summary() string {
	return fmt.Sprintf("扫描 %d 项，匹配 %d 项，路径变化 %d 项，补建本地父目录 %d 项，保留待处理 %d 项，已应用=%t", r.Scanned, r.Matched, len(r.Changes), r.AddedParents, len(r.Skipped), r.Applied)
}

package vault

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"pikpakvault/internal/pikpak"
)

type cloudAdoptionPlan struct {
	file   pikpak.File
	parent string
	id     string
	depth  int
}

// adoptCloud records files already present in the account's dedicated root.
// It is deliberately read-only against PikPak: the local transaction creates
// the nodes and bindings only after the complete remote tree has been checked.
func (a *App) adoptCloud(ctx context.Context, c pikpak.Provider, j *Job, d *JobData) error {
	ctx, unlock := a.lockStructure(ctx)
	defer unlock()
	ac, err := a.Store.Account(j.AccountID)
	if err != nil {
		return err
	}
	if ac.RootID == "" {
		return block("The account's library root is not prepared; verify the account first")
	}
	a.updateJobProgress(j, 2, "正在读取 PikPak 专用目录")
	me, err := c.Me(ctx)
	if err != nil {
		return err
	}
	if ac.Identity == "" || me.Sub != ac.Identity {
		return &pikpak.APIError{Status: 409, Code: "identity_changed"}
	}
	root, err := c.Get(ctx, ac.RootID)
	if err != nil {
		return err
	}
	if root.Trashed || !root.Folder() {
		return block("专用目录不可用，未接管云端文件")
	}

	remote := map[string]pikpak.File{}
	seen := map[string]bool{}
	var walk func(string) error
	walk = func(parent string) error {
		if seen[parent] {
			return block("云端目录存在循环，未接管文件")
		}
		seen[parent] = true
		files, err := listAll(ctx, c, parent)
		if err != nil {
			return err
		}
		sort.Slice(files, func(i, k int) bool { return files[i].ID < files[k].ID })
		for _, f := range files {
			if f.ID == "" || f.ParentID != parent || f.Trashed {
				return block("云端目录树不一致，未接管文件")
			}
			if _, exists := remote[f.ID]; exists || f.ID == ac.RootID {
				return block("云端目录树包含重复文件，未接管文件")
			}
			if err := ValidName(f.Name); err != nil {
				return err
			}
			remote[f.ID] = f
			if len(remote)%20 == 0 {
				a.updateJobProgress(j, min(28, 4+len(remote)/4), fmt.Sprintf("正在读取云端文件（已发现 %d 项）", len(remote)))
			}
			if f.Folder() {
				if err := walk(f.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = walk(ac.RootID); err != nil {
		return err
	}
	a.updateJobProgress(j, 30, fmt.Sprintf("云端目录读取完成，共 %d 项，正在核对变更", len(remote)))

	nodes, err := a.Store.AllNodes(j.AccountID)
	if err != nil {
		return err
	}
	byID := map[string]Node{}
	byRemote := map[string]Node{}
	existingPaths := map[string]string{}
	for _, n := range nodes {
		byID[n.ID] = n
		if n.RemoteID != "" {
			byRemote[n.RemoteID] = n
		}
		if !n.Trashed {
			key := n.ParentID + "\x00" + n.Name
			if previous, ok := existingPaths[key]; ok && previous != n.ID {
				return block("本地目录存在同名项目，未接管云端文件：" + n.Name)
			}
			existingPaths[key] = n.ID
		}
	}

	plans := map[string]*cloudAdoptionPlan{}
	var resolveParent func(string) (string, int, error)
	resolveParent = func(remoteID string) (string, int, error) {
		if remoteID == ac.RootID {
			return "root", 0, nil
		}
		if n, ok := byRemote[remoteID]; ok {
			if n.Trashed || n.Kind != "folder" {
				return "", 0, block("云端父目录与本地记录冲突，未接管文件")
			}
			return n.ID, 0, nil
		}
		if p, ok := plans[remoteID]; ok {
			return p.id, p.depth, nil
		}
		f, ok := remote[remoteID]
		if !ok || !f.Folder() {
			return "", 0, block("无法确定云端文件的父目录，未接管文件")
		}
		parent, depth, err := resolveParent(f.ParentID)
		if err != nil {
			return "", 0, err
		}
		id := stableNode("cloud-adopt:"+j.AccountID, remoteID)
		if old, exists := byID[id]; exists && old.RemoteID != remoteID {
			return "", 0, block("本地节点标识冲突，未接管文件：" + f.Name)
		}
		p := &cloudAdoptionPlan{file: f, parent: parent, id: id, depth: depth + 1}
		plans[remoteID] = p
		return id, p.depth, nil
	}

	remoteIDs := make([]string, 0, len(remote))
	for id := range remote {
		remoteIDs = append(remoteIDs, id)
	}
	sort.Strings(remoteIDs)
	for i, id := range remoteIDs {
		f := remote[id]
		if _, already := byRemote[id]; already {
			continue
		}
		parent, depth, err := resolveParent(f.ParentID)
		if err != nil {
			return err
		}
		// resolveParent creates an unbound folder plan when it is needed by a
		// child; create every remote item eagerly here so empty folders are also
		// represented.
		if _, exists := plans[id]; !exists {
			stable := stableNode("cloud-adopt:"+j.AccountID, id)
			if old, collision := byID[stable]; collision && old.RemoteID != id {
				return block("本地节点标识冲突，未接管文件：" + f.Name)
			}
			plans[id] = &cloudAdoptionPlan{file: f, parent: parent, id: stable, depth: depth + 1}
		}
		if i%20 == 0 {
			a.updateJobProgress(j, min(55, 32+i*20/max(len(remote), 1)), fmt.Sprintf("正在整理云端目录（%d/%d）", i+1, len(remote)))
		}
	}

	// A local path occupied by any existing node is a real conflict. Refuse the
	// complete adoption so one click cannot produce a partially merged tree.
	for _, p := range plans {
		key := p.parent + "\x00" + p.file.Name
		if existing, ok := existingPaths[key]; ok && existing != p.id {
			return block("本地目录存在同名项目，未接管云端文件：" + p.file.Name)
		}
		if _, ok := plans[p.file.ID]; !ok {
			// Every plan is keyed by its remote ID; this guard keeps malformed
			// provider responses from creating an orphan node.
			return block("云端目录计划不完整，未接管文件")
		}
		existingPaths[key] = p.id
	}

	// Validate the listing again before publishing local bindings. This keeps a
	// concurrent rename/move/upload from being recorded under stale metadata.
	for i, id := range remoteIDs {
		f := remote[id]
		check, err := c.Get(ctx, id)
		if err != nil {
			return err
		}
		if check.Trashed || check.Name != f.Name || check.ParentID != f.ParentID || check.Kind != f.Kind || check.Size != f.Size || f.Hash != "" && check.Hash != "" && !strings.EqualFold(f.Hash, check.Hash) || !check.Complete() {
			return wait("扫描期间云端文件发生变化，请重新接管")
		}
		if i%20 == 0 {
			a.updateJobProgress(j, min(75, 58+i*15/max(len(remote), 1)), fmt.Sprintf("正在确认云端文件（%d/%d）", i+1, len(remote)))
		}
	}

	a.gate.RLock()
	defer a.gate.RUnlock()
	if a.active() != j.AccountID {
		return errPaused
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active string
	if err = tx.QueryRow(`SELECT value FROM settings WHERE key='active_account'`).Scan(&active); err != nil {
		return err
	}
	if active != j.AccountID {
		return errPaused
	}

	ordered := make([]*cloudAdoptionPlan, 0, len(plans))
	for _, p := range plans {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, k int) bool {
		if ordered[i].depth != ordered[k].depth {
			return ordered[i].depth < ordered[k].depth
		}
		return ordered[i].id < ordered[k].id
	})
	for _, p := range ordered {
		f := p.file
		if _, err = tx.Exec(`INSERT INTO nodes(id,parent_id,name,kind,size,hash,mime,created,modified) VALUES(?,?,?,?,?,?,?,?,?)`, p.id, p.parent, f.Name, map[bool]string{true: "folder", false: "file"}[f.Folder()], int64(f.Size), f.Hash, f.MimeType, now(), now()); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO bindings(account_id,node_id,remote_id,state,remote_name,remote_parent,thumbnail,checked) VALUES(?,?,?,?,?,?,?,?)`, j.AccountID, p.id, f.ID, "present", f.Name, f.ParentID, f.Thumbnail, now()); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	d.Note = fmt.Sprintf("已将 %d 项 PikPak 云端文件加入资源管理", len(plans))
	a.updateJobProgress(j, 95, "已核对云端文件，正在保存接管结果")
	return nil
}

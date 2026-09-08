package vault

import (
	"context"
	"database/sql"
	"strings"

	"pikpakvault/internal/pikpak"
)

type rowQuerier interface {
	QueryRow(string, ...any) *sql.Row
}

func localNodeActive(q rowQuerier, id string) (bool, error) {
	var deleted, rooted int
	e := q.QueryRow(`WITH RECURSIVE ancestors(id,parent_id,trashed) AS (
		SELECT id,parent_id,trashed FROM nodes WHERE id=?
		UNION SELECT n.id,n.parent_id,n.trashed FROM nodes n JOIN ancestors p ON n.id=p.parent_id
	) SELECT COALESCE(SUM(trashed),0),COALESCE(SUM(id='root'),0) FROM ancestors`, id).Scan(&deleted, &rooted)
	return deleted == 0 && rooted == 1, e
}

// Empty monitorID means the regular library scan: only automatic monitors may
// repair. An explicit monitor scan authorizes repair for that monitor as well.
func (a *App) enqueueTelDriveMissing(ctx context.Context, j *Job, monitorID string) (int, error) {
	monitors, e := a.Store.monitors()
	if e != nil {
		return 0, e
	}
	selected := map[string]TelDriveMonitor{}
	for _, m := range monitors {
		if m.ID == monitorID || monitorID == "" && m.AutoMinutes > 0 {
			selected[m.ID] = m
		}
	}
	if len(selected) == 0 {
		return 0, nil
	}
	rows, e := a.Store.DB.Query(`SELECT DISTINCT s.id,s.secret FROM sources s JOIN nodes n ON n.source_id=s.id
		JOIN bindings b ON b.node_id=n.id AND b.account_id=?
		WHERE s.kind='teldrive' AND b.state='missing' AND n.trashed=0`, j.AccountID)
	if e != nil {
		return 0, e
	}
	sources := map[string]telDriveSource{}
	for rows.Next() {
		var id, secret string
		if e = rows.Scan(&id, &secret); e != nil {
			break
		}
		var ref telDriveSource
		if e = a.Store.Unseal(secret, &ref); e != nil {
			break
		}
		if _, ok := selected[ref.MonitorID]; ok {
			sources[id] = ref
		}
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return 0, e
	}
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	current, e := a.Store.Job(j.ID)
	if e != nil {
		return 0, e
	}
	if current.State == "paused" || current.State == "cancelled" || a.active() != j.AccountID {
		return 0, errPaused
	}
	tx, e := a.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	operations, e := nodeJobs(tx, j.AccountID)
	if e != nil {
		return 0, e
	}
	moving, e := pathJobs(tx, j.AccountID)
	if e != nil {
		return 0, e
	}
	rows, e = tx.Query(`SELECT `+nodeCols+` FROM nodes n JOIN bindings b ON b.node_id=n.id AND b.account_id=?
		WHERE b.state='missing' AND b.remote_id<>'' AND n.trashed=0 ORDER BY n.created,n.id`, j.AccountID)
	if e != nil {
		return 0, e
	}
	var missing []Node
	for rows.Next() {
		n, err := nodeScan(rows)
		if err != nil {
			e = err
			break
		}
		missing = append(missing, n)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return 0, e
	}
	groups := map[string][]string{}
	for _, n := range missing {
		if _, busy := operations[n.ID]; busy || moving[n.ID] {
			continue
		}
		active, e := localNodeActive(tx, n.ID)
		if e != nil {
			return 0, e
		}
		if !active {
			continue
		}
		owner := ""
		if ref, ok := sources[n.SourceID]; ok && n.ID == stableNode(ref.MonitorID, ref.File.ID) {
			owner = ref.MonitorID
		} else if n.Kind == "folder" && n.SourceID == "" && n.SourceKey != "" {
			for id := range selected {
				if n.ID == stableNode(id, n.SourceKey) {
					owner = id
					break
				}
			}
		}
		if owner != "" {
			groups[owner] = append(groups[owner], n.ID)
		}
	}
	count := 0
	for _, m := range monitors {
		ids := groups[m.ID]
		if len(ids) == 0 {
			continue
		}
		var minutes int
		if e = tx.QueryRow(`SELECT auto_minutes FROM teldrive_monitors WHERE id=?`, m.ID).Scan(&minutes); e != nil {
			return 0, e
		}
		if monitorID == "" && minutes == 0 {
			continue
		}
		if _, e = enqueueTx(tx, j.AccountID, "recover", "恢复 TelDrive · "+m.Name,
			JobData{MonitorID: m.ID, ExactNodes: true, NodeIDs: ids}); e != nil {
			return 0, e
		}
		count += len(ids)
	}
	return count, tx.Commit()
}

// Refresh the entire target ancestry after a long download or upload. Cached
// folder resolutions from its start may predate an ancestor move or rename.
func (a *App) telDriveTarget(ctx context.Context, c pikpak.Provider, account string, saved Node) (Node, string, error) {
	latest, e := a.Store.Node(saved.ID, account)
	if e != nil {
		return latest, "", e
	}
	if e = a.telDriveNodeActive(latest.ID); e != nil {
		return latest, "", e
	}
	if latest.SourceID != saved.SourceID || latest.Size != saved.Size || latest.Kind != saved.Kind || latest.Hash != "" && saved.Hash != "" && !strings.EqualFold(latest.Hash, saved.Hash) {
		return latest, "", block("文件来源或内容在传输期间变化，已保留原上传会话")
	}
	if latest.Hash == "" {
		latest.Hash = saved.Hash
	}
	if cache := operation(ctx); cache != nil {
		cache.folders = map[string]resolvedFolder{}
	}
	parent, e := a.folder(ctx, c, account, latest.ParentID, map[string]bool{})
	return latest, parent, e
}

package vault

import (
	"database/sql"
	"net/url"
	"strings"
)

type FolderPreview struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Select at most three videos per displayed folder from local metadata only.
// Prefer direct children, then descend through available, non-trashed folders.
// Depth and cycle guards protect a listing against malformed legacy paths.
func attachFolderPreviews(tx *sql.Tx, account string, nodes []Node) error {
	indices := map[string]int{}
	seeds := []string{}
	args := []any{}
	for i, n := range nodes {
		if n.Kind == "folder" && !n.Trashed && n.Transfer == nil && (n.State == "present" || n.State == "drift") {
			indices[n.ID] = i
			seeds = append(seeds, "(?,?,0,?)")
			args = append(args, n.ID, n.ID, "/"+n.ID+"/")
		}
	}
	if len(seeds) == 0 {
		return nil
	}
	args = append(args, account, account)
	rows, err := tx.Query(`WITH RECURSIVE folders(owner,id,depth,trail) AS (
		VALUES `+strings.Join(seeds, ",")+`
		UNION ALL
		SELECT f.owner,n.id,f.depth+1,f.trail||n.id||'/' FROM folders f
		JOIN nodes n ON n.parent_id=f.id AND n.kind='folder' AND n.trashed=0
		JOIN bindings b ON b.node_id=n.id AND b.account_id=? AND b.state IN ('present','drift')
		WHERE f.depth<64 AND instr(f.trail,'/'||n.id||'/')=0
	), candidates AS (
		SELECT f.owner,n.id,b.remote_id,ROW_NUMBER() OVER(PARTITION BY f.owner ORDER BY f.depth,n.name COLLATE NOCASE,n.id) AS position
		FROM folders f JOIN nodes n ON n.parent_id=f.id AND n.kind='file' AND n.trashed=0
		JOIN bindings b ON b.node_id=n.id AND b.account_id=? AND b.remote_id<>'' AND b.state IN ('present','drift')
		WHERE (n.mime LIKE 'video/%' OR lower(n.name) GLOB '*.mp4' OR lower(n.name) GLOB '*.mkv' OR lower(n.name) GLOB '*.mov' OR lower(n.name) GLOB '*.avi' OR lower(n.name) GLOB '*.webm' OR lower(n.name) GLOB '*.m4v')
	)
	SELECT owner,id,remote_id FROM candidates WHERE position<=3 ORDER BY owner,position`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, id, remote string
		if err = rows.Scan(&owner, &id, &remote); err != nil {
			return err
		}
		q := url.Values{"account": {account}, "remote": {remote}, "folder": {owner}}
		i := indices[owner]
		nodes[i].FolderPreviews = append(nodes[i].FolderPreviews, FolderPreview{ID: id, URL: "/api/v1/files/" + id + "/thumbnail?" + q.Encode()})
	}
	return rows.Err()
}

// Validate stale image requests again, including local moves and account changes.
func (a *App) previewAncestor(n Node, folder, account string) bool {
	seen := map[string]bool{}
	for id, depth := n.ParentID, 0; id != "" && !seen[id] && depth <= 64; depth++ {
		seen[id] = true
		parent, err := a.Store.Node(id, account)
		if err != nil || parent.Trashed || parent.Kind != "folder" || (parent.State != "present" && parent.State != "drift") {
			return false
		}
		if id == folder {
			return true
		}
		id = parent.ParentID
	}
	return false
}

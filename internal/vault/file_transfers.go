package vault

import (
	"database/sql"
	"encoding/json"
	"mime"
	"net/url"
	"path"
	"sort"
	"strings"
)

type FileTransfer struct {
	JobID    string `json:"job_id"`
	State    string `json:"state"`
	Progress int    `json:"progress"`
	Message  string `json:"message"`
}

// Pending items are projections, not nodes: a queued source is not yet a
// verified file and must never enter recovery, folder pickers, or file actions.
// The caller reads this and the real nodes in one SQLite snapshot so registration
// cannot produce a duplicate or an empty gap in the same response.
func fileTransfers(tx *sql.Tx, account, parent string, q url.Values) ([]Node, map[string]*FileTransfer, error) {
	bySource := map[string]*FileTransfer{}
	rows, err := tx.Query(`SELECT j.id,j.title,j.state,j.progress,j.message,j.created,j.updated,j.data,s.kind,s.manifest
		FROM jobs j JOIN sources s ON s.id=json_extract(j.data,'$.source_id')
		JOIN nodes target ON target.id=COALESCE(NULLIF(json_extract(j.data,'$.parent_id'),''),'root') AND target.trashed=0
		WHERE j.account_id=? AND j.kind='import' AND j.state NOT IN ('completed','cancelled') ORDER BY j.created,j.id`, account)
	if err != nil {
		return nil, nil, err
	}
	var candidates []Node
	for rows.Next() {
		var j Job
		var data, kind, manifest string
		if err = rows.Scan(&j.ID, &j.Title, &j.State, &j.Progress, &j.Message, &j.Created, &j.Updated, &data, &kind, &manifest); err != nil {
			break
		}
		var d JobData
		if err = json.Unmarshal([]byte(data), &d); err != nil {
			break
		}
		if d.ParentID == "" {
			d.ParentID = "root"
		}
		transfer := &FileTransfer{JobID: j.ID, State: j.State, Progress: min(max(j.Progress, 0), 99), Message: j.Message}
		bySource[d.SourceID] = transfer
		entries := d.Preview
		var saved []Entry
		if err = json.Unmarshal([]byte(manifest), &saved); err != nil {
			break
		}
		if len(saved) > 0 {
			entries = saved
		}
		if t := d.Transfers[d.SourceID]; t != nil {
			if len(t.Expected) > 0 {
				entries = t.Expected
			} else if len(t.Display) > 0 {
				entries = t.Display
			}
			if len(t.Entries) > 0 {
				entries = nil
				for _, r := range t.Entries {
					k := "file"
					if r.File.Folder() {
						k = "folder"
					}
					entries = append(entries, Entry{Name: r.File.Name, Path: r.Path, Kind: k, Size: int64(r.File.Size)})
				}
			}
		}
		if len(entries) == 0 {
			name := j.Title
			if name == "Save PikPak share" {
				name = "PikPak 分享 · 正在解析"
			} else if name == "Save magnet link" {
				name = "磁力资源 · 正在解析"
			}
			// Kind is intentionally unknown until PikPak resolves the source.
			entries = []Entry{{Name: name, Kind: "transfer"}}
		}
		seen := map[string]bool{}
		for _, entry := range entries {
			if strings.Contains(entry.Path, "/") || seen[entry.Path] {
				continue
			}
			seen[entry.Path] = true
			n := Node{ID: "import-" + j.ID + "-" + stableNode(d.SourceID, entry.Path), ParentID: d.ParentID,
				Name: entry.Name, Kind: entry.Kind, Size: entry.Size, SourceID: d.SourceID, SourcePath: entry.Path,
				Created: j.Created, Modified: j.Updated, State: "transferring", Transfer: transfer}
			n.Mime = mime.TypeByExtension(strings.ToLower(path.Ext(n.Name)))
			candidates = append(candidates, n)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	// Include trashed/moved nodes: an already registered result must not reappear
	// as a fresh placeholder at the original import destination.
	rows, err = tx.Query(`SELECT source_id,source_path FROM nodes WHERE source_id IN
		(SELECT json_extract(data,'$.source_id') FROM jobs WHERE account_id=? AND kind='import' AND state NOT IN ('completed','cancelled'))`, account)
	if err != nil {
		return nil, nil, err
	}
	existing := map[string]map[string]bool{}
	for rows.Next() {
		var source, relative string
		if err = rows.Scan(&source, &relative); err != nil {
			break
		}
		if existing[source] == nil {
			existing[source] = map[string]bool{}
		}
		existing[source][relative] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	out := []Node{}
	for _, n := range candidates {
		if existing[n.SourceID][n.SourcePath] || (n.SourcePath == "" && len(existing[n.SourceID]) > 0) {
			continue
		}
		if matchesTransferView(n, parent, q) {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Kind == "folder") != (b.Kind == "folder") {
			return a.Kind == "folder"
		}
		cmp := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		var x, y int64
		switch q.Get("sort") {
		case "size":
			x, y = a.Size, b.Size
		case "created":
			x, y = a.Created, b.Created
		case "modified":
			x, y = a.Modified, b.Modified
		default:
			if q.Get("direction") == "desc" {
				return cmp > 0
			}
			return cmp < 0
		}
		if q.Get("direction") == "desc" {
			return x > y
		}
		return x < y
	})
	return out, bySource, nil
}

func matchesTransferView(n Node, parent string, q url.Values) bool {
	search := strings.ToLower(q.Get("search"))
	if !strings.Contains(strings.ToLower(n.Name), search) {
		return false
	}
	switch q.Get("view") {
	case "", "files":
		return search != "" || n.ParentID == parent
	case "video", "audio", "image":
		return strings.HasPrefix(n.Mime, q.Get("view")+"/")
	case "documents":
		return n.Kind == "file" && !strings.HasPrefix(n.Mime, "video/") && !strings.HasPrefix(n.Mime, "audio/") && !strings.HasPrefix(n.Mime, "image/")
	default:
		return false
	}
}

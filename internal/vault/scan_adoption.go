package vault

import (
	"fmt"

	"pikpakvault/internal/pikpak"
)

// Rebind manually transferred files by their complete library path, never just
// a basename or hash. Only known local nodes are adopted; extra cloud files stay
// untouched. The caller commits these results with the rest of the full scan.
func scanUnbound(nodes []Node, remote map[string][]pikpak.File, root string) (map[string]pikpak.File, map[string]string, error) {
	byID := map[string]Node{}
	claimed := map[string]string{}
	paths := map[string]string{"root": ""}
	counts := map[string]int{}
	visiting := map[string]bool{}
	var localPath func(string) (string, error)
	localPath = func(id string) (string, error) {
		if p, ok := paths[id]; ok {
			return p, nil
		}
		n, ok := byID[id]
		if !ok || visiting[id] {
			return "", fmt.Errorf("本地目录树不完整或存在循环")
		}
		visiting[id] = true
		p, err := localPath(n.ParentID)
		delete(visiting, id)
		if err != nil {
			return "", err
		}
		paths[id] = p + "/" + n.Name
		return paths[id], nil
	}
	for _, n := range nodes {
		byID[n.ID] = n
		if n.RemoteID != "" {
			claimed[n.RemoteID] = n.ID
		}
	}
	for _, n := range nodes {
		p, err := localPath(n.ID)
		if err != nil {
			return nil, nil, err
		}
		if !n.Trashed {
			counts[p]++
		}
	}
	bindings := map[string]pikpak.File{}
	states := map[string]string{}
	var resolve func(string) string
	resolve = func(id string) string {
		if id == "root" {
			return root
		}
		n := byID[id]
		if n.Trashed {
			return ""
		}
		if n.RemoteID != "" {
			return n.RemoteID
		}
		if _, ok := states[id]; ok {
			return bindings[id].ID
		}
		states[id] = "missing"
		p := paths[id]
		matches := remote[p]
		if len(matches) == 0 {
			return ""
		}
		states[id] = "conflict"
		if len(matches) != 1 || counts[p] != 1 {
			return ""
		}
		f := matches[0]
		if owner := claimed[f.ID]; owner != "" && owner != id || !compatible(n, f) {
			return ""
		}
		if !f.Complete() {
			states[id] = "pending"
			return ""
		}
		if n.Kind != "folder" && n.Hash != "" && f.Hash == "" {
			states[id] = "unknown"
			return ""
		}
		parent := resolve(n.ParentID)
		if parent == "" || parent != f.ParentID {
			return ""
		}
		claimed[f.ID] = id
		bindings[id], states[id] = f, "present"
		return f.ID
	}
	for _, n := range nodes {
		if !n.Trashed && n.RemoteID == "" {
			resolve(n.ID)
		}
	}
	return bindings, states, nil
}

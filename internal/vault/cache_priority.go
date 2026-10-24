package vault

import (
	"encoding/json"
	"os"
	"path/filepath"

	"pikpakvault/internal/aria2"
)

// telDriveResumeNode identifies work that can finish an existing upload or
// release a complete download. It uses only durable checkpoints and the fixed
// files in this task's own cache; no remote queries or content hashing occur in
// the scheduler. Partial downloads alone do not reserve the worker.
func (a *App) telDriveResumeNode(j Job, d *JobData, nodeID string) bool {
	if d.Done[nodeID] {
		return false
	}
	u := d.Uploads[nodeID]
	if u == nil {
		return false
	}
	if u.Attempted || u.RemoteID != "" || u.Secret != "" {
		return true
	}
	dir := a.telDriveCacheDir(j.ID, nodeID)
	if aria2.Validate(dir) != nil {
		return false
	}
	var owner cacheOwner
	b, err := os.ReadFile(filepath.Join(dir, "owner.json"))
	if err != nil || json.Unmarshal(b, &owner) != nil || owner.JobID != j.ID || owner.NodeID != nodeID {
		return false
	}
	var meta struct {
		Complete bool
		Size     int64
	}
	b, err = os.ReadFile(filepath.Join(dir, "source.json"))
	if err != nil || json.Unmarshal(b, &meta) != nil || !meta.Complete || meta.Size < 0 {
		return false
	}
	file := filepath.Join(dir, "content")
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() != meta.Size {
		return false
	}
	_, err = os.Lstat(file + ".aria2")
	return os.IsNotExist(err)
}

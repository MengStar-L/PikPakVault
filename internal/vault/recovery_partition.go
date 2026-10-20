package vault

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

type recoveryPartitionNode struct {
	id, parent, kind, source string
}

// partitionRecovery freezes a legacy recovery's selected descendants and gives
// each resource one lane. The caller holds jobMu and dataMu.RLock. Keeping the
// original job for TelDrive also preserves its aria2 cache ownership and upload
// session; OperationID preserves the names used by older cloud checkpoints.
func (a *App) partitionRecovery(j *Job) error {
	if j.Kind != "recover" {
		return nil
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := jobScan(tx.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, j.ID))
	if err != nil {
		return err
	}
	*j = current
	if current.Kind != "recover" || (current.State != "queued" && current.State != "waiting" && current.State != "retry") {
		return nil
	}
	var data JobData
	if err = json.Unmarshal(current.Data, &data); err != nil {
		return err
	}
	if data.RecoveryLane != "" {
		return nil
	}
	nodes, sources, err := recoveryPartitionSnapshot(tx)
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, id := range data.NodeIDs {
		selected[id] = true
	}
	if !data.ExactNodes {
		if len(data.NodeIDs) == 0 || selected["root"] {
			for _, n := range nodes {
				selected[n.id] = true
			}
		} else {
			for changed := true; changed; {
				changed = false
				for _, n := range nodes {
					if selected[n.parent] && !selected[n.id] {
						selected[n.id], changed = true, true
					}
				}
			}
		}
		// Descendants excludes the synthetic root, even when it selects all.
		delete(selected, "root")
	}
	nodeLanes := map[string]string{}
	cloudIDs, telDriveIDs := []string{}, []string{}
	for _, n := range nodes {
		lane := "cloud"
		if n.kind != "folder" && sources[n.source] == "teldrive" {
			lane = "teldrive"
		}
		nodeLanes[n.id] = lane
		if !selected[n.id] {
			continue
		}
		if lane == "teldrive" {
			telDriveIDs = append(telDriveIDs, n.id)
		} else {
			cloudIDs = append(cloudIDs, n.id)
		}
	}
	for id := range data.Uploads {
		if nodeLanes[id] == "" {
			nodeLanes[id] = "teldrive"
		}
	}
	if data.OperationID == "" {
		data.OperationID = current.ID
	}
	data.ExactNodes = true
	var sibling *Job
	if len(telDriveIDs) > 0 && len(cloudIDs) > 0 {
		cloud := recoveryLaneData(data, "cloud", cloudIDs, nodeLanes, sources)
		td := recoveryLaneData(data, "teldrive", telDriveIDs, nodeLanes, sources)
		cloud.MonitorID = ""
		// A previous successful TelDrive file may have left its path in Note.
		// The new cloud task must report its own result instead.
		cloud.Note = ""
		child := current
		child.ID = ID()
		child.Lane = "cloud"
		child.Title = recoveryLaneTitle("cloud", len(cloudIDs))
		child.State, child.NextRun = "queued", 0
		child.Progress = recoveryCompletedProgress(cloud)
		child.Message = "云端资源已独立排队恢复"
		child.Data = json.RawMessage(jsonText(cloud))
		child.Updated = now()
		if _, err = tx.Exec(`INSERT INTO jobs(`+jobCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			child.ID, child.AccountID, child.Kind, child.Title, child.State, string(child.Data),
			child.Progress, child.Message, child.Attempts, child.NextRun, child.Created, child.Updated); err != nil {
			return err
		}
		sibling = &child
		data = td
	} else {
		data.RecoveryLane, data.NodeIDs = "cloud", cloudIDs
		if len(telDriveIDs) > 0 {
			data.RecoveryLane, data.NodeIDs = "teldrive", telDriveIDs
		}
	}
	current.Lane = data.RecoveryLane
	current.Title = recoveryLaneTitle(data.RecoveryLane, len(data.NodeIDs))
	current.Data = json.RawMessage(jsonText(data))
	current.Updated = now()
	if len(data.NodeIDs) == 0 {
		// An exact, now-empty selection must never become "restore everything".
		// Keep its checkpoint data for audit, but release the empty reservation.
		current.State, current.Progress = "completed", 100
		current.Message = "所选资源已不存在，无需恢复"
	}
	if _, err = tx.Exec(`UPDATE jobs SET title=?,state=?,data=?,progress=?,message=?,updated=? WHERE id=?`,
		current.Title, current.State, string(current.Data), current.Progress, current.Message, current.Updated, current.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	*j = current
	a.Store.Event("job", map[string]string{"id": current.ID, "state": current.State})
	if sibling != nil {
		a.Store.Event("job", map[string]string{"id": sibling.ID, "state": sibling.State})
		a.notify()
	}
	return nil
}

func recoveryPartitionSnapshot(tx *sql.Tx) ([]recoveryPartitionNode, map[string]string, error) {
	rows, err := tx.Query(`SELECT id,parent_id,kind,source_id FROM nodes ORDER BY created,id`)
	if err != nil {
		return nil, nil, err
	}
	var nodes []recoveryPartitionNode
	for rows.Next() {
		var n recoveryPartitionNode
		if err = rows.Scan(&n.id, &n.parent, &n.kind, &n.source); err != nil {
			break
		}
		nodes = append(nodes, n)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = tx.Query(`SELECT id,kind FROM sources`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	sources := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err = rows.Scan(&id, &kind); err != nil {
			return nil, nil, err
		}
		sources[id] = kind
	}
	return nodes, sources, rows.Err()
}

func recoveryLaneData(data JobData, lane string, ids []string, nodes, sources map[string]string) JobData {
	data.RecoveryLane, data.NodeIDs = lane, ids
	nodeInLane := func(id string) bool {
		// Retain checkpoint entries whose node was permanently removed in the
		// cloud history, rather than silently discarding their remote ownership.
		nodeLane := nodes[id]
		if nodeLane == "" {
			nodeLane = "cloud"
		}
		return nodeLane == lane
	}
	sourceInLane := func(id string) bool {
		return (sources[id] == "teldrive") == (lane == "teldrive")
	}
	data.Done = recoveryMapFilter(data.Done, nodeInLane)
	data.Problems = recoveryMapFilter(data.Problems, nodeInLane)
	data.Instant = recoveryMapFilter(data.Instant, nodeInLane)
	data.InstantTried = recoveryMapFilter(data.InstantTried, nodeInLane)
	data.InstantStates = recoveryMapFilter(data.InstantStates, nodeInLane)
	data.Transfers = recoveryMapFilter(data.Transfers, sourceInLane)
	// These are keyed by local node IDs, but their cache is keyed by the original
	// job ID too. Even an orphaned session must stay with that original job.
	if lane == "cloud" {
		data.Uploads = nil
	}
	if data.SourceID != "" && !sourceInLane(data.SourceID) {
		data.SourceID = ""
	}
	return data
}

func recoveryMapFilter[T any](values map[string]T, keep func(string) bool) map[string]T {
	if values == nil {
		return nil
	}
	out := map[string]T{}
	for key, value := range values {
		if keep(key) {
			out[key] = value
		}
	}
	return out
}

func recoveryLaneTitle(lane string, count int) string {
	if lane == "teldrive" {
		return fmt.Sprintf("TelDrive 恢复 · %d 项", count)
	}
	return fmt.Sprintf("云端恢复 · %d 项", count)
}

func recoveryCompletedProgress(data JobData) int {
	done := 0
	for _, id := range data.NodeIDs {
		if data.Done[id] {
			done++
		}
	}
	return min(99, done*100/max(len(data.NodeIDs), 1))
}

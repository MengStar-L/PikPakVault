package vault

import "database/sql"

// Scope reservations to an account and stable local IDs, never a destination
// path. Paused/failed tasks retain their upload session until explicitly cancelled.
// Recoveries expand folders just like Descendants, including children added later.
const resourceJobsCTE = `WITH RECURSIVE unfinished_jobs AS (
	SELECT id,kind,state,progress,message,created,data FROM jobs
	WHERE account_id=? AND state NOT IN ('completed','cancelled')
	AND (kind IN ('import','teldrive_upload','recover') OR (kind='sync' AND COALESCE(json_extract(data,'$.monitor_id'),'')<>''))
), resource_jobs(job_id,node_id,kind) AS (
	SELECT j.id,n.id,j.kind FROM unfinished_jobs j JOIN nodes n ON n.source_id=json_extract(j.data,'$.source_id')
	WHERE j.kind IN ('import','teldrive_upload') AND n.source_id<>''
	UNION
	SELECT j.id,v.value,j.kind FROM unfinished_jobs j,json_each(j.data,'$.node_ids') v
	WHERE j.kind IN ('teldrive_upload','sync','recover')
	UNION
	SELECT j.id,'root',j.kind FROM unfinished_jobs j WHERE j.kind='recover'
	AND NOT EXISTS (SELECT 1 FROM json_each(j.data,'$.node_ids'))
	UNION
	SELECT r.job_id,n.id,r.kind FROM resource_jobs r JOIN nodes n ON n.parent_id=r.node_id WHERE r.kind='recover'
)
`

const recoveryNeededSQL = `(b.state IS NULL OR b.state IN ('missing','drift','conflict','unknown'))
	AND NOT EXISTS (SELECT 1 FROM resource_jobs r WHERE r.node_id=n.id AND r.kind<>'recover')`

type nodeJob struct {
	FileTransfer
	Kind string
}

type rowsQuerier interface {
	Query(string, ...any) (*sql.Rows, error)
}

// Prefer the original transfer over a recovery accidentally queued by an older
// version; otherwise the oldest recovery owns the resource. This also prevents
// two legacy recovery jobs from waiting on each other.
func nodeJobs(q rowsQuerier, account string) (map[string]nodeJob, error) {
	rows, err := q.Query(resourceJobsCTE+`SELECT r.node_id,j.id,j.kind,j.state,j.progress,j.message
		FROM resource_jobs r JOIN unfinished_jobs j ON j.id=r.job_id
		ORDER BY CASE WHEN j.kind='recover' THEN 1 ELSE 0 END,j.created,j.id`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]nodeJob{}
	for rows.Next() {
		var id string
		var j nodeJob
		if err = rows.Scan(&id, &j.JobID, &j.Kind, &j.State, &j.Progress, &j.Message); err != nil {
			return nil, err
		}
		j.Progress = min(max(j.Progress, 0), 99)
		if _, ok := out[id]; !ok {
			out[id] = j
		}
	}
	return out, rows.Err()
}

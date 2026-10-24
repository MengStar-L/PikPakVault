package vault

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

const (
	cloudLane    = "cloud"
	telDriveLane = "teldrive"
)

type scheduledExecutionKey struct{}

func jobLane(j Job) string {
	if j.Kind == "teldrive_upload" {
		return telDriveLane
	}
	if j.Kind == "recover" {
		var d struct {
			Lane string `json:"recovery_lane"`
		}
		if json.Unmarshal(j.Data, &d) == nil && d.Lane == telDriveLane {
			return telDriveLane
		}
	}
	return cloudLane
}

func operationJobID(j *Job, d *JobData) string {
	if d.OperationID != "" {
		return d.OperationID
	}
	return j.ID
}

// Independent bounded lanes keep large transfers from occupying the cloud
// worker. Each job retains its own context, checkpoints, and operation cache.
func (a *App) Run(ctx context.Context) {
	resumed := false
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var workers sync.WaitGroup
	defer workers.Wait() // The service closes the database only after Run exits.
	finished := make(chan string, 2)
	busy := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case lane := <-finished:
			delete(busy, lane)
		case <-tick.C:
		case <-a.wake:
		}
		if ctx.Err() != nil {
			return
		}
		a.dataMu.RLock()
		if a.Store.Get("import_review_required") == "true" || updatePending() {
			a.dataMu.RUnlock()
			continue
		}
		if !resumed {
			_, err := a.Store.DB.Exec(`UPDATE jobs SET state='queued',message='Resuming after restart' WHERE state='running'`)
			if err != nil {
				a.dataMu.RUnlock()
				continue
			}
			resumed = true
		}
		a.scheduleRoot()
		a.scheduleScan()
		a.scheduleCleanup()
		a.scheduleTelDrive()
		a.scheduleRSS()
		jobs := a.runnableJobs(busy)
		a.dataMu.RUnlock()
		for _, job := range jobs {
			lane := jobLane(job)
			busy[lane] = true
			workers.Add(1)
			go func(j Job, lane string) {
				defer workers.Done()
				a.Execute(context.WithValue(ctx, scheduledExecutionKey{}, true), &j)
				finished <- lane
			}(job, lane)
		}
	}
}

// Caller holds dataMu. Closing rows before partitioning is required because
// the store has one SQLite connection. jobMu also excludes task-control writes.
func (a *App) runnableJobs(busy map[string]bool) []Job {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	rows, err := a.Store.DB.Query(`SELECT `+jobCols+` FROM jobs
		WHERE state IN ('queued','waiting','retry') AND (next_run<=? OR kind='teldrive_upload' OR
			(kind='recover' AND COALESCE(json_extract(data,'$.recovery_lane'),'') IN ('','teldrive')))
		AND (kind='verify' OR (account_id=? AND EXISTS (SELECT 1 FROM accounts
			WHERE accounts.id=jobs.account_id AND (status='ready' OR
			(status='quota_exceeded' AND jobs.kind IN ('scan','trash','sync','cleanup'))))))
		ORDER BY CASE WHEN kind='verify' THEN 0 WHEN kind='root' THEN 1 WHEN kind='cleanup' THEN 3 ELSE 2 END,next_run,created,id`,
		now(), a.active())
	if err != nil {
		return nil
	}
	var candidates []Job
	for rows.Next() {
		j, err := jobScan(rows)
		if err != nil {
			rows.Close()
			return nil
		}
		candidates = append(candidates, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil
	}
	// Include deferred TelDrive work in the snapshot. A completed download or
	// submitted upload keeps its cache until verification, so starting another
	// large download during its short retry delay can exhaust the disk and also
	// hold the only upload worker when that verification becomes due.
	prepared := make([]Job, 0, len(candidates))
	resume := make(map[string]bool)
	haveTelDriveResume := false
	for _, j := range candidates {
		if a.executing[j.ID] {
			continue
		}
		if j.Kind == "recover" {
			if err := a.partitionRecovery(&j); err != nil {
				j.State, j.Message = "attention", "恢复任务分流未完成，原进度已保留，请重试"
				_ = a.Store.SaveJob(&j, nil)
				a.Store.Event("job", map[string]string{"id": j.ID, "state": j.State})
				continue
			}
		}
		if j.State != "queued" && j.State != "waiting" && j.State != "retry" {
			continue
		}
		prepared = append(prepared, j)
		if jobLane(j) == telDriveLane {
			var d JobData
			if json.Unmarshal(j.Data, &d) == nil {
				for id := range d.Uploads {
					if a.telDriveResumeNode(j, &d, id) {
						resume[j.ID], haveTelDriveResume = true, true
						break
					}
				}
			}
		}
	}
	selected := make(map[string]bool)
	var jobs []Job
	for _, j := range prepared {
		if j.NextRun > now() {
			continue
		}
		lane := jobLane(j)
		if busy[lane] || selected[lane] {
			continue
		}
		if lane == telDriveLane && haveTelDriveResume && !resume[j.ID] {
			continue
		}
		selected[lane] = true
		jobs = append(jobs, j)
	}
	return jobs
}

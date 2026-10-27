package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

const retryableJobStates = `('failed','partial','attention','paused')`

type jobRetrySummary struct {
	AccountID string `json:"account_id"`
	Count     int    `json:"count"`
}

type jobsRetryResult struct {
	AccountID      string         `json:"account_id"`
	Restarted      int            `json:"restarted"`
	Skipped        int            `json:"skipped"`
	SkippedReasons map[string]int `json:"skipped_reasons"`
	Message        string         `json:"message"`
}

// Caller holds jobMu. Both individual and bulk retry must retain the original
// job and its remote tickets, downloaded files, and completed recovery items.
func (a *App) retryJobGuard(j Job, active string, account Account) (string, error) {
	if j.State == "running" || a.executing[j.ID] {
		return "executing", fail(409, "原任务仍在保存进度，请稍后再试")
	}
	if j.AccountID != active && j.Kind != "verify" {
		return "other_account", fail(409, "Switch to this task's account first")
	}
	if !accountStatusAllows(account.Status, j.Kind) {
		return "account_unavailable", fail(409, accountErrorMessage(account))
	}
	return "", nil
}

func resetJobForRetry(j *Job, d *JobData) {
	j.State = "queued"
	j.Message = "已收到重试请求，等待继续原任务"
	if j.Kind == "import" {
		j.Message = "已收到重试请求，将重新核对云端保存结果"
	}
	j.Attempts = 0
	j.NextRun = 0
	j.WaitReason = ""
	d.WaitReason, d.WaitSpace = "", nil
	d.Problems = map[string]string{}
	for _, transfer := range d.Transfers {
		if transfer != nil {
			transfer.Polls = 0
		}
	}
}

func retryCheckKey(j Job, d JobData) string {
	switch j.Kind {
	case "scan", "verify", "adopt":
		return j.Kind
	case "rss_scan":
		return j.Kind + ":" + d.RSSID
	case "teldrive_scan":
		return j.Kind + ":" + d.MonitorID
	}
	return ""
}

func decodeRetryData(raw json.RawMessage, d *JobData) bool {
	// JSON null would otherwise decode to an empty recovery selection, which
	// means "all resources". An invalid checkpoint must never widen its scope.
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Unmarshal(trimmed, d) == nil
}

func (a *App) jobsRetryAll(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		AccountID string `json:"account_id"`
	}
	if err := decode(r, &input); err != nil {
		return err
	}
	if input.AccountID == "" {
		return fail(400, "请先选择要重新开始任务的账号")
	}
	a.gate.RLock()
	defer a.gate.RUnlock()
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	tx, err := a.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	active := a.activeFromTx(tx)
	if active != input.AccountID {
		return fail(409, "当前账号已切换，请刷新任务列表后重试")
	}
	account, err := accountScan(tx.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id=?`, active))
	if err != nil {
		return fail(409, "请先连接并验证当前 PikPak 账号")
	}
	// Read the complete eligible history, not the capped UI listing. Runnable
	// checks seed deduplication; paused requests may still be unwinding too.
	rows, err := tx.Query(`SELECT `+jobCols+` FROM jobs WHERE account_id=?
		AND state NOT IN ('completed','cancelled') ORDER BY created DESC,id DESC`, active)
	if err != nil {
		return err
	}
	var jobs []Job
	for rows.Next() {
		j, e := jobScan(rows)
		if e != nil {
			rows.Close()
			return e
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	checks := map[string]bool{}
	for _, j := range jobs {
		if j.State == "queued" || j.State == "running" || j.State == "waiting" || j.State == "retry" || a.executing[j.ID] {
			var d JobData
			if decodeRetryData(j.Data, &d) {
				if key := retryCheckKey(j, d); key != "" {
					checks[key] = true
				}
			}
		}
	}
	result := jobsRetryResult{AccountID: active, SkippedReasons: map[string]int{}}
	skip := func(reason string) { result.Skipped++; result.SkippedReasons[reason]++ }
	for _, j := range jobs {
		if j.State != "failed" && j.State != "partial" && j.State != "attention" && j.State != "paused" {
			continue
		}
		if reason, e := a.retryJobGuard(j, active, account); e != nil {
			skip(reason)
			continue
		}
		var d JobData
		if !decodeRetryData(j.Data, &d) {
			skip("invalid_data")
			continue
		}
		key := retryCheckKey(j, d)
		if key != "" && checks[key] {
			skip("duplicate_check")
			continue
		}
		d.init()
		resetJobForRetry(&j, &d)
		if _, err = tx.Exec(`UPDATE jobs SET state=?,data=?,message=?,attempts=?,next_run=?,updated=? WHERE id=?`,
			j.State, jsonText(d), j.Message, j.Attempts, j.NextRun, now(), j.ID); err != nil {
			return err
		}
		if key != "" {
			checks[key] = true
		}
		result.Restarted++
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	result.Message = fmt.Sprintf("已将 %d 个任务重新加入队列，保留原有进度", result.Restarted)
	if result.Restarted == 0 {
		result.Message = "本次没有任务重新加入队列"
	}
	if result.SkippedReasons["account_unavailable"] > 0 {
		result.Message += "；" + accountErrorMessage(account)
	}
	if result.Restarted > 0 {
		a.Store.Event("jobs", map[string]any{"account_id": active, "restarted": result.Restarted})
		a.notify()
	}
	writeJSON(w, http.StatusOK, result)
	return nil
}

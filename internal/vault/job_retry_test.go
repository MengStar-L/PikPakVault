package vault

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func retryFixtureJob(t *testing.T, a *App, account, kind, state string, data JobData) Job {
	t.Helper()
	j, err := a.Store.NewJob(account, kind, kind, data)
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.Attempts, j.NextRun, j.Progress, j.Message = state, 6, now()+600, 43, "old failure"
	if err = a.Store.SaveJob(&j, nil); err != nil {
		t.Fatal(err)
	}
	return j
}

func retryAllForTest(t *testing.T, a *App) jobsRetryResult {
	t.Helper()
	w := request(t, a.Handler(nil), "POST", "/api/v1/jobs/retry-all", map[string]string{"account_id": "a"}, "test-csrf")
	var result jobsRetryResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return result
}

func TestBatchRetryPreservesOriginalWorkAndSkipsUnrelatedJobs(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	addAccount(t, a, "b", "user-b")
	data := JobData{
		SourceID: "source", NodeIDs: []string{"movie"}, ParentID: "latest-folder", OperationID: "original", RecoveryLane: "teldrive",
		Done: map[string]bool{"already-saved": true}, Problems: map[string]string{"movie": "disk full"},
		Uploads:   map[string]*TelDriveUpload{"movie": {Attempted: true, Parent: "remote-parent", Name: "video.mp4", GCID: "hash", RemoteID: "remote-file", Secret: "sealed-ticket", Polls: 7}},
		Transfers: map[string]*TransferState{"source": {Phase: "submitted", Polls: 100, TaskID: "upstream-task", OutputIDs: []string{"output"}, TargetID: "remote-target"}},
		Instant:   map[string]string{"movie": "instant"}, InstantTried: map[string]bool{"movie": true},
	}
	var expected []Job
	for i, state := range []string{"failed", "partial", "attention", "paused"} {
		expected = append(expected, retryFixtureJob(t, a, "a", []string{"teldrive_upload", "recover", "import", "sync"}[i], state, data))
	}
	cache := filepath.Join(a.telDriveCacheDir(expected[0].ID, "movie"), "content.part")
	if err := os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("partial-download"), 0600); err != nil {
		t.Fatal(err)
	}
	var untouched []Job
	for _, state := range []string{"queued", "running", "waiting", "retry", "completed", "cancelled"} {
		untouched = append(untouched, retryFixtureJob(t, a, "a", "teldrive_upload", state, data))
	}
	untouched = append(untouched, retryFixtureJob(t, a, "b", "import", "failed", data))
	executing := retryFixtureJob(t, a, "a", "teldrive_upload", "paused", data)
	untouched = append(untouched, executing)
	a.executing = map[string]bool{executing.ID: true}
	result := retryAllForTest(t, a)
	if result.AccountID != "a" || result.Restarted != 4 || result.Skipped != 1 || result.SkippedReasons["executing"] != 1 {
		t.Fatal(result)
	}
	for _, before := range expected {
		after, err := a.Store.Job(before.ID)
		if err != nil || after.State != "queued" || after.Attempts != 0 || after.NextRun != 0 || after.Progress != before.Progress || after.Created != before.Created || after.AccountID != before.AccountID {
			t.Fatal("job identity/progress changed", after, err)
		}
		var got JobData
		if err = json.Unmarshal(after.Data, &got); err != nil {
			t.Fatal(err)
		}
		want := data
		want.Problems = map[string]string{}
		want.Transfers = map[string]*TransferState{"source": {Phase: "submitted", TaskID: "upstream-task", OutputIDs: []string{"output"}, TargetID: "remote-target"}}
		// init() may create omitted empty maps; compare the durable JSON fields.
		if jsonText(got) != jsonText(want) {
			t.Fatalf("checkpoint lost:\n%s\n%s", jsonText(got), jsonText(want))
		}
	}
	for _, before := range untouched {
		after, _ := a.Store.Job(before.ID)
		// The computed Lane is not persisted by Store.NewJob for sync jobs.
		if after.State != before.State || after.Attempts != before.Attempts || after.NextRun != before.NextRun || string(after.Data) != string(before.Data) || after.Message != before.Message {
			t.Fatal("unrelated job was changed", before.ID)
		}
	}
	if content, err := os.ReadFile(cache); err != nil || string(content) != "partial-download" {
		t.Fatal("partial download changed", err)
	}
	result = retryAllForTest(t, a)
	if result.Restarted != 0 || result.Skipped != 1 {
		t.Fatal("repeat click requeued work", result)
	}
}

func TestBatchRetryCoversHistoryBeyondListingLimit(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	tx, err := a.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 620; i++ {
		if _, err = tx.Exec(`INSERT INTO jobs(id,account_id,kind,title,state,data,created,updated) VALUES(?,'a','import','old failure','failed','{}',?,?)`, fmt.Sprintf("older-%03d", i), i, i); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 500; i++ {
		if _, err = tx.Exec(`INSERT INTO jobs(id,account_id,kind,title,state,data,created,updated) VALUES(?,'a','scan','newer completion','completed','{}',?,?)`, fmt.Sprintf("newer-%03d", i), i+1000, i+1000); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w := request(t, a.Handler(nil), "GET", "/api/v1/jobs", nil, "")
	var listing struct {
		Jobs    []Job           `json:"jobs"`
		Summary jobRetrySummary `json:"retry_summary"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil || len(listing.Jobs) != 500 || listing.Summary.Count != 620 || listing.Summary.AccountID != "a" {
		t.Fatalf("listing or full count incorrect: %d %+v", w.Code, listing.Summary)
	}
	result := retryAllForTest(t, a)
	if result.Restarted != 620 || result.Skipped != 0 {
		t.Fatal(result)
	}
	var count int
	if err = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state='queued'`).Scan(&count); err != nil || count != 620 {
		t.Fatal(count, err)
	}
	w = request(t, a.Handler(nil), "GET", "/api/v1/jobs", nil, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil || listing.Summary.Count != 0 || len(listing.Jobs) != 500 {
		t.Fatal("retry listing failed", w.Code)
	}
	for _, j := range listing.Jobs {
		if j.State != "queued" || !strings.HasPrefix(j.ID, "older-") {
			t.Fatal("newer completed jobs hide resumed history", j)
		}
	}
}

func TestBatchRetryDeduplicatesChecksButKeepsHistory(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	for i := 0; i < 3; i++ {
		retryFixtureJob(t, a, "a", "scan", "failed", JobData{})
	}
	retryFixtureJob(t, a, "a", "scan", "running", JobData{})
	for i := 0; i < 2; i++ {
		retryFixtureJob(t, a, "a", "verify", "failed", JobData{})
		retryFixtureJob(t, a, "a", "teldrive_scan", "failed", JobData{MonitorID: "monitor-1"})
	}
	retryFixtureJob(t, a, "a", "teldrive_scan", "paused", JobData{MonitorID: "monitor-2"})
	retryFixtureJob(t, a, "a", "rss_scan", "queued", JobData{RSSID: "rss-1"})
	retryFixtureJob(t, a, "a", "rss_scan", "failed", JobData{RSSID: "rss-1"})
	retryFixtureJob(t, a, "a", "rss_scan", "failed", JobData{RSSID: "rss-2"})
	result := retryAllForTest(t, a)
	if result.Restarted != 4 || result.Skipped != 6 || result.SkippedReasons["duplicate_check"] != 6 {
		t.Fatal(result)
	}
	result = retryAllForTest(t, a)
	if result.Restarted != 0 || result.Skipped != 6 || result.SkippedReasons["duplicate_check"] != 6 {
		t.Fatal("repeat request duplicated checks", result)
	}
	var total, failed int
	_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&total)
	_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state='failed'`).Scan(&failed)
	if total != 12 || failed != 6 {
		t.Fatal("check histories discarded", total, failed)
	}
}

func TestBatchRetryRespectsHealthAndExpectedAccount(t *testing.T) {
	for _, status := range []string{"blocked", "auth_required", "verification_required", "quota_exceeded", "error"} {
		t.Run(status, func(t *testing.T) {
			a, _ := testApp(t)
			sessionForTest(t, a)
			for _, kind := range []string{"import", "scan", "verify"} {
				retryFixtureJob(t, a, "a", kind, "paused", JobData{})
			}
			_, _ = a.Store.DB.Exec(`UPDATE accounts SET status=?,error='明确的账号问题' WHERE id='a'`, status)
			result := retryAllForTest(t, a)
			want := 1
			if status == "quota_exceeded" {
				want = 2
			}
			if result.Restarted != want || result.Skipped != 3-want || result.SkippedReasons["account_unavailable"] != 3-want || !strings.Contains(result.Message, "明确的账号问题") {
				t.Fatal(result)
			}
		})
	}
	a, _ := testApp(t)
	sessionForTest(t, a)
	j := retryFixtureJob(t, a, "a", "import", "failed", JobData{})
	for _, tc := range []struct {
		account, csrf string
		status        int
	}{{"a", "", 403}, {"b", "test-csrf", 409}, {"", "test-csrf", 400}} {
		w := request(t, a.Handler(nil), "POST", "/api/v1/jobs/retry-all", map[string]string{"account_id": tc.account}, tc.csrf)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	after, _ := a.Store.Job(j.ID)
	if !reflect.DeepEqual(j, after) {
		t.Fatal("rejected request changed job")
	}
}

func TestBatchRetryInvalidCheckpointAndAtomicFailure(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	bad := retryFixtureJob(t, a, "a", "import", "failed", JobData{})
	_, _ = a.Store.DB.Exec(`UPDATE jobs SET data='{"node_ids":true}' WHERE id=?`, bad.ID)
	nullRecovery := retryFixtureJob(t, a, "a", "recover", "failed", JobData{})
	_, _ = a.Store.DB.Exec(`UPDATE jobs SET data='null' WHERE id=?`, nullRecovery.ID)
	retryFixtureJob(t, a, "a", "import", "failed", JobData{})
	result := retryAllForTest(t, a)
	if result.Restarted != 1 || result.SkippedReasons["invalid_data"] != 2 {
		t.Fatal(result)
	}
	first := retryFixtureJob(t, a, "a", "import", "failed", JobData{})
	second := retryFixtureJob(t, a, "a", "import", "failed", JobData{})
	_, _ = a.Store.DB.Exec(`UPDATE jobs SET created=created+100 WHERE id=?`, first.ID)
	if _, err := a.Store.DB.Exec(`CREATE TRIGGER retry_abort BEFORE UPDATE ON jobs WHEN old.id='` + second.ID + `' AND new.state='queued' BEGIN SELECT RAISE(ABORT,'test abort'); END`); err != nil {
		t.Fatal(err)
	}
	w := request(t, a.Handler(nil), "POST", "/api/v1/jobs/retry-all", map[string]string{"account_id": "a"}, "test-csrf")
	if w.Code != 500 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, j := range []Job{first, second} {
		after, _ := a.Store.Job(j.ID)
		if after.State != "failed" || after.Attempts != j.Attempts || string(after.Data) != string(j.Data) {
			t.Fatal("partial transaction committed", after)
		}
	}
}

func TestBatchRetryConcurrentRequestsQueueOriginalsOnce(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	for i := 0; i < 8; i++ {
		retryFixtureJob(t, a, "a", "teldrive_upload", "failed", JobData{})
	}
	h := a.Handler(nil)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			responses <- request(t, h, "POST", "/api/v1/jobs/retry-all", map[string]string{"account_id": "a"}, "test-csrf")
		}()
	}
	close(start)
	restarted := 0
	for i := 0; i < 2; i++ {
		w := <-responses
		var result jobsRetryResult
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		restarted += result.Restarted
	}
	var count int
	_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE state='queued'`).Scan(&count)
	if restarted != 8 || count != 8 {
		t.Fatal("concurrent retry duplicated work", restarted, count)
	}
}

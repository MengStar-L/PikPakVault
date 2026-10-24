package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func cachePriorityJob(t *testing.T, a *App, account, kind string, d JobData) Job {
	t.Helper()
	j, err := a.Store.NewJob(account, kind, kind, d)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func selectedLane(jobs []Job, lane string) string {
	for _, j := range jobs {
		if jobLane(j) == lane {
			return j.ID
		}
	}
	return ""
}

func TestSchedulerPrioritizesSubmittedUploadBeforeNewDownloads(t *testing.T) {
	a, _ := testApp(t)
	fresh := cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"fresh"}})
	cloud := cachePriorityJob(t, a, "a", "import", JobData{})
	resume := cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"sent"}, Uploads: map[string]*TelDriveUpload{"sent": {RemoteID: "accepted-file", Secret: "original-ticket"}}})
	resume.State, resume.NextRun = "waiting", now()-1
	if err := a.Store.SaveJob(&resume, nil); err != nil {
		t.Fatal(err)
	}
	jobs := a.runnableJobs(map[string]bool{})
	if len(jobs) != 2 || selectedLane(jobs, telDriveLane) != resume.ID || selectedLane(jobs, cloudLane) != cloud.ID {
		t.Fatal("queued fresh download displaced existing upload or blocked cloud lane", jobs, fresh.ID)
	}
	if jobs := a.runnableJobs(map[string]bool{telDriveLane: true}); len(jobs) != 1 || jobs[0].ID != cloud.ID {
		t.Fatal("busy upload worker was overbooked", jobs)
	}
	// Selection itself must not consume credentials, alter state, or reset the
	// upstream retry clock. A new scheduler sees the same durable reservation.
	after, _ := a.Store.Job(resume.ID)
	if string(after.Data) != string(resume.Data) || after.NextRun != resume.NextRun || after.State != resume.State {
		t.Fatal("scheduling changed original upload checkpoint", after)
	}
	if jobs = NewApp(a.Store).runnableJobs(map[string]bool{}); selectedLane(jobs, telDriveLane) != resume.ID {
		t.Fatal("reservation lost on scheduler restart", jobs)
	}
}

func TestSchedulerWaitsForCachedUploadDelayWithoutBlockingCloud(t *testing.T) {
	a, _ := testApp(t)
	cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"fresh"}})
	cloud := cachePriorityJob(t, a, "a", "import", JobData{})
	resume := cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"sent"}, Uploads: map[string]*TelDriveUpload{"sent": {Attempted: true, RemoteID: "accepted-file"}}})
	resume.State, resume.NextRun = "retry", now()+300
	if err := a.Store.SaveJob(&resume, nil); err != nil {
		t.Fatal(err)
	}
	jobs := a.runnableJobs(map[string]bool{})
	if len(jobs) != 1 || jobs[0].ID != cloud.ID {
		t.Fatal("started another download or ignored existing upload cooldown", jobs)
	}
	resume.NextRun = now() - 1
	if err := a.Store.SaveJob(&resume, nil); err != nil {
		t.Fatal(err)
	}
	if jobs = a.runnableJobs(map[string]bool{}); selectedLane(jobs, telDriveLane) != resume.ID {
		t.Fatal("upload did not become runnable after its original delay", jobs)
	}
}

func TestSchedulerInactiveOrFinishedUploadDoesNotReserveCacheLane(t *testing.T) {
	for _, state := range []string{"paused", "failed", "attention", "completed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			a, _ := testApp(t)
			fresh := cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"fresh"}})
			resume := cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"sent"}, Uploads: map[string]*TelDriveUpload{"sent": {RemoteID: "accepted-file"}}})
			resume.State, resume.NextRun = state, now()+600
			if err := a.Store.SaveJob(&resume, nil); err != nil {
				t.Fatal(err)
			}
			if jobs := a.runnableJobs(map[string]bool{}); len(jobs) != 1 || jobs[0].ID != fresh.ID {
				t.Fatal("inactive historical upload blocks new work", jobs)
			}
		})
	}
}

func TestSchedulerUploadPriorityRespectsAccountAndDoneNodes(t *testing.T) {
	a, _ := testApp(t)
	addAccount(t, a, "b", "user-b")
	cachePriorityJob(t, a, "b", "teldrive_upload", JobData{Uploads: map[string]*TelDriveUpload{"other": {RemoteID: "other-file"}}})
	fresh := cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"fresh"}})
	finished := cachePriorityJob(t, a, "a", "recover", JobData{RecoveryLane: telDriveLane, NodeIDs: []string{"done"}, Done: map[string]bool{"done": true}, Uploads: map[string]*TelDriveUpload{"done": {RemoteID: "saved-file"}}})
	finished.NextRun = now() + 600
	if err := a.Store.SaveJob(&finished, nil); err != nil {
		t.Fatal(err)
	}
	if jobs := a.runnableJobs(map[string]bool{}); len(jobs) != 1 || jobs[0].ID != fresh.ID {
		t.Fatal("other account or finished node held the upload lane", jobs)
	}
	if _, err := a.Store.DB.Exec(`UPDATE accounts SET status='blocked' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if jobs := a.runnableJobs(map[string]bool{}); len(jobs) != 0 {
		t.Fatal("priority bypassed account health restriction", jobs)
	}
	if err := a.Store.Set("active_account", "b"); err != nil {
		t.Fatal(err)
	}
	if jobs := a.runnableJobs(map[string]bool{}); len(jobs) != 1 || jobs[0].AccountID != "b" {
		t.Fatal("switched account retained original lane reservation", jobs)
	}
}

func TestSchedulerRecognizesOnlyCompleteOwnedDownloadCache(t *testing.T) {
	t.Setenv("VAULT_PROGRAM_DIR", t.TempDir())
	a, _ := testApp(t)
	fresh := cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"fresh"}})
	d := JobData{NodeIDs: []string{"cached"}, Uploads: map[string]*TelDriveUpload{"cached": {}}}
	resume := cachePriorityJob(t, a, "a", "teldrive_upload", d)
	resume.NextRun = now() + 60
	if err := a.Store.SaveJob(&resume, nil); err != nil {
		t.Fatal(err)
	}
	dir := a.telDriveCacheDir(resume.ID, "cached")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	owner, _ := json.Marshal(cacheOwner{JobID: resume.ID, NodeID: "cached"})
	write("owner.json", owner)
	write("content", []byte("body"))
	write("source.json", []byte(`{"Complete":false,"Size":4}`))
	if jobs := a.runnableJobs(map[string]bool{}); len(jobs) != 1 || jobs[0].ID != fresh.ID {
		t.Fatal("partial download incorrectly reserved lane", jobs)
	}
	write("source.json", []byte(`{"Complete":true,"Size":4}`))
	if jobs := a.runnableJobs(map[string]bool{}); len(jobs) != 0 {
		t.Fatal("complete download did not reserve pending resume", jobs)
	}
	write("content.aria2", []byte("incomplete control file"))
	if a.telDriveResumeNode(resume, &d, "cached") {
		t.Fatal("incomplete controlled cache treated as complete")
	}
	if err := os.Remove(filepath.Join(dir, "content.aria2")); err != nil {
		t.Fatal(err)
	}
	write("owner.json", []byte(`{"job_id":"foreign","node_id":"cached"}`))
	if a.telDriveResumeNode(resume, &d, "cached") {
		t.Fatal("foreign cache reserved worker")
	}
	write("owner.json", owner)
	write("unknown-file", []byte("do not touch"))
	if a.telDriveResumeNode(resume, &d, "cached") {
		t.Fatal("unvalidated cache reserved worker")
	}
}

func TestSchedulerLegacyRecoveryKeepsUploadReservationAndCloudSibling(t *testing.T) {
	a := recoveryPartitionFixture(t)
	fresh := cachePriorityJob(t, a, "a", "teldrive_upload", JobData{NodeIDs: []string{"td-b"}})
	legacy := cachePriorityJob(t, a, "a", "recover", JobData{NodeIDs: []string{"td-a", "cloud-a"}, Uploads: map[string]*TelDriveUpload{"td-a": {RemoteID: "sent-before-restart", Secret: "original-sealed-session"}}})
	legacy.State, legacy.NextRun = "waiting", now()+60
	if err := a.Store.SaveJob(&legacy, nil); err != nil {
		t.Fatal(err)
	}
	if jobs := a.runnableJobs(map[string]bool{}); len(jobs) != 0 {
		t.Fatal("new download ran while legacy upload awaited confirmation", jobs, fresh.ID)
	}
	jobs := a.runnableJobs(map[string]bool{})
	if len(jobs) != 1 || jobLane(jobs[0]) != cloudLane || jobs[0].ID == legacy.ID {
		t.Fatal("cached legacy upload blocked independently partitioned cloud work", jobs)
	}
	after, _ := a.Store.Job(legacy.ID)
	d := recoveryJobData(t, after)
	if after.NextRun != legacy.NextRun || d.Uploads["td-a"].RemoteID != "sent-before-restart" || d.Uploads["td-a"].Secret != "original-sealed-session" || d.OperationID != legacy.ID {
		t.Fatal("partition lost original cache/session ownership", after)
	}
}

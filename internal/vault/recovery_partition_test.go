package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"pikpakvault/internal/pikpak"
)

func recoveryPartitionFixture(t *testing.T) *App {
	t.Helper()
	a, _ := testApp(t)
	for _, source := range []Source{
		{ID: "cloud-source", Kind: "magnet", Link: "magnet:?xt=urn:btih:test"},
		{ID: "td-source", Kind: "teldrive", Link: "https://teldrive.invalid/files/original"},
	} {
		if err := a.Store.SaveSource(source); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []Node{
		{ID: "parent", ParentID: "root", Name: "Mixed library", Kind: "folder"},
		{ID: "cloud-a", ParentID: "parent", Name: "A.mp4", Kind: "file", SourceID: "cloud-source"},
		{ID: "cloud-b", ParentID: "parent", Name: "B.mp4", Kind: "file", SourceID: "cloud-source"},
		{ID: "td-a", ParentID: "parent", Name: "C.mp4", Kind: "file", SourceID: "td-source"},
		{ID: "td-b", ParentID: "parent", Name: "D.mp4", Kind: "file", SourceID: "td-source"},
		{ID: "unselected", ParentID: "root", Name: "Outside.mp4", Kind: "file", SourceID: "cloud-source"},
	} {
		if err := a.Store.InsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func partitionForTest(t *testing.T, a *App, j *Job) {
	t.Helper()
	a.dataMu.RLock()
	defer a.dataMu.RUnlock()
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	if err := a.partitionRecovery(j); err != nil {
		t.Fatal(err)
	}
}

func recoveryJobData(t *testing.T, j Job) JobData {
	t.Helper()
	var d JobData
	if err := json.Unmarshal(j.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRecoveryPartitionKeepsSessionsBatchesAndResourceOwnership(t *testing.T) {
	t.Setenv("VAULT_PROGRAM_DIR", t.TempDir())
	a := recoveryPartitionFixture(t)
	secret, err := a.Store.Seal(pikpak.UploadSession{Sent: true})
	if err != nil {
		t.Fatal(err)
	}
	data := JobData{
		NodeIDs: []string{"parent"}, MonitorID: "monitor",
		Note: "TelDrive 文件已保存至 /old/file.mp4",
		Uploads: map[string]*TelDriveUpload{
			"td-a":       {Secret: secret, RemoteID: "upload-file", Attempted: true, Polls: 3},
			"deleted-td": {Secret: secret, RemoteID: "orphan-upload"},
		},
		Transfers: map[string]*TransferState{
			"cloud-source": {StageID: "original-stage", TaskID: "offline-task", Phase: "submitted", BeforeIDs: []string{"preexisting"}},
		},
		Done:          map[string]bool{"cloud-a": true, "td-a": true, "deleted-cloud": true, "deleted-td": true},
		Problems:      map[string]string{"cloud-b": "source delayed", "td-b": "download interrupted"},
		Instant:       map[string]string{"cloud-a": "instant-file"},
		InstantTried:  map[string]bool{"cloud-a": true, "cloud-b": true},
		InstantStates: map[string]*InstantState{"cloud-b": {Phase: "submitted", ID: "instant-pending", Before: []string{"old-id"}}},
	}
	j, err := a.Store.NewJob("a", "recover", "Legacy recovery", data)
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.NextRun, j.Attempts, j.Progress, j.Message = "waiting", now()+600, 4, 76, "aria2 下载中"
	if err = a.Store.SaveJob(&j, &data); err != nil {
		t.Fatal(err)
	}
	original := j
	cacheDir := a.telDriveCacheDir(j.ID, "td-b")
	if err = os.MkdirAll(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(cacheDir, "content")
	if err = os.WriteFile(cachePath, []byte("partially downloaded"), 0600); err != nil {
		t.Fatal(err)
	}
	partitionForTest(t, a, &j)
	td := recoveryJobData(t, j)
	if j.ID != original.ID || j.State != original.State || j.NextRun != original.NextRun || j.Attempts != original.Attempts || j.Progress != original.Progress {
		t.Fatal("TelDrive identity/backoff/progress changed", j)
	}
	if td.RecoveryLane != "teldrive" || !td.ExactNodes || td.OperationID != original.ID || !reflect.DeepEqual(td.NodeIDs, []string{"td-a", "td-b"}) {
		t.Fatal("incorrect TelDrive selection", td)
	}
	if !reflect.DeepEqual(td.Uploads, data.Uploads) || len(td.Transfers) != 0 || len(td.InstantStates) != 0 || len(td.Done) != 2 || !td.Done["td-a"] || !td.Done["deleted-td"] || len(td.Problems) != 1 || td.Problems["td-b"] == "" {
		t.Fatal("TelDrive session or checkpoint changed")
	}
	bytes, err := os.ReadFile(cachePath)
	if err != nil || string(bytes) != "partially downloaded" || a.telDriveCacheDir(j.ID, "td-b") != cacheDir {
		t.Fatal("original download cache changed", err)
	}
	var decoded pikpak.UploadSession
	if err = a.Store.Unseal(td.Uploads["td-a"].Secret, &decoded); err != nil || !decoded.Sent {
		t.Fatal("encrypted upload session not preserved", err)
	}
	jobs := repairJobs(t, a)
	if len(jobs) != 2 {
		t.Fatal("expected exactly two recovery lanes", len(jobs))
	}
	var child Job
	for _, candidate := range jobs {
		if candidate.ID != j.ID {
			child = candidate
		}
	}
	cloud := recoveryJobData(t, child)
	if cloud.RecoveryLane != "cloud" || !cloud.ExactNodes || cloud.OperationID != original.ID || cloud.MonitorID != "" || cloud.Note != "" {
		t.Fatal("cloud lane metadata lost", cloud)
	}
	if !reflect.DeepEqual(cloud.NodeIDs, []string{"cloud-a", "cloud-b", "parent"}) || len(cloud.Uploads) != 0 ||
		!reflect.DeepEqual(cloud.Transfers, data.Transfers) || !reflect.DeepEqual(cloud.InstantStates, data.InstantStates) ||
		len(cloud.Done) != 2 || !cloud.Done["cloud-a"] || !cloud.Done["deleted-cloud"] ||
		len(cloud.Problems) != 1 || cloud.Problems["cloud-b"] == "" {
		t.Fatal("cloud batch split or checkpoint lost", cloud)
	}
	if child.NextRun != 0 || child.State != "queued" || child.Attempts != original.Attempts || child.Created != original.Created || child.AccountID != original.AccountID {
		t.Fatal("cloud inherited TelDrive wait or lost original account/retry history", child)
	}
	operations, err := nodeJobs(a.Store.DB, "a")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range td.NodeIDs {
		if td.Done[id] {
			if _, exists := operations[id]; exists {
				t.Fatal("completed TelDrive item still reserved", id)
			}
			continue
		}
		if operations[id].JobID != original.ID {
			t.Fatal("TelDrive reservation changed", id, operations[id])
		}
	}
	for _, id := range cloud.NodeIDs {
		if cloud.Done[id] {
			if _, exists := operations[id]; exists {
				t.Fatal("completed cloud item still reserved", id)
			}
			continue
		}
		if operations[id].JobID != child.ID {
			t.Fatal("cloud reservation overlaps old recovery", id, operations[id])
		}
	}
	if _, exists := operations["unselected"]; exists {
		t.Fatal("selection expanded beyond original descendants")
	}
	// A new descendant is not part of the user's frozen recovery selection.
	if err = a.Store.InsertNode(Node{ID: "later", ParentID: "parent", Name: "Later.mp4", Kind: "file"}); err != nil {
		t.Fatal(err)
	}
	operations, err = nodeJobs(a.Store.DB, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := operations["later"]; exists {
		t.Fatal("split recovery captured a future descendant")
	}
	// A new scheduler instance must reuse both durable jobs, not split again.
	restarted := NewApp(a.Store)
	partitionForTest(t, restarted, &j)
	partitionForTest(t, restarted, &child)
	if len(repairJobs(t, restarted)) != 2 {
		t.Fatal("restart created another sibling")
	}
}

func TestRecoveryPartitionRereadsPausedJobsAndResumesOnlyOnRetry(t *testing.T) {
	for _, state := range []string{"paused", "failed", "partial", "attention", "cancelled", "completed", "running"} {
		t.Run(state, func(t *testing.T) {
			a := recoveryPartitionFixture(t)
			j, err := a.Store.NewJob("a", "recover", "Original title", JobData{NodeIDs: []string{"parent"}})
			if err != nil {
				t.Fatal(err)
			}
			stale := j
			j.State = state
			if err = a.Store.SaveJob(&j, nil); err != nil {
				t.Fatal(err)
			}
			partitionForTest(t, a, &stale)
			if stale.State != state || stale.Title != "Original title" || recoveryJobData(t, stale).RecoveryLane != "" || len(repairJobs(t, a)) != 1 {
				t.Fatal("ineligible/stale job was split or resumed", stale)
			}
			// Explicit retry, or the established running->queued restart path.
			j.State = "queued"
			if err = a.Store.SaveJob(&j, nil); err != nil {
				t.Fatal(err)
			}
			partitionForTest(t, a, &j)
			if len(repairJobs(t, a)) != 2 || recoveryJobData(t, j).RecoveryLane != "teldrive" {
				t.Fatal("eligible legacy job did not split")
			}
		})
	}
}

func TestRecoveryPartitionExactAndSingleLaneSelections(t *testing.T) {
	for _, test := range []struct {
		name string
		data JobData
		lane string
		ids  []string
	}{
		{"cloud", JobData{NodeIDs: []string{"cloud-a", "cloud-b"}}, "cloud", []string{"cloud-a", "cloud-b"}},
		{"teldrive", JobData{NodeIDs: []string{"td-a", "td-b"}}, "teldrive", []string{"td-a", "td-b"}},
		{"folder-exact", JobData{NodeIDs: []string{"parent"}, ExactNodes: true}, "cloud", []string{"parent"}},
		{"root-exact", JobData{NodeIDs: []string{"root"}, ExactNodes: true}, "cloud", []string{"root"}},
		{"empty-exact", JobData{NodeIDs: []string{}, ExactNodes: true}, "cloud", []string{}},
		{"deleted-exact", JobData{NodeIDs: []string{"no-longer-present"}, ExactNodes: true}, "cloud", []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := recoveryPartitionFixture(t)
			j, err := a.Store.NewJob("a", "recover", "Legacy recovery", test.data)
			if err != nil {
				t.Fatal(err)
			}
			id := j.ID
			partitionForTest(t, a, &j)
			d := recoveryJobData(t, j)
			if j.ID != id || d.RecoveryLane != test.lane || !d.ExactNodes || !slices.Equal(d.NodeIDs, test.ids) || len(repairJobs(t, a)) != 1 {
				t.Fatal("single lane or exact selection changed", j, d)
			}
			if len(test.ids) == 0 {
				if j.State != "completed" {
					t.Fatal("empty selection was not released", j.State)
				}
				ops, err := nodeJobs(a.Store.DB, "a")
				if err != nil || len(ops) != 0 {
					t.Fatal("empty exact selection reserved library", ops, err)
				}
			}
		})
	}
}

func TestRecoveryPartitionInsertAndOriginalUpdateAreAtomic(t *testing.T) {
	a := recoveryPartitionFixture(t)
	j, err := a.Store.NewJob("a", "recover", "Legacy recovery", JobData{NodeIDs: []string{"parent"}})
	if err != nil {
		t.Fatal(err)
	}
	original := j
	if _, err = a.Store.DB.Exec(`CREATE TRIGGER reject_partition BEFORE UPDATE ON jobs
		WHEN NEW.title LIKE 'TelDrive%'
		BEGIN SELECT RAISE(ABORT, 'forced partition update failure'); END`); err != nil {
		t.Fatal(err)
	}
	a.dataMu.RLock()
	a.jobMu.Lock()
	err = a.partitionRecovery(&j)
	a.jobMu.Unlock()
	a.dataMu.RUnlock()
	if err == nil {
		t.Fatal("expected forced transaction failure")
	}
	saved, err := a.Store.Job(original.ID)
	if err != nil || saved.ID != original.ID || saved.Title != original.Title || saved.State != original.State || string(saved.Data) != string(original.Data) || len(repairJobs(t, a)) != 1 {
		t.Fatal("partial split survived rollback", saved, err)
	}
}

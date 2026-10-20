package vault

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPartialRecoveryCompletedFileCanBeScannedAndRecoveredAgain(t *testing.T) {
	a, f, _, file, other := scanProtectionFixture(t)
	sessionForTest(t, a)
	data := JobData{
		NodeIDs: []string{file.ID, other.ID}, ExactNodes: true,
		Done: map[string]bool{file.ID: true}, Problems: map[string]string{other.ID: "source unavailable"},
	}
	original, err := a.Store.NewJob("a", "recover", "partial recovery", data)
	if err != nil {
		t.Fatal(err)
	}
	original.State = "partial"
	if err = a.Store.SaveJob(&original, &data); err != nil {
		t.Fatal(err)
	}
	delete(f.files, file.RemoteID)
	delete(f.files, other.RemoteID)
	if err = a.checkLibrary(context.Background(), f, &Job{AccountID: "a"}); err != nil {
		t.Fatal(err)
	}
	assertScanState(t, a, file.ID, "missing")
	// The unfinished item remains owned by the original recovery, so scanning
	// must not interfere with a saved upload checkpoint or its binding.
	assertScanState(t, a, other.ID, "present")
	preview := recoveryPreviewForTest(t, a, []string{file.ID})
	if len(preview.Items) != 1 || preview.Skipped != 0 {
		t.Fatal("completed checkpoint still blocks real later loss", preview)
	}
	w := request(t, a.Handler(nil), "POST", "/api/v1/recovery",
		recoveryInput{IDs: []string{file.ID}, AccountID: "a", Confirm: true}, "test-csrf")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var next Job
	if err = json.Unmarshal(w.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	f.instantHit = true
	requireComplete(t, execute(t, a, &next))
	assertScanState(t, a, file.ID, "present")
	// Finishing the replacement recovery does not silently resume or alter
	// the original partial task and its remaining problem.
	saved, err := a.Store.Job(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	var savedData JobData
	if err = json.Unmarshal(saved.Data, &savedData); err != nil {
		t.Fatal(err)
	}
	if saved.State != "partial" || !savedData.Done[file.ID] || savedData.Problems[other.ID] != "source unavailable" {
		t.Fatal("new recovery changed original checkpoint", saved.State, savedData)
	}
	recovered, err := a.Store.Node(file.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	instantCalls := f.calls["instant"]
	w = request(t, a.Handler(nil), "POST", "/api/v1/jobs/"+original.ID+"/retry", map[string]any{}, "test-csrf")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	requireComplete(t, execute(t, a, &original))
	afterRetry, err := a.Store.Node(file.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	if afterRetry.RemoteID != recovered.RemoteID || f.calls["instant"] != instantCalls+1 {
		t.Fatal("retry restored an already completed item instead of only the remaining item")
	}
	assertScanState(t, a, other.ID, "present")
}

func TestCompletedRecoveryParentStillReservesUnfinishedDescendants(t *testing.T) {
	for _, exact := range []bool{false, true} {
		name := "expanded"
		if exact {
			name = "exact"
		}
		t.Run(name, func(t *testing.T) {
			a, _, parent, file, other := scanProtectionFixture(t)
			data := JobData{NodeIDs: []string{parent.ID}, ExactNodes: exact, Done: map[string]bool{parent.ID: true}}
			job, err := a.Store.NewJob("a", "recover", "partial directory recovery", data)
			if err != nil {
				t.Fatal(err)
			}
			job.State = "partial"
			if err = a.Store.SaveJob(&job, &data); err != nil {
				t.Fatal(err)
			}
			owners, err := nodeJobs(a.Store.DB, "a")
			if err != nil {
				t.Fatal(err)
			}
			if _, owned := owners[parent.ID]; owned {
				t.Fatal("completed directory still reserved")
			}
			owner, owned := owners[file.ID]
			if owned == exact || owned && owner.JobID != job.ID {
				t.Fatal("directory expansion lost pending child or expanded exact selection", owners)
			}
			if _, owned := owners[other.ID]; owned {
				t.Fatal("directory recovery expanded into sibling")
			}
			protected, err := scanProtectedNodes(a.Store.DB, "a")
			if err != nil {
				t.Fatal(err)
			}
			if protected[parent.ID] == exact || protected[file.ID] == exact {
				t.Fatal("implicit parent protection must follow pending child", protected)
			}
			owners, err = nodeJobs(a.Store.DB, "other-account")
			if err != nil || len(owners) != 0 {
				t.Fatal("reservation crossed accounts", owners, err)
			}
		})
	}
}

func TestRecoveryDoneFlagsNeverReleaseActiveUpload(t *testing.T) {
	a, _, _, file, _ := scanProtectionFixture(t)
	// An upload is one operation; its checkpoints and ownership stay intact
	// until the job completes, even if legacy data happens to contain Done.
	job, err := a.Store.NewJob("a", "teldrive_upload", "upload", JobData{
		NodeIDs: []string{file.ID}, Done: map[string]bool{file.ID: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	job.State = "running"
	if err = a.Store.SaveJob(&job, nil); err != nil {
		t.Fatal(err)
	}
	owners, err := nodeJobs(a.Store.DB, "a")
	if err != nil || owners[file.ID].JobID != job.ID {
		t.Fatal("active upload lost its reservation", owners, err)
	}
	protected, err := scanProtectedNodes(a.Store.DB, "a")
	if err != nil || !protected[file.ID] {
		t.Fatal("active upload lost scan protection", protected, err)
	}
}

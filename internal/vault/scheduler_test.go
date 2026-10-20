package vault

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"pikpakvault/internal/pikpak"
)

// The data transfer deliberately does not hold the control-plane mutex, just
// like Client.ContinueUpload. Keeping this distinction catches false parallelism.
type laneUploadDrive struct {
	lockedProvider
	upload    *uploadFake
	start     chan struct{}
	release   chan struct{}
	unwinding chan struct{}
}

func (f *laneUploadDrive) BeginUpload(ctx context.Context, parent string, file pikpak.File) (pikpak.UploadTicket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upload.BeginUpload(ctx, parent, file)
}

func (f *laneUploadDrive) ContinueUpload(ctx context.Context, session *pikpak.UploadSession, size int64, read pikpak.RangeReader, save func(int64) error) error {
	close(f.start)
	select {
	case <-f.release:
	case <-ctx.Done():
		if f.unwinding != nil {
			close(f.unwinding)
			<-f.release
		}
		return ctx.Err()
	}
	// Checkpoints must remain outside the provider lock just like real upload IO.
	if err := save(0); err != nil {
		return err
	}
	body, err := read(ctx, 0, size)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.upload.sends++
	f.upload.data = string(body)
	file := f.f.files[session.Ticket.File.ID]
	file.Phase = "PHASE_TYPE_COMPLETE"
	f.f.files[file.ID] = file
	f.mu.Unlock()
	session.Sent = true
	return save(size)
}

func schedulerFixture(t *testing.T) (*App, *laneUploadDrive, Job) {
	t.Helper()
	a, fake := testApp(t)
	fake.instantHit = true
	drive := &laneUploadDrive{lockedProvider: lockedProvider{f: fake, mu: &sync.Mutex{}},
		upload: &uploadFake{fakeDrive: fake}, start: make(chan struct{}), release: make(chan struct{})}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, telDriveFixture(t))
	requireComplete(t, runTDScan(t, a, m))
	// Keep this test focused on transfers, not automatic periodic work.
	a.Store.Set("scan_minutes", "0")
	a.Store.Set("cleanup_check:a", jsonText(now()))
	if err := a.prepareRoot(context.Background(), drive, "a"); err != nil {
		t.Fatal(err)
	}
	return a, drive, tdUploadJob(t, a, "a")
}

func awaitLaneSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(20 * time.Second):
		t.Fatal("worker did not reach expected phase")
	}
}

func awaitJobState(t *testing.T, a *App, id, state string) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, err := a.Store.Job(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == state {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := a.Store.Job(id)
	t.Fatalf("expected %s, got %s: %s", state, j.State, j.Message)
	return j
}

func startScheduler(t *testing.T, a *App) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	a.notify()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("scheduler failed to shut down")
		}
	})
	return cancel, done
}

func TestSchedulerCloudRecoveryCompletesDuringTelDriveUpload(t *testing.T) {
	a, drive, td := schedulerFixture(t)
	_, _ = startScheduler(t, a)
	defer close(drive.release)
	awaitLaneSignal(t, drive.start)
	// The cloud task arrives only after the upload has actually started.
	n := Node{ID: "cloud-lane-file", ParentID: "root", Name: "quick.mp4", Kind: "file", Size: 42, Hash: "cloud-hash"}
	if err := a.Store.InsertNode(n); err != nil {
		t.Fatal(err)
	}
	cloud, err := a.Store.NewJob("a", "recover", "cloud test", JobData{NodeIDs: []string{n.ID}})
	if err != nil {
		t.Fatal(err)
	}
	a.notify()
	result := awaitJobState(t, a, cloud.ID, "completed")
	if result.Lane != cloudLane {
		t.Fatal("missing lane metadata", result.Lane)
	}
	stillRunning, _ := a.Store.Job(td.ID)
	if stillRunning.State != "running" {
		t.Fatal("upload unexpectedly completed before release", stillRunning.State)
	}
	if file, _ := a.Store.Node(n.ID, "a"); file.State != "present" {
		t.Fatal("cloud resource did not recover")
	}
	// Even a direct concurrent Execute or immediate pause/retry cannot re-enter.
	duplicate := stillRunning
	returned := make(chan struct{})
	go func() { a.Execute(context.Background(), &duplicate); close(returned) }()
	awaitLaneSignal(t, returned)
	w := request(t, a.Handler(nil), "POST", "/api/v1/jobs/"+td.ID+"/pause", map[string]any{}, "test-csrf")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(t, a.Handler(nil), "POST", "/api/v1/jobs/"+td.ID+"/retry", map[string]any{}, "test-csrf")
	if w.Code != 409 {
		t.Fatal("retry reentered an unwinding request", w.Code, w.Body.String())
	}
	drive.mu.Lock()
	begins := drive.upload.begins
	drive.mu.Unlock()
	if begins != 1 {
		t.Fatal("duplicate upload ticket", begins)
	}
}

func TestSchedulerWaitsForUploadOnShutdown(t *testing.T) {
	a, drive, td := schedulerFixture(t)
	drive.unwinding = make(chan struct{})
	cancel, done := startScheduler(t, a)
	defer close(drive.release)
	awaitLaneSignal(t, drive.start)
	cancel()
	awaitLaneSignal(t, drive.unwinding)
	select {
	case <-done:
		t.Fatal("Run returned before the upload saved its checkpoint")
	case <-time.After(50 * time.Millisecond):
	}
	saved, err := a.Store.Job(td.ID)
	if err != nil {
		t.Fatal(err)
	}
	var data JobData
	if err = json.Unmarshal(saved.Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, upload := range data.Uploads {
		if upload.Secret == "" || upload.RemoteID == "" {
			t.Fatal("in-flight session was not checkpointed")
		}
	}
}

func TestSchedulerKeepsOneJobPerLaneAndAccountRestrictions(t *testing.T) {
	a := recoveryPartitionFixture(t)
	for _, id := range []string{"td-a", "td-b", "cloud-a", "cloud-b"} {
		if _, err := a.Store.NewJob("a", "recover", id, JobData{NodeIDs: []string{id}}); err != nil {
			t.Fatal(err)
		}
	}
	selected := a.runnableJobs(map[string]bool{})
	if len(selected) != 2 || jobLane(selected[0]) == jobLane(selected[1]) {
		t.Fatal("expected one job per lane", selected)
	}
	selected = a.runnableJobs(map[string]bool{telDriveLane: true})
	if len(selected) != 1 || jobLane(selected[0]) != cloudLane {
		t.Fatal("busy upload lane blocked cloud work or overbooked", selected)
	}
	a.observeAccountError("a", &pikpak.APIError{Code: "user_blocked"})
	if selected = a.runnableJobs(map[string]bool{}); len(selected) != 0 {
		t.Fatal("frozen account still scheduled", selected)
	}
	addAccount(t, a, "b", "user-b")
	a.Store.Set("active_account", "b")
	if selected = a.runnableJobs(map[string]bool{}); len(selected) != 0 {
		t.Fatal("old account continued after switching", selected)
	}
}

func TestSchedulerPartitionsDelayedLegacyRecoveryWithoutDelayingCloud(t *testing.T) {
	a := recoveryPartitionFixture(t)
	j, err := a.Store.NewJob("a", "recover", "legacy mixed", JobData{NodeIDs: []string{"cloud-a", "td-a"}})
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.NextRun = "waiting", now()+300
	if err := a.Store.SaveJob(&j, nil); err != nil {
		t.Fatal(err)
	}
	if jobs := a.runnableJobs(map[string]bool{}); len(jobs) != 0 {
		t.Fatal("original TelDrive delay was ignored", jobs)
	}
	jobs := a.runnableJobs(map[string]bool{})
	if len(jobs) != 1 || jobs[0].ID == j.ID || jobLane(jobs[0]) != cloudLane {
		t.Fatal("cloud recovery inherited TelDrive delay", jobs)
	}
	original, _ := a.Store.Job(j.ID)
	if original.NextRun != j.NextRun || original.State != "waiting" {
		t.Fatal("legacy resume state changed")
	}
}

func TestConcurrentFolderResolutionCreatesOneSharedParent(t *testing.T) {
	a, fake := testApp(t)
	drive := lockedProvider{f: fake, mu: &sync.Mutex{}}
	if err := a.Store.InsertNode(Node{ID: "shared-folder", ParentID: "root", Name: "Shared", Kind: "folder"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), operationKey{}, &operationCache{folders: map[string]resolvedFolder{}})
			id, err := a.folder(ctx, drive, "a", "shared-folder", map[string]bool{})
			results <- id
			errs <- err
		}()
	}
	wg.Wait()
	if first, second := <-results, <-results; first == "" || first != second {
		t.Fatal("duplicate folders", first, second)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	// My Pack + dedicated root + one shared folder.
	if fake.calls["mkdir"] != 3 {
		t.Fatal("duplicate root/parent creation", fake.calls["mkdir"])
	}
}

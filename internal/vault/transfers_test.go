package vault

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
)

type alteredDrive struct {
	*fakeDrive
	afterOffline func(*pikpak.Transfer)
	afterShare   func(*pikpak.Transfer)
	taskError    error
	offlineError error
}

func (f *alteredDrive) Offline(ctx context.Context, link, parent string) (pikpak.Transfer, error) {
	if f.offlineError != nil {
		return pikpak.Transfer{}, f.offlineError
	}
	r, err := f.fakeDrive.Offline(ctx, link, parent)
	if err == nil && f.afterOffline != nil {
		f.afterOffline(&r)
	}
	return r, err
}
func (f *alteredDrive) RestoreShare(ctx context.Context, id, token string, ids []string, parent string) (pikpak.Transfer, error) {
	r, err := f.fakeDrive.RestoreShare(ctx, id, token, ids, parent)
	if err == nil && f.afterShare != nil {
		f.afterShare(&r)
	}
	return r, err
}
func (f *alteredDrive) Tasks(ctx context.Context) ([]pikpak.Task, error) {
	if f.taskError != nil {
		return nil, f.taskError
	}
	return f.fakeDrive.Tasks(ctx)
}
func loadJobData(t *testing.T, j Job) JobData {
	t.Helper()
	var d JobData
	if err := json.Unmarshal(j.Data, &d); err != nil {
		t.Fatal(err)
	}
	d.init()
	return d
}
func ageTransfer(t *testing.T, a *App, j *Job) {
	t.Helper()
	d := loadJobData(t, *j)
	for _, v := range d.Transfers {
		v.StableSince = now() - 31
	}
	if err := a.Store.SaveJob(j, &d); err != nil {
		t.Fatal(err)
	}
}

func TestDirectImportHasNoStagingMovesOrRenames(t *testing.T) {
	a, f := testApp(t)
	root, err := a.ensureRoot(context.Background(), f, "a")
	if err != nil {
		t.Fatal(err)
	}
	f.calls = map[string]int{}
	f.outputs = sampleOutputs()
	f.files["someone-else"] = pikpak.File{ID: "someone-else", ParentID: root, Name: "personal.txt", Kind: "drive#file", Phase: "PHASE_TYPE_COMPLETE"}
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	for _, method := range []string{"mkdir", "move", "rename", "trash"} {
		if f.calls[method] != 0 {
			t.Fatalf("unnecessary %s: %d", method, f.calls[method])
		}
	}
	nodes, _ := a.Store.AllNodes("a")
	if len(nodes) != 4 {
		t.Fatalf("adopted unrelated files: %d", len(nodes))
	}
	if f.calls["get"] > 6 {
		t.Fatalf("repeated parent verification: %v", f.calls)
	}
	if d := loadJobData(t, j); d.Transfers[d.SourceID].Mode != "direct" {
		t.Fatal("missing durable destination")
	}
}
func TestLegacyShareAssociationMovesOnlyOnce(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	for i, r := range f.outputs {
		kind := "file"
		if r.File.Folder() {
			kind = "folder"
		}
		f.shareEntries = append(f.shareEntries, Entry{ID: string(rune('a' + i)), Path: r.Path, Name: r.File.Name, Kind: kind, Size: int64(r.File.Size), Hash: r.File.Hash})
	}
	// Names in a publisher manifest are independent of the fake output templates.
	for i := range f.shareEntries {
		parts := strings.Split(f.shareEntries[i].Path, "/")
		f.shareEntries[i].Name = parts[len(parts)-1]
	}
	f.shareOutside, f.shareResponseIDs = true, true
	j := createImport(t, a, "share")
	root, _ := a.ensureRoot(context.Background(), f, "a")
	result, _ := f.RestoreShare(context.Background(), "share", "pass-token", []string{"a"}, root)
	d := loadJobData(t, j)
	d.Transfers[d.SourceID] = &TransferState{Mode: "direct", TargetID: root, Phase: "submitted", Expected: f.shareEntries, OutputIDs: []string{result.Files[0].ID}, Started: now() - 500}
	a.Store.SaveJob(&j, &d)
	requireComplete(t, execute(t, a, &j))
	if f.calls["move"] != 1 || f.calls["share_restore"] != 1 {
		t.Fatalf("double movement: %v", f.calls)
	}
}
func TestStalledTaskVerifiesRealTreeWithoutResubmission(t *testing.T) {
	for _, phase := range []string{"PHASE_TYPE_RUNNING", "PHASE_TYPE_ERROR"} {
		t.Run(phase, func(t *testing.T) {
			a, f := testApp(t)
			f.outputs = sampleOutputs()
			w := &alteredDrive{fakeDrive: f}
			w.afterOffline = func(r *pikpak.Transfer) { f.tasks[0].Phase = phase; f.tasks[0].Progress = 99; r.Task = &f.tasks[0] }
			a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
			j := createImport(t, a, "magnet")
			if got := execute(t, a, &j); got.State != "waiting" || !strings.Contains(got.Message, "复核") {
				t.Fatal(got.State, got.Message)
			}
			ageTransfer(t, a, &j)
			requireComplete(t, execute(t, a, &j))
			d := loadJobData(t, j)
			if !d.Transfers[d.SourceID].VerifiedByFiles || f.calls["offline"] != 1 {
				t.Fatal("stalled result was not reconciled")
			}
		})
	}
}
func TestStalledIncompleteEmptyAndChangingTreesDoNotFinish(t *testing.T) {
	for _, variant := range []string{"incomplete", "empty", "changing", "page-error", "size-mismatch"} {
		t.Run(variant, func(t *testing.T) {
			a, f := testApp(t)
			f.outputs = sampleOutputs()
			w := &alteredDrive{fakeDrive: f}
			w.afterOffline = func(r *pikpak.Transfer) { f.tasks[0].Phase = "PHASE_TYPE_RUNNING"; r.Task = &f.tasks[0] }
			a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
			j := createImport(t, a, "magnet")
			execute(t, a, &j)
			ageTransfer(t, a, &j)
			switch variant {
			case "incomplete":
				for id, v := range f.files {
					if !v.Folder() {
						v.Phase = "PHASE_TYPE_RUNNING"
						f.files[id] = v
						break
					}
				}
			case "empty":
				for id, v := range f.files {
					if !v.Folder() {
						delete(f.files, id)
					}
				}
			case "changing":
				for id, v := range f.files {
					if !v.Folder() {
						v.Size++
						f.files[id] = v
						break
					}
				}
			case "page-error":
				f.failPage = f.tasks[0].FileID
			case "size-mismatch":
				v := f.files[f.tasks[0].FileID]
				v.Size = 10000
				f.files[v.ID] = v
			}
			got := execute(t, a, &j)
			if got.State == "completed" {
				t.Fatal("premature completion")
			}
			if f.calls["offline"] != 1 {
				t.Fatal("resubmitted")
			}
			nodes, _ := a.Store.AllNodes("a")
			if len(nodes) != 0 {
				t.Fatal("registered incomplete tree")
			}
		})
	}
}
func TestLostResponseAmbiguousTasksNeverAdoptsConcurrentFiles(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	f.lostOffline = true
	j := createImport(t, a, "magnet")
	execute(t, a, &j)
	d := loadJobData(t, j)
	parent := d.Transfers[d.SourceID].TargetID
	files := f.putOutputs(parent)
	other := f.tasks[0]
	other.ID = "other-task"
	other.FileID = files[0].ID
	f.tasks = append(f.tasks, other)
	got := execute(t, a, &j)
	if got.State != "attention" || f.calls["offline"] != 1 || f.calls["move"] != 0 {
		t.Fatal(got.State, got.Message, f.calls)
	}
}
func TestTransferRejectsPreexistingAndPublisherIDs(t *testing.T) {
	for _, kind := range []string{"magnet", "share"} {
		t.Run(kind, func(t *testing.T) {
			a, f := testApp(t)
			root, _ := a.ensureRoot(context.Background(), f, "a")
			id := "existing"
			if kind == "share" {
				id = "publisher"
			}
			f.files[id] = pikpak.File{ID: id, ParentID: root, Name: "existing", Kind: "drive#file", Size: 42, Phase: "PHASE_TYPE_COMPLETE"}
			f.outputs = []RemoteEntry{{pikpak.File{Kind: "drive#file", Size: 42}, "existing"}}
			f.shareEntries = []Entry{{ID: id, Path: "existing", Name: "existing", Kind: "file", Size: 42}}
			w := &alteredDrive{fakeDrive: f}
			replace := func(r *pikpak.Transfer) { r.Files = nil; r.FileIDs = []string{id} }
			w.afterOffline = replace
			w.afterShare = replace
			a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
			j := createImport(t, a, kind)
			got := execute(t, a, &j)
			if got.State != "attention" {
				t.Fatal(got.State, got.Message)
			}
			if f.calls["move"] != 0 {
				t.Fatal("moved preexisting result")
			}
		})
	}
}
func TestLegacyTransferResumesAndEmptyStageCleanupIsScoped(t *testing.T) {
	a, f := testApp(t)
	root, _ := a.ensureRoot(context.Background(), f, "a")
	j := createImport(t, a, "magnet")
	d := loadJobData(t, j)
	name := ".vault-task-" + j.ID + "-" + d.SourceID[:8]
	stage, _ := f.Mkdir(context.Background(), root, name)
	f.outputs = sampleOutputs()
	f.putOutputs(stage.ID)
	d.Transfers[d.SourceID] = &TransferState{StageID: stage.ID, Phase: "submitted", Started: now()}
	a.Store.SaveJob(&j, &d)
	requireComplete(t, execute(t, a, &j))
	if f.calls["offline"] != 0 {
		t.Fatal("legacy checkpoint resubmitted")
	}
	unrelated, _ := f.Mkdir(context.Background(), root, ".vault-task-untracked")
	a.scheduleCleanup()
	jobs, _ := a.Store.Jobs()
	var clean Job
	for _, v := range jobs {
		if v.Kind == "cleanup" {
			clean = v
		}
	}
	if clean.ID == "" {
		t.Fatal("no cleanup scheduled")
	}
	requireComplete(t, execute(t, a, &clean))
	if !f.files[stage.ID].Trashed || f.files[unrelated.ID].Trashed {
		t.Fatal("incorrect cleanup scope")
	}
	calls := f.calls["trash"]
	a.Store.Set("cleanup_check:a", "0")
	a.scheduleCleanup()
	if f.calls["trash"] != calls {
		t.Fatal("cleanup repeated")
	}
}
func TestCleanupRetainsNonemptyRenamedReferencedOrIncompleteDirectories(t *testing.T) {
	for _, mode := range []string{"nonempty", "renamed", "referenced", "page-error"} {
		t.Run(mode, func(t *testing.T) {
			a, f := testApp(t)
			root, _ := a.ensureRoot(context.Background(), f, "a")
			j := createImport(t, a, "magnet")
			d := loadJobData(t, j)
			stage, _ := f.Mkdir(context.Background(), root, ".vault-task-"+j.ID+"-"+d.SourceID[:8])
			d.Transfers[d.SourceID] = &TransferState{StageID: stage.ID, Phase: "complete"}
			j.State = "completed"
			a.Store.SaveJob(&j, &d)
			switch mode {
			case "nonempty":
				f.Mkdir(context.Background(), stage.ID, "keep")
			case "renamed":
				f.Rename(context.Background(), stage.ID, "user folder")
			case "referenced":
				a.Store.NewJob("a", "recover", "pending", d)
			case "page-error":
				f.failPage = stage.ID
			}
			clean, _ := a.Store.NewJob("a", "cleanup", "clean", JobData{CleanupJobs: []string{j.ID}})
			execute(t, a, &clean)
			if f.files[stage.ID].Trashed || f.calls["trash"] != 0 {
				t.Fatal("deleted protected folder")
			}
		})
	}
}
func TestDirectInstantHitAvoidsStagingAndExtraWrites(t *testing.T) {
	a, f := testApp(t)
	f.outputs = []RemoteEntry{{pikpak.File{Kind: "drive#file", Size: 42, Hash: "hash"}, "hello"}}
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	nodes, _ := a.Store.AllNodes("a")
	delete(f.files, nodes[0].RemoteID)
	f.instantHit = true
	f.calls = map[string]int{}
	r, _ := a.Store.NewJob("a", "recover", "recover", JobData{})
	requireComplete(t, execute(t, a, &r))
	if f.calls["instant"] != 1 || f.calls["mkdir"] != 0 || f.calls["move"] != 0 || f.calls["rename"] != 0 || f.calls["offline"] != 0 {
		t.Fatal(f.calls)
	}
}
func TestWaitingTransferBackoffIsBounded(t *testing.T) {
	x := &TransferState{}
	for _, want := range []int64{15, 30, 60, 120, 120} {
		if got := x.wait("wait").(*pending).delay; got != want {
			t.Fatalf("%d != %d", got, want)
		}
	}
}

func TestVerificationAndRateLimitProtectOtherQueuedJobs(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(string(rune(status)), func(t *testing.T) {
			a, f := testApp(t)
			w := &alteredDrive{fakeDrive: f, offlineError: &pikpak.APIError{Status: status, Code: "rejected", RetryAfter: 180}}
			a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
			j := createImport(t, a, "magnet")
			other := createImport(t, a, "magnet")
			execute(t, a, &j)
			other, _ = a.Store.Job(other.ID)
			if status == 401 {
				ac, _ := a.Store.Account("a")
				if ac.Status != "verification_required" || j.State != "paused" || other.State != "paused" {
					t.Fatal(ac.Status, j.State, other.State)
				}
			} else if other.NextRun < now()+175 || j.NextRun < now()+175 {
				t.Fatal("cooldown not shared", j.NextRun, other.NextRun)
			}
		})
	}
}

func TestTombstonesRestoreRootAndFolderWithoutSourceReplay(t *testing.T) {
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	j := createImport(t, a, "magnet")
	requireComplete(t, execute(t, a, &j))
	f.tombstones = true
	ac, _ := a.Store.Account("a")
	for id, v := range f.files {
		if id == ac.RootID || v.Name == "Collection" {
			v.Trashed = true
			f.files[id] = v
		}
	}
	r, _ := a.Store.NewJob("a", "recover", "restore", JobData{})
	requireComplete(t, execute(t, a, &r))
	if f.calls["offline"] != 1 || f.calls["untrash"] != 2 {
		t.Fatal(f.calls)
	}
}

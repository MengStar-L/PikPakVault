package vault

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
)

func shareResultFixture(t *testing.T) (*App, *fakeDrive, *alteredDrive) {
	t.Helper()
	a, f := testApp(t)
	f.outputs = sampleOutputs()
	for i, r := range f.outputs {
		kind := "file"
		if r.File.Folder() {
			kind = "folder"
		}
		f.shareEntries = append(f.shareEntries, Entry{ID: fmt.Sprint("publisher-", i), Path: r.Path, Name: path.Base(r.Path), Kind: kind, Size: int64(r.File.Size), Hash: r.File.Hash})
	}
	w := &alteredDrive{fakeDrive: f, afterShare: func(r *pikpak.Transfer) { *r = pikpak.Transfer{} }}
	a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
	return a, f, w
}

func TestShareWithoutResultIDsOnlyReconcilesDestination(t *testing.T) {
	for _, location := range []string{"target", "root", "My Pack", "Pack From Shared", "new landing"} {
		t.Run(location, func(t *testing.T) {
			a, f, w := shareResultFixture(t)
			_, err := a.ensureRoot(context.Background(), f, "a")
			if err != nil {
				t.Fatal(err)
			}
			fallback := ""
			if location == "My Pack" || location == "Pack From Shared" {
				for _, v := range f.files {
					if v.Name == location {
						fallback = v.ID
					}
				}
				if fallback == "" {
					folder, _ := f.Mkdir(context.Background(), "", location)
					fallback = folder.ID
				}
			}
			w.afterShare = func(r *pikpak.Transfer) {
				if location == "new landing" {
					folder, _ := f.Mkdir(context.Background(), "", "Pack From Shared")
					fallback = folder.ID
				}
				if location != "target" {
					for _, v := range r.Files {
						f.Move(context.Background(), v.ID, fallback)
					}
				}
				*r = pikpak.Transfer{}
			}
			j := createImport(t, a, "share")
			result := execute(t, a, &j)
			if location != "target" {
				if result.State == "completed" || f.calls["share_restore"] != 1 || f.calls["move"] != 1 {
					t.Fatal(result.State, f.calls)
				}
				return
			}
			requireComplete(t, result)
			nodes, _ := a.Store.AllNodes("a")
			if len(nodes) != 4 || f.calls["share_restore"] != 1 {
				t.Fatal(len(nodes), f.calls)
			}
			d := loadJobData(t, j)
			if len(d.Transfers[d.SourceID].OutputIDs) != 1 || !strings.Contains(j.Message, "未重复转存") {
				t.Fatal(j.Message)
			}
			for _, n := range nodes {
				if n.State != "present" {
					t.Fatal(n)
				}
			}
		})
	}
}

func TestShareRetryRechecksLegacyCheckpointWithoutResubmission(t *testing.T) {
	a, f, _ := shareResultFixture(t)
	sessionForTest(t, a)
	root, _ := a.ensureRoot(context.Background(), f, "a")
	j := createImport(t, a, "share")
	d := loadJobData(t, j)
	d.Transfers[d.SourceID] = &TransferState{Mode: "direct", TargetID: root, Phase: "submitted", Started: now() - 500, Expected: f.shareEntries, Polls: 7}
	j.State = "attention"
	a.Store.SaveJob(&j, &d)
	// File visibility can arrive after the earlier request returned no IDs.
	f.putOutputs(root)
	r := request(t, a.Handler(nil), "POST", "/api/v1/jobs/"+j.ID+"/retry", map[string]any{}, "test-csrf")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "重新核对") {
		t.Fatal(r.Code, r.Body.String())
	}
	requireComplete(t, execute(t, a, &j))
	if f.calls["share_restore"] != 0 {
		t.Fatal("legacy retry submitted another transfer")
	}
	jobs, _ := a.Store.Jobs()
	if len(jobs) != 1 {
		t.Fatal("created another job")
	}
}

func TestShareReconciliationRejectsUncertainResults(t *testing.T) {
	for _, mode := range []string{"preexisting", "duplicate", "wrong-hash", "missing-hash", "source-hash-missing", "empty", "incomplete", "page-error", "claimed", "other-task", "publisher-id"} {
		t.Run(mode, func(t *testing.T) {
			a, f, w := shareResultFixture(t)
			root, _ := a.ensureRoot(context.Background(), f, "a")
			if mode == "preexisting" {
				f.putOutputs(root)
			}
			if mode == "source-hash-missing" {
				for i := range f.shareEntries {
					f.shareEntries[i].Hash = ""
				}
			}
			w.afterShare = func(r *pikpak.Transfer) {
				id := r.Files[0].ID
				switch mode {
				case "duplicate":
					f.putOutputs(root)
				case "preexisting", "empty":
					for key, v := range f.files {
						if v.ID == id || v.ParentID == id {
							delete(f.files, key)
						}
					}
				case "wrong-hash", "missing-hash", "incomplete":
					for key, v := range f.files {
						if !v.Folder() {
							if mode == "wrong-hash" {
								v.Hash = "different"
							}
							if mode == "missing-hash" {
								v.Hash = ""
							}
							if mode == "incomplete" {
								v.Phase = "PHASE_TYPE_RUNNING"
							}
							f.files[key] = v
						}
					}
				case "page-error":
					f.failPage = id
				case "claimed":
					a.Store.Bind("a", "root", id, "present", "Collection", root, "")
				case "other-task":
					a.Store.NewJob("a", "import", "other", JobData{Transfers: map[string]*TransferState{"other": {OutputIDs: []string{id}}}})
				case "publisher-id":
					v := f.files[id]
					delete(f.files, id)
					v.ID = f.shareEntries[0].ID
					f.files[v.ID] = v
					for key, child := range f.files {
						if child.ParentID == id {
							child.ParentID = v.ID
							f.files[key] = child
						}
					}
				}
				*r = pikpak.Transfer{}
			}
			j := createImport(t, a, "share")
			execute(t, a, &j)
			d := loadJobData(t, j)
			d.Transfers[d.SourceID].Started = now() - 500
			a.Store.SaveJob(&j, &d)
			execute(t, a, &j)
			if j.State == "completed" || f.calls["share_restore"] != 1 {
				t.Fatal(j.State, f.calls)
			}
			nodes, _ := a.Store.AllNodes("a")
			if len(nodes) != 0 {
				t.Fatal("adopted uncertain result")
			}
		})
	}
}

func TestShareFallbackNeverAdoptsPreexistingOrUnrecordedLanding(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		a, f, _ := shareResultFixture(t)
		root, _ := a.ensureRoot(context.Background(), f, "a")
		landing, _ := f.Mkdir(context.Background(), "", "Pack From Shared")
		f.putOutputs(landing.ID)
		j := createImport(t, a, "share")
		d := loadJobData(t, j)
		x := &TransferState{Mode: "direct", TargetID: root, Started: now() - 500, Phase: "submitted", Expected: f.shareEntries}
		x.StrictTarget = !legacy
		d.Transfers[d.SourceID] = x
		a.Store.SaveJob(&j, &d)
		execute(t, a, &j)
		if j.State != "attention" || f.calls["share_restore"] != 0 {
			t.Fatal(j.State, j.Message)
		}
	}
}

func TestRetryReportsAccountNotReady(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	j := createImport(t, a, "magnet")
	a.Store.DB.Exec(`UPDATE accounts SET status='verification_required' WHERE id='a'`)
	r := request(t, a.Handler(nil), "POST", "/api/v1/jobs/"+j.ID+"/retry", map[string]any{}, "test-csrf")
	if r.Code != 409 || !strings.Contains(r.Body.String(), "完成验证") {
		t.Fatal(r.Code, r.Body.String())
	}
}

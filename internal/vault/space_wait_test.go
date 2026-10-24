package vault

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pikpakvault/internal/aria2"
	"pikpakvault/internal/pikpak"
	"pikpakvault/internal/teldrive"
)

func TestDownloadSpaceWaitPersistsAcrossRestartWithoutRemoteRequests(t *testing.T) {
	for _, kind := range []string{"teldrive_upload", "recover"} {
		t.Run(kind, func(t *testing.T) {
			a, _ := testApp(t)
			n := Node{ID: "huge-file", ParentID: "root", Name: "large.mp4", Kind: "file", Size: 1 << 62}
			if err := a.Store.InsertNode(n); err != nil {
				t.Fatal(err)
			}
			d := JobData{NodeIDs: []string{n.ID}, RecoveryLane: telDriveLane, Uploads: map[string]*TelDriveUpload{n.ID: {GCID: "preserved"}}, Done: map[string]bool{"previous-file": true}}
			j, err := a.Store.NewJob("a", kind, "space wait", d)
			if err != nil {
				t.Fatal(err)
			}
			j.Progress, j.Attempts = 43, 6
			dir := a.telDriveCacheDir(j.ID, n.ID)
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "content"), []byte("resume-me"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = a.downloadTelDrive(context.Background(), &j, &d, n, nil, teldrive.File{ID: "remote-source", Size: n.Size})
			var p *pending
			if !errors.As(err, &p) || p.delay != 60 || d.WaitReason != "disk_space" || d.WaitSpace == nil {
				t.Fatal(err, d.WaitReason)
			}
			j.State, j.NextRun = "waiting", now()-1
			if err = a.Store.SaveJob(&j, &d); err != nil {
				t.Fatal(err)
			}
			if j.WaitReason != "disk_space" {
				t.Fatal("wait reason not exposed")
			}
			// Reopen the actual database, rather than relying on an in-memory flag.
			storeDir := a.Store.Dir
			if err = a.Store.DB.Close(); err != nil {
				t.Fatal(err)
			}
			a.Store, err = Open(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Store.DB.Close() })
			calls := 0
			a.Factory = func(Account) (pikpak.Provider, error) {
				calls++
				return nil, errors.New("must not contact upstream while waiting")
			}
			for i := 0; i < 2; i++ {
				a.Execute(context.Background(), &j)
				j, err = a.Store.Job(j.ID)
				if err != nil || j.State != "waiting" || j.WaitReason != "disk_space" || j.Attempts != 6 || j.Progress != 43 || j.NextRun < now()+50 {
					t.Fatal(j, err)
				}
			}
			if calls != 0 {
				t.Fatal("space retry touched upstream", calls)
			}
			var saved JobData
			if err = json.Unmarshal(j.Data, &saved); err != nil || !saved.Done["previous-file"] || saved.Uploads[n.ID].GCID != "preserved" {
				t.Fatal("checkpoint changed", err)
			}
			body, err := os.ReadFile(filepath.Join(dir, "content"))
			if err != nil || string(body) != "resume-me" {
				t.Fatal("partial cache changed", err)
			}
			j.State = "paused"
			a.Store.SaveJob(&j, nil)
			a.Execute(context.Background(), &j)
			if j.State != "paused" || calls != 0 || j.WaitReason != "" {
				t.Fatal("paused task resumed", j)
			}
			j.State, j.NextRun = "waiting", now()-1
			a.Store.SaveJob(&j, nil)
			addAccount(t, a, "b", "user-b")
			a.Store.Set("active_account", "b")
			a.Execute(context.WithValue(context.Background(), scheduledExecutionKey{}, true), &j)
			if calls != 0 || j.AccountID != "a" || j.State != "waiting" {
				t.Fatal("account switch resumed old work", j)
			}
		})
	}
}

func TestSpaceWaitAutomaticallyContinuesOriginalUploadAndRecovery(t *testing.T) {
	for _, kind := range []string{"teldrive_upload", "recover"} {
		t.Run(kind, func(t *testing.T) {
			a, remote := testApp(t)
			td := telDriveFixture(t)
			drive := &uploadFake{fakeDrive: remote}
			a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
			m := addMonitor(t, a, td)
			runTDScan(t, a, m)
			j := tdUploadJob(t, a, "a")
			n, err := a.Store.Node(stableNode(m.ID, "movie"), "a")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "recover" {
				j.State = "cancelled"
				a.Store.SaveJob(&j, nil)
				j, err = a.Store.NewJob("a", kind, "recover after disk release", JobData{})
				if err != nil {
					t.Fatal(err)
				}
			}
			d := JobData{NodeIDs: []string{n.ID}, RecoveryLane: telDriveLane, ExactNodes: true, Uploads: map[string]*TelDriveUpload{n.ID: {}}}
			markSpaceWait(&d, n.ID, n.Size, &aria2.InsufficientSpaceError{Needed: uint64(n.Size) + 64<<20})
			j.State, j.NextRun = "waiting", now()-1
			if err = a.Store.SaveJob(&j, &d); err != nil {
				t.Fatal(err)
			}
			id := j.ID
			a.Execute(context.WithValue(context.Background(), scheduledExecutionKey{}, true), &j)
			j, err = a.Store.Job(id)
			if err != nil || j.ID != id || j.State != "completed" || j.WaitReason != "" || drive.begins != 1 || drive.sends != 1 || drive.data != td.body {
				t.Fatal(j, err, drive.begins, drive.sends)
			}
			var saved JobData
			if err = json.Unmarshal(j.Data, &saved); err != nil || saved.WaitReason != "" || saved.WaitSpace != nil {
				t.Fatal("stale wait checkpoint", err)
			}
			if _, err = os.Stat(a.telDriveCacheDir(id, n.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed cache retained", err)
			}
		})
	}
}

func TestSpacePreflightLetsAnotherCachedRecoveryNodeFinishFirst(t *testing.T) {
	a, _ := testApp(t)
	for _, n := range []Node{{ID: "large", ParentID: "root", Name: "large", Kind: "file", Size: 1 << 62}, {ID: "uploaded", ParentID: "root", Name: "uploaded", Kind: "file", Size: 1}} {
		if err := a.Store.InsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	j := Job{ID: "job", AccountID: "a", Kind: "recover"}
	d := JobData{RecoveryLane: telDriveLane, NodeIDs: []string{"large", "uploaded"}, WaitReason: "disk_space", WaitSpace: &diskSpaceWait{NodeID: "large", Size: 1 << 62}, Uploads: map[string]*TelDriveUpload{"uploaded": {RemoteID: "owned-result"}}}
	if err := a.recheckDownloadSpace(&j, &d); err != nil || d.WaitReason != "" {
		t.Fatal("cached recovery blocked by another node's disk wait", err)
	}
}

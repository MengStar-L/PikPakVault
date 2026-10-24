package vault

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pikpakvault/internal/aria2"
	"pikpakvault/internal/pikpak"
	"pikpakvault/internal/teldrive"
)

func TestRecoverySpaceWaitFinishesCachedNodeBeforeFreshDownload(t *testing.T) {
	a, remote := testApp(t)
	var low *aria2.InsufficientSpaceError
	if err := aria2.CheckSpace(aria2.ProgramDir(), 1<<62); !errors.As(err, &low) {
		t.Fatal("cannot read test disk budget", err)
	}
	largeSize := int64(low.Available + (1 << 30))
	if largeSize > int64(64<<20)*10000 {
		t.Skip("filesystem free space exceeds the upload limit")
	}
	td := telDriveFixture(t)
	largeFile := td.files["movie"]
	largeFile.Size = largeSize
	td.files["movie"] = largeFile
	td.files["cached"] = teldrive.File{ID: "cached", Name: "cached.mp4", Type: "file", ParentID: "sub", Size: int64(len(td.body)), Hash: "blake3-cached", Updated: largeFile.Updated, Mime: "video/mp4"}
	drive := &uploadFake{fakeDrive: remote}
	a.Factory = func(Account) (pikpak.Provider, error) { return drive, nil }
	m := addMonitor(t, a, td)
	requireComplete(t, runTDScan(t, a, m))
	if _, err := a.Store.DB.Exec(`UPDATE jobs SET state='cancelled' WHERE kind='teldrive_upload'`); err != nil {
		t.Fatal(err)
	}
	largeID, cachedID := stableNode(m.ID, "movie"), stableNode(m.ID, "cached")
	cached, err := a.Store.Node(cachedID, "a")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := pikpak.GCID(strings.NewReader(td.body), cached.Size)
	if err != nil {
		t.Fatal(err)
	}
	remote.files["saved-result"] = pikpak.File{ID: "saved-result", ParentID: "old-position", Name: cached.Name, Kind: "drive#file", Size: pikpak.Number(cached.Size), Hash: hash, MimeType: cached.Mime, Phase: "PHASE_TYPE_COMPLETE"}
	d := JobData{RecoveryLane: telDriveLane, ExactNodes: true, NodeIDs: []string{largeID, cachedID}, Uploads: map[string]*TelDriveUpload{cachedID: {RemoteID: "saved-result", GCID: hash}}}
	markSpaceWait(&d, largeID, largeSize, low)
	j, err := a.Store.NewJob("a", "recover", "cached first", d)
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.NextRun = "waiting", now()-1
	if err = a.Store.SaveJob(&j, &d); err != nil {
		t.Fatal(err)
	}
	dir := a.telDriveCacheDir(j.ID, cachedID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"content":     []byte(td.body),
		"owner.json":  []byte(jsonText(cacheOwner{JobID: j.ID, NodeID: cachedID})),
		"source.json": []byte(jsonText(map[string]any{"Complete": true, "Size": cached.Size})),
	} {
		if err = os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	a.Execute(context.WithValue(context.Background(), scheduledExecutionKey{}, true), &j)
	j, err = a.Store.Job(j.ID)
	if err != nil || j.State != "waiting" || j.WaitReason != "disk_space" {
		t.Fatal("fresh file did not wait", j, err)
	}
	var saved JobData
	if err = json.Unmarshal(j.Data, &saved); err != nil || !saved.Done[cachedID] || saved.Done[largeID] || saved.WaitSpace == nil || saved.WaitSpace.NodeID != largeID {
		t.Fatal("cached result not completed ahead of disk wait", err, saved.Done)
	}
	if _, err = os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verified cache not released", err)
	}
	if drive.begins != 0 || drive.sends != 0 || td.downloads.Load() != 0 {
		t.Fatal("duplicated existing result or started a download without space")
	}
}

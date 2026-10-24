package vault

import (
	"encoding/json"
	"testing"
)

func TestSpaceWaitFileProjectionMatchesTaskState(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	if err := a.Store.InsertNode(Node{ID: "space-node", ParentID: "root", Name: "waiting.mp4", Kind: "file", Size: 42}); err != nil {
		t.Fatal(err)
	}
	d := JobData{NodeIDs: []string{"space-node"}, WaitReason: "disk_space", WaitSpace: &diskSpaceWait{NodeID: "space-node", Size: 42}}
	j, err := a.Store.NewJob("a", "teldrive_upload", "waiting", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"waiting", "paused"} {
		j.State = state
		if err = a.Store.SaveJob(&j, &d); err != nil {
			t.Fatal(err)
		}
		w := request(t, a.Handler(nil), "GET", "/api/v1/files?parent=root", nil, "")
		var result struct {
			Files []Node `json:"files"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		found := false
		for _, n := range result.Files {
			if n.ID != "space-node" {
				continue
			}
			found = true
			want := ""
			if state == "waiting" {
				want = "disk_space"
			}
			if n.Transfer == nil || n.Transfer.State != state || n.Transfer.WaitReason != want {
				t.Fatalf("file and task disagree: %+v", n.Transfer)
			}
		}
		if !found {
			t.Fatal("waiting file disappeared")
		}
	}
}

package pikpak

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestShareExplicitDestinationReceiptAndTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/drive/v1/share/restore" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			want := map[string]any{"share_id": "share", "pass_code_token": "pass", "parent_id": "chosen-folder", "specify_parent_id": true, "ancestor_ids": []any{}, "file_ids": []any{"source1", "source2"}, "params": map[string]any{"trace_file_ids": "source1,source2"}}
			if !reflect.DeepEqual(body, want) {
				t.Errorf("unexpected restore payload: %#v", body)
			}
			w.Write([]byte(`{"file_id":"chosen-folder","restore_task_id":"receipt-task","restore_status":"PENDING","params":{"trace_file_ids":"source1,source2"}}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/drive/v1/tasks/receipt-task" {
			w.Write([]byte(`{"id":"receipt-task","file_id":"chosen-folder","phase":"PHASE_TYPE_COMPLETE","params":{"trace_file_ids":"{\"source1\":\"copy1\",\"source2\":\"copy2\"}"}}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "token", CaptchaToken: "captcha"})
	c.DriveURL = server.URL
	c.RequestInterval, c.WriteInterval = 0, 0
	r, err := c.RestoreShare(context.Background(), "share", "pass", []string{"source1", "source2"}, "chosen-folder")
	if err != nil || r.RestoreParentID != "chosen-folder" || r.RestoreTaskID != "receipt-task" || r.File != nil || len(r.FileIDs) != 0 {
		t.Fatal(r, err)
	}
	task, err := c.Task(context.Background(), r.RestoreTaskID)
	if err != nil || task.ID != r.RestoreTaskID || len(task.Params.TraceFileIDs) == 0 {
		t.Fatal(task, err)
	}
}

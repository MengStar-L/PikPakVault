package vault

import (
	"context"
	"encoding/json"
	"testing"

	"pikpakvault/internal/pikpak"
)

func TestShareTraceValidation(t *testing.T) {
	for _, raw := range []string{`{"source":"copy"}`, `"{\"source\":\"copy\"}"`} {
		x := TransferState{TargetID: "parent", Expected: []Entry{{ID: "source", Path: "file"}}, ShareTrace: json.RawMessage(raw)}
		if err := x.collectShareTrace(); err != nil || len(x.OutputIDs) != 1 || x.OutputIDs[0] != "copy" {
			t.Fatal(x, err)
		}
	}
	for _, raw := range []string{`{"other":"copy"}`, `{"source":"parent"}`, `{"source":"source"}`, `{"source":""}`, `{"source":17}`, `{"source":"copy","extra":"two"}`} {
		x := TransferState{TargetID: "parent", Expected: []Entry{{ID: "source", Path: "file"}}, ShareTrace: json.RawMessage(raw)}
		if x.collectShareTrace() == nil || len(x.OutputIDs) != 0 {
			t.Fatal("accepted invalid map", raw)
		}
	}
	x := TransferState{Expected: []Entry{{ID: "source", Path: "file"}}, ShareTrace: json.RawMessage(`"source"`)}
	if err := x.collectShareTrace(); err != nil || len(x.OutputIDs) != 0 {
		t.Fatal(err)
	}
}

type shareReceiptDrive struct {
	*alteredDrive
	task  pikpak.Task
	reads int
}

func (c *shareReceiptDrive) Task(_ context.Context, id string) (pikpak.Task, error) {
	c.reads++
	return c.task, nil
}

func TestShareReceiptRegistersOnlyMappedOutputsWithoutMoves(t *testing.T) {
	for _, mode := range []string{"immediate", "task", "stalled", "failed", "wrong-parent", "wrong-result-parent"} {
		t.Run(mode, func(t *testing.T) {
			a, f, w := shareResultFixture(t)
			root, _ := a.ensureRoot(context.Background(), f, "a")
			f.calls = map[string]int{}
			reader := &shareReceiptDrive{alteredDrive: w}
			a.Factory = func(Account) (pikpak.Provider, error) { return reader, nil }
			w.afterShare = func(r *pikpak.Transfer) {
				trace, _ := json.Marshal(map[string]string{f.shareEntries[0].ID: r.Files[0].ID})
				if mode == "wrong-result-parent" {
					v := f.files[r.Files[0].ID]
					v.ParentID = "elsewhere"
					f.files[v.ID] = v
				}
				*r = pikpak.Transfer{RestoreParentID: root}
				if mode == "wrong-parent" {
					r.RestoreParentID = "Pack From Shared"
				}
				if mode == "immediate" || mode == "wrong-result-parent" {
					r.Params.TraceFileIDs = trace
					return
				}
				r.RestoreTaskID = "receipt"
				reader.task = pikpak.Task{ID: "receipt", FileID: root, Phase: "PHASE_TYPE_COMPLETE", Params: pikpak.TaskParams{TraceFileIDs: trace}}
				if mode == "stalled" {
					reader.task.Phase = "PHASE_TYPE_RUNNING"
				}
				if mode == "failed" {
					reader.task.Phase = "PHASE_TYPE_ERROR"
					reader.task.Params.TraceFileIDs = nil
					f.files = map[string]pikpak.File{root: f.files[root]}
				}
			}
			j := createImport(t, a, "share")
			result := execute(t, a, &j)
			if mode == "wrong-parent" || mode == "wrong-result-parent" || mode == "failed" {
				if result.State != "attention" {
					t.Fatal(result.State, result.Message)
				}
				execute(t, a, &j)
			} else {
				if mode == "stalled" {
					if result.State != "waiting" {
						t.Fatal(result.State)
					}
					ageTransfer(t, a, &j)
					result = execute(t, a, &j)
				}
				requireComplete(t, result)
				nodes, _ := a.Store.AllNodes("a")
				if len(nodes) != 4 {
					t.Fatal("adopted landing parent", len(nodes))
				}
			}
			if f.calls["share_restore"] != 1 || f.calls["move"] != 0 || f.calls["mkdir"] != 0 {
				t.Fatal(f.calls)
			}
			if mode == "task" && reader.reads != 1 {
				t.Fatal("did not use receipt task endpoint", reader.reads)
			}
		})
	}
}

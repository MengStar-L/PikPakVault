package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"path"
	"sort"
	"strings"

	"pikpakvault/internal/pikpak"
)

// New imports go straight to the final parent. StageID remains authoritative for
// older checkpoints; upgrading must never submit those transfers a second time.
func (a *App) materialize(ctx context.Context, c pikpak.Provider, j *Job, d *JobData, s Source) ([]RemoteEntry, error) {
	t := d.Transfers[s.ID]
	if t == nil {
		t = &TransferState{}
		d.Transfers[s.ID] = t
	}
	if t.Phase == "complete" {
		return t.Entries, nil
	}
	if t.StageID == "" && t.TargetID == "" {
		if j.Kind == "import" {
			parent, err := a.folder(ctx, c, j.AccountID, d.ParentID, map[string]bool{})
			if err != nil {
				return nil, err
			}
			t.Mode, t.TargetID = "direct", parent
		} else {
			// Only source-batch recovery needs isolation: saved paths may now differ
			// and a batch can include files the user did not request to recover.
			parent, err := a.stage(ctx, c, j.AccountID, "恢复暂存")
			if err != nil {
				return nil, err
			}
			t.StageName, t.StageParent = "恢复-"+j.ID[:8]+"-"+s.ID[:8], parent
			id, err := namedStage(ctx, c, parent, t.StageName)
			if err != nil {
				return nil, err
			}
			t.StageID = id
		}
		if err := a.checkpoint(j, d); err != nil {
			return nil, err
		}
	}
	parent := t.TargetID
	if t.StageID != "" {
		parent = t.StageID
	}
	if t.Phase == "" {
		var pass string
		if err := a.Store.Unseal(s.Secret, &pass); err != nil {
			return nil, err
		}
		var shareToken string
		shareIDs := []string{}
		if s.Kind == "share" {
			entries, token, err := shareTree(ctx, c, s, pass)
			if err != nil {
				return nil, err
			}
			if len(entries) == 0 {
				return nil, block("No files selected in share")
			}
			shareToken, t.Expected = token, entries
			for _, entry := range entries {
				if !strings.Contains(entry.Path, "/") {
					shareIDs = append(shareIDs, entry.ID)
				}
			}
			if len(s.Manifest) == 0 {
				s.Manifest = entries
				if err = a.Store.SaveSource(s); err != nil {
					return nil, err
				}
			}
		} else {
			t.Expected = s.Manifest
		}
		if t.Mode == "direct" {
			files, err := listAll(ctx, c, parent)
			if err != nil {
				return nil, err
			}
			t.BeforeIDs = nil
			for _, f := range files {
				t.BeforeIDs = append(t.BeforeIDs, f.ID)
			}
			if s.Kind == "magnet" {
				tasks, err := c.Tasks(ctx)
				if err != nil {
					return nil, err
				}
				t.BeforeTasks = nil
				for _, task := range tasks {
					t.BeforeTasks = append(t.BeforeTasks, task.ID)
				}
			}
		}
		t.Phase, t.Started = "dispatching", now()
		if err := a.checkpoint(j, d); err != nil {
			return nil, err
		}
		var result pikpak.Transfer
		var err error
		if s.Kind == "magnet" {
			result, err = c.Offline(ctx, s.Link, parent)
		} else {
			result, err = c.RestoreShare(ctx, s.ShareID, shareToken, shareIDs, parent)
		}
		if err != nil {
			var up *pikpak.APIError
			if errors.As(err, &up) && up.Status >= 400 && up.Status < 500 {
				t.Phase = ""
			}
			return nil, err // ambiguous writes are reconciled, never blindly repeated
		}
		if result.Task != nil {
			t.TaskID = result.Task.ID
			t.addOutput(result.Task.FileID)
		}
		if result.TaskID != "" {
			t.TaskID = result.TaskID
		}
		if result.File != nil {
			t.addOutput(result.File.ID)
			t.rememberDisplay(*result.File)
		}
		for _, f := range result.Files {
			t.addOutput(f.ID)
			t.rememberDisplay(f)
		}
		for _, id := range result.FileIDs {
			t.addOutput(id)
		}
		t.Phase = "submitted"
		if err = a.checkpoint(j, d); err != nil {
			return nil, err
		}
	}

	taskPending, taskFailed := false, false
	var taskErr error
	if t.TaskID != "" || (s.Kind == "magnet" && t.Mode == "direct" && len(t.OutputIDs) == 0) {
		tasks, err := c.Tasks(ctx)
		if err != nil {
			// A task-list outage is not evidence that the saved file is unavailable.
			if !pikpak.Temporary(err) || len(t.OutputIDs) == 0 {
				return nil, err
			}
			taskErr, taskPending = err, true
		} else {
			if t.TaskID == "" {
				candidates := []pikpak.Task{}
				for _, task := range tasks {
					if !contains(t.BeforeTasks, task.ID) && sameMagnet(s.Link, task.Params.URL) && task.FileID != "" {
						f, err := c.Get(ctx, task.FileID)
						if err != nil {
							return nil, err
						}
						if f.ParentID == parent && !contains(t.BeforeIDs, f.ID) {
							candidates = append(candidates, task)
						}
					}
				}
				if len(candidates) > 1 {
					return nil, block("多个新任务对应相同来源，无法唯一确认结果。请在任务详情中关联文件；不会重复转存。")
				}
				if len(candidates) == 1 {
					t.TaskID = candidates[0].ID
					t.Phase = "submitted"
				}
			}
			found := false
			for _, task := range tasks {
				if task.ID != t.TaskID {
					continue
				}
				found = true
				t.addOutput(task.FileID) // collect IDs even when upstream progress is stuck
				taskPending = task.Phase != "PHASE_TYPE_COMPLETE"
				taskFailed = task.Phase == "PHASE_TYPE_ERROR"
				j.Progress = max(j.Progress, min(task.Progress, 95))
			}
			if t.TaskID != "" && !found {
				taskPending = true
			}
		}
	}
	files := []pikpak.File{}
	// Legacy/isolated directories are owned by this transfer; direct destinations
	// are shared with other operations and must never be consumed wholesale.
	if t.StageID != "" {
		var err error
		files, err = listAll(ctx, c, t.StageID)
		if err != nil {
			return nil, err
		}
	}
	for _, id := range t.OutputIDs {
		if contains(t.BeforeIDs, id) {
			return nil, block("返回对象在转存前已经存在，已暂停以避免接管原文件")
		}
		for _, entry := range append(append([]Entry{}, t.Expected...), s.Manifest...) {
			if s.Kind == "share" && entry.ID == id {
				return nil, block("Share response returned source IDs; saved result needs manual association")
			}
		}
		found := false
		for _, f := range files {
			if f.ID == id {
				found = true
				break
			}
		}
		if found {
			continue
		}
		f, err := c.Get(ctx, id)
		if pikpak.Missing(err) {
			return nil, t.wait("等待 PikPak 的文件结果可见；不会重复提交")
		}
		if err != nil {
			return nil, err
		}
		if f.Trashed {
			return nil, block("Transferred output was deleted before registration")
		}
		files = append(files, f) // align directly later; never move into an intermediate folder
		t.rememberDisplay(f)
	}
	if len(files) == 0 {
		if taskErr != nil {
			return nil, taskErr
		}
		if taskFailed {
			return nil, block("PikPak 任务失败，尚无可核验文件，请检查来源")
		}
		if now()-t.Started < 120 {
			return nil, t.wait("等待可确认归属的转存结果；不会重复提交")
		}
		return nil, block("PikPak returned no attributable saved files. Associate the saved result IDs in task details; the request will not be repeated automatically.")
	}
	entries, err := transferTree(ctx, c, files)
	if err != nil {
		t.Fingerprint, t.StableSince = "", 0
		var p *pending
		if errors.As(err, &p) {
			if taskFailed {
				return nil, block("PikPak 任务失败，结果文件仍不完整，请检查来源")
			}
			return nil, t.wait(p.message)
		}
		return nil, err
	}
	expected := t.Expected
	if len(expected) == 0 {
		expected = s.Manifest
	}
	if len(expected) > 0 && !matchesManifest(entries, expected) {
		t.Fingerprint, t.StableSince = "", 0
		if taskFailed {
			return nil, block("PikPak 任务失败，已保存文件与来源清单不一致")
		}
		return nil, t.wait("已找到结果，正在等待全部文件与来源清单一致")
	}
	if taskPending {
		// Do not equate a visible folder or 99% progress with success. A stalled
		// task needs two complete, identical tree reads at least 30 seconds apart.
		leaves := 0
		for _, entry := range entries {
			if !entry.File.Folder() {
				leaves++
			}
		}
		if leaves == 0 {
			return nil, t.wait("云端任务尚未结束，结果目录中还没有完整文件")
		}
		fingerprint := treeFingerprint(entries)
		if t.Fingerprint != fingerprint || t.StableSince == 0 {
			t.Fingerprint, t.StableSince = fingerprint, now()
			return nil, t.wait("文件已就绪，正在复核目录完整性；无需重新转存")
		}
		if now()-t.StableSince < 30 {
			return nil, t.wait("文件已就绪，等待第二次完整性复核")
		}
		t.VerifiedByFiles = true
		d.Note = "已按实际文件完成核验并登记，PikPak 的任务进度尚未更新；未重复转存。"
	}
	t.Entries, t.Phase = entries, "complete"
	if err = a.checkpoint(j, d); err != nil {
		return nil, err
	}
	return entries, nil
}

func (t *TransferState) addOutput(id string) {
	if id != "" && !contains(t.OutputIDs, id) {
		t.OutputIDs = append(t.OutputIDs, id)
	}
}

// Reuse metadata from requests already needed for transfer verification. Showing
// in-progress files must not cause any extra cloud requests or folder mutations.
func (t *TransferState) rememberDisplay(f pikpak.File) {
	if f.ID == "" || f.Name == "" || (f.Kind != "drive#file" && !f.Folder()) {
		return
	}
	kind := "file"
	if f.Folder() {
		kind = "folder"
	}
	entry := Entry{ID: f.ID, Path: f.Name, Name: f.Name, Kind: kind, Size: int64(f.Size)}
	for i := range t.Display {
		if t.Display[i].ID == f.ID {
			t.Display[i] = entry
			return
		}
	}
	t.Display = append(t.Display, entry)
}
func (t *TransferState) wait(message string) error {
	delay := int64(15 << min(t.Polls, 3))
	t.Polls++
	return &pending{message, delay}
}
func sameMagnet(a, b string) bool {
	one, e1 := url.Parse(a)
	two, e2 := url.Parse(b)
	if e1 != nil || e2 != nil || !strings.EqualFold(one.Scheme, "magnet") || !strings.EqualFold(two.Scheme, "magnet") {
		return false
	}
	for _, x := range one.Query()["xt"] {
		for _, y := range two.Query()["xt"] {
			if x != "" && strings.EqualFold(x, y) {
				return true
			}
		}
	}
	return false
}
func namedStage(ctx context.Context, c pikpak.Provider, parent, name string) (string, error) {
	files, err := listAll(ctx, c, parent)
	if err != nil {
		return "", err
	}
	id := ""
	for _, f := range files {
		if f.Name != name {
			continue
		}
		if id != "" || !f.Folder() {
			return "", block("恢复暂存位置存在冲突")
		}
		id = f.ID
	}
	if id != "" {
		return id, nil
	}
	f, err := c.Mkdir(ctx, parent, name)
	return f.ID, err
}
func transferTree(ctx context.Context, c pikpak.Provider, files []pikpak.File) ([]RemoteEntry, error) {
	entries := []RemoteEntry{}
	visited, paths := map[string]bool{}, map[string]bool{}
	// Some APIs return both the batch root and its direct children.
	topIDs := map[string]bool{}
	for _, f := range files {
		topIDs[f.ID] = true
	}
	roots := []pikpak.File{}
	for _, f := range files {
		if !topIDs[f.ParentID] {
			roots = append(roots, f)
		}
	}
	var walk func([]pikpak.File, string) (int64, error)
	walk = func(files []pikpak.File, prefix string) (int64, error) {
		var bytes int64
		for _, f := range files {
			if f.ID == "" || visited[f.ID] {
				return 0, block("Duplicate remote object in transfer tree")
			}
			visited[f.ID] = true
			if f.Trashed || !f.Complete() {
				return 0, wait("正在核对实际文件：部分文件尚未完整保存")
			}
			if err := ValidName(f.Name); err != nil {
				return 0, err
			}
			p := path.Join(prefix, f.Name)
			if paths[p] {
				return 0, block("转存结果包含同名路径，需要人工处理")
			}
			paths[p] = true
			entries = append(entries, RemoteEntry{f, p})
			if f.Folder() {
				children, err := listAll(ctx, c, f.ID)
				if err != nil {
					return 0, err
				}
				for _, child := range children {
					if child.ParentID != f.ID {
						return 0, block("转存结果目录归属不一致")
					}
				}
				size, err := walk(children, p)
				if err != nil {
					return 0, err
				}
				if f.Size > 0 && size != int64(f.Size) {
					return 0, wait("结果目录大小与子文件合计不一致，等待完整保存")
				}
				bytes += size
			} else {
				bytes += int64(f.Size)
			}
		}
		return bytes, nil
	}
	_, err := walk(roots, "")
	return entries, err
}
func matchesManifest(entries []RemoteEntry, expected []Entry) bool {
	if len(entries) != len(expected) {
		return false
	}
	byPath := map[string]pikpak.File{}
	for _, entry := range entries {
		byPath[entry.Path] = entry.File
	}
	for _, entry := range expected {
		f, ok := byPath[entry.Path]
		if !ok || !compatible(Node{Kind: entry.Kind, Size: entry.Size, Hash: entry.Hash}, f) {
			return false
		}
	}
	return true
}
func treeFingerprint(entries []RemoteEntry) string {
	lines := []string{}
	for _, entry := range entries {
		f := entry.File
		lines = append(lines, jsonText([]any{entry.Path, f.ID, f.ParentID, f.Kind, f.Size, f.Hash, f.Phase}))
	}
	sort.Strings(lines)
	hash := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(hash[:])
}

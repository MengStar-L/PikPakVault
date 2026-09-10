package vault

import (
	"context"
	"encoding/json"
	"strings"

	"pikpakvault/internal/pikpak"
)

// Share tasks are often absent from the offline-task listing. Read the receipt's
// task directly; older providers can still supply a task listing.
func transferTasks(ctx context.Context, c pikpak.Provider, kind, id string) ([]pikpak.Task, error) {
	if reader, ok := c.(pikpak.TaskReader); ok && kind == "share" && id != "" {
		task, err := reader.Task(ctx, id)
		if pikpak.Missing(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if task.ID != id {
			return nil, block("PikPak 返回的分享任务 ID 不一致，请核对原任务")
		}
		return []pikpak.Task{task}, nil
	}
	return c.Tasks(ctx)
}

func (t *TransferState) collectShareTrace() error {
	raw := t.ShareTrace
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return nil
	}
	if raw[0] == '"' {
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil {
			return block("分享结果映射格式无效，请核对原任务")
		}
		raw = []byte(encoded)
		// While pending, PikPak can echo the requested source IDs before
		// replacing them with the completed source-to-copy mapping.
		var requested []string
		for _, e := range t.Expected {
			if !strings.Contains(e.Path, "/") {
				requested = append(requested, e.ID)
			}
		}
		if encoded == strings.Join(requested, ",") {
			return nil
		}
	}
	var mapping map[string]string
	if json.Unmarshal(raw, &mapping) != nil || len(mapping) == 0 {
		return block("分享结果映射格式无效，请核对原任务；不会重复转存")
	}
	selected, publisher := []string{}, map[string]bool{}
	for _, e := range t.Expected {
		publisher[e.ID] = true
		if !strings.Contains(e.Path, "/") {
			selected = append(selected, e.ID)
		}
	}
	if len(mapping) != len(selected) {
		return block("分享结果映射与所选文件不一致，请核对原任务")
	}
	seen := map[string]bool{}
	for _, id := range selected {
		output := mapping[id]
		if output == "" || seen[output] || publisher[output] || output == t.TargetID || output == t.StageID {
			return block("分享结果映射无法唯一确认已保存文件，请核对原任务")
		}
		seen[output] = true
	}
	// Validate the complete map before changing any persisted output IDs.
	for _, id := range selected {
		t.addOutput(mapping[id])
	}
	return nil
}

package vault

import (
	"context"
	"errors"
	"strings"

	"pikpakvault/internal/pikpak"
)

// Some share restores return success without result IDs. Reconcile only the
// snapshotted destination, using the complete publisher manifest and content
// hashes. Names alone never establish ownership in a shared account.
func (a *App) reconcileShareResults(ctx context.Context, c pikpak.Provider, j *Job, t *TransferState) ([]string, error) {
	if t.Mode != "direct" || t.TargetID == "" || t.Started == 0 || len(t.Expected) == 0 {
		return nil, block("PikPak 未返回保存结果 ID，缺少转存前的目录或来源清单；请在任务详情中关联已保存的文件")
	}
	expected := map[string][]Entry{}
	publisherIDs, before := map[string]bool{}, map[string]bool{}
	leaves := 0
	for _, id := range t.BeforeIDs {
		before[id] = true
	}
	for _, e := range t.Expected {
		root := strings.SplitN(e.Path, "/", 2)[0]
		expected[root] = append(expected[root], e)
		publisherIDs[e.ID] = true
		if e.Kind != "folder" {
			leaves++
			if e.Hash == "" {
				return nil, block("PikPak 未返回保存结果 ID，来源缺少文件指纹，无法仅凭同名文件确认归属；请在任务详情中关联已保存的文件")
			}
		}
	}
	for name, manifest := range expected {
		rootFound := false
		for _, e := range manifest {
			rootFound = rootFound || e.Path == name
		}
		if !rootFound {
			return nil, block("保存的分享清单缺少父目录，请在任务详情中核对来源")
		}
	}
	if leaves == 0 {
		return nil, block("PikPak 未返回保存结果 ID，空目录无法按内容确认归属；请在任务详情中关联已保存的目录")
	}
	files, err := listAll(ctx, c, t.TargetID)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.ParentID != t.TargetID {
			return nil, block("PikPak 返回的保存位置与指定目录不一致，已暂停处理")
		}
	}
	var outputs []string
	var verified []RemoteEntry
	for name, manifest := range expected {
		var matches []pikpak.File
		var matchEntries []RemoteEntry
		incomplete := false
		for _, f := range files {
			if f.Name != name || before[f.ID] || publisherIDs[f.ID] || f.Trashed {
				continue
			}
			entries, err := transferTree(ctx, c, []pikpak.File{f})
			if err != nil {
				var pending *pending
				if errors.As(err, &pending) {
					incomplete = true
					continue
				}
				return nil, err
			}
			if !matchesManifest(entries, manifest) {
				continue
			}
			hashesMatch := true
			for _, entry := range entries {
				if !entry.File.Folder() && entry.File.Hash == "" {
					hashesMatch = false
				}
			}
			if !hashesMatch {
				continue
			}
			for _, entry := range entries {
				if before[entry.File.ID] || publisherIDs[entry.File.ID] {
					return nil, block("候选结果包含转存前已有的文件或分享原文件，请在任务详情中核对归属")
				}
			}
			matches = append(matches, f)
			matchEntries = entries
		}
		if len(matches) > 1 {
			return nil, block("目标目录中有多份内容一致的新增结果，无法唯一确认本次转存；请在任务详情中关联结果")
		}
		if incomplete || len(matches) == 0 {
			if now()-t.Started < 120 {
				return nil, t.wait("正在核对分享保存结果，等待目标目录中的文件完整可见")
			}
			return nil, block("尚未在原目标目录找到唯一且完整的分享副本。重试会重新核对；若文件保存在其他位置，请在任务详情中关联结果")
		}
		outputs = append(outputs, matches[0].ID)
		verified = append(verified, matchEntries...)
	}
	// Another import or a previously recorded resource may have claimed a new
	// candidate since the destination snapshot. Do not take over its IDs.
	rows, err := a.Store.DB.Query(`SELECT remote_id FROM bindings WHERE account_id=?
		UNION SELECT v.value FROM jobs j,json_each(j.data,'$.transfers') t,json_each(t.value,'$.output_ids') v
		WHERE j.account_id=? AND j.id<>?
		UNION SELECT json_extract(v.value,'$.file.id') FROM jobs j,json_each(j.data,'$.transfers') t,json_each(t.value,'$.entries') v
		WHERE j.account_id=? AND j.id<>?`, j.AccountID, j.AccountID, j.ID, j.AccountID, j.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claimed := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		claimed[id] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, e := range verified {
		if claimed[e.File.ID] {
			return nil, block("匹配的云端文件已关联其他资源或任务，请在任务详情中核对，避免重复登记")
		}
	}
	return outputs, nil
}

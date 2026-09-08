package vault

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"pikpakvault/internal/pikpak"
)

const DefaultRootPath = "My Pack/PikPakVault"

func normalizeRootPath(value string) (string, error) {
	value = strings.Trim(strings.ReplaceAll(strings.TrimSpace(value), `\`, "/"), "/")
	parts := strings.Split(value, "/")
	if value == "" || len(value) > 1024 || len(parts) > 16 {
		return "", fmt.Errorf("专用目录路径不能为空，最多 16 层、1024 字节")
	}
	for _, part := range parts {
		if ValidName(part) != nil || strings.TrimSpace(part) != part || strings.ContainsRune(part, ':') || strings.IndexFunc(part, unicode.IsControl) >= 0 {
			return "", fmt.Errorf("目录路径无效：请用 / 分隔文件夹，不能包含空层级、. 或 ..")
		}
	}
	return strings.Join(parts, "/"), nil
}

func (a *App) rootPath() string {
	value := a.Store.Get("root_path")
	if value == "" {
		return DefaultRootPath
	}
	return value
}

// Enqueue legacy root migration once. Failed or paused jobs remain visible for
// manual retry; an account switch or explicit settings change creates a new job.
func (a *App) scheduleRoot() {
	account := a.active()
	ac, err := a.Store.Account(account)
	if err != nil || ac.Identity == "" || ac.Status != "ready" {
		return
	}
	desired := a.rootPath()
	if a.Store.Get("root_path:"+account) == desired {
		return
	}
	var count int
	if err = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE account_id=? AND kind='root' AND json_extract(data,'$.root_path')=?`, account, desired).Scan(&count); err != nil || count > 0 {
		return
	}
	_, _ = a.Store.NewJob(account, "root", "调整专用目录 · "+desired, JobData{RootPath: desired})
}

func (a *App) prepareRoot(ctx context.Context, c pikpak.Provider, account string) error {
	_, err := a.resolveRoot(ctx, c, account, false)
	return err
}

func (a *App) ensureRoot(ctx context.Context, c pikpak.Provider, account string) (string, error) {
	cache := operation(ctx)
	if cache != nil && cache.root != "" && cache.rootPath == a.rootPath() {
		return cache.root, nil
	}
	id, err := a.resolveRoot(ctx, c, account, true)
	if err == nil && cache != nil {
		cache.root, cache.rootPath = id, a.rootPath()
	}
	return id, err
}

// Reuse only parent folders. The terminal folder must be the already bound root
// or an attributable creation marker; another folder is never silently adopted.
func (a *App) rootParent(ctx context.Context, c pikpak.Provider, parts []string, bound string) (string, error) {
	parent := ""
	for _, name := range parts {
		files, err := listAll(ctx, c, parent)
		if err != nil {
			return "", err
		}
		var matches []pikpak.File
		for _, f := range files {
			if f.Name == name {
				matches = append(matches, f)
			}
		}
		if len(matches) > 1 || len(matches) == 1 && !matches[0].Folder() {
			return "", block("路径存在重名或非文件夹项目：" + name)
		}
		var folder pikpak.File
		if len(matches) == 1 {
			folder = matches[0]
		} else {
			folder, err = c.Mkdir(ctx, parent, name)
			if err != nil {
				return "", err
			}
		}
		if folder.ID == "" || !folder.Folder() || folder.Trashed {
			return "", block("无法确认路径中的文件夹：" + name)
		}
		if folder.ID == bound && bound != "" {
			return "", block("目标路径不能位于当前专用目录内部，请选择其他父目录")
		}
		parent = folder.ID
	}
	return parent, nil
}

func (a *App) resolveRoot(ctx context.Context, c pikpak.Provider, account string, restore bool) (string, error) {
	ac, err := a.Store.Account(account)
	if err != nil {
		return "", err
	}
	if ac.Identity == "" {
		return "", block("Verify this account before creating the library root")
	}
	desired, err := normalizeRootPath(a.rootPath())
	if err != nil {
		return "", err
	}
	parts := strings.Split(desired, "/")
	name := parts[len(parts)-1]
	var root pikpak.File
	if ac.RootID != "" {
		root, err = c.Get(ctx, ac.RootID)
		if err != nil && !pikpak.Missing(err) {
			return "", err
		}
		if err == nil && !root.Trashed && !root.Folder() {
			return "", block("The bound root is not a directory")
		}
		if (pikpak.Missing(err) || root.Trashed) && !restore {
			// Applying a location rule never restores deleted data by itself.
			return ac.RootID, a.Store.Set("root_path:"+account, desired)
		}
		if pikpak.Missing(err) {
			root = pikpak.File{}
		}
		if root.ID != "" && !root.Trashed && a.Store.Get("root_path:"+account) == desired && root.Name == name && root.ParentID == a.Store.Get("root_parent:"+account) {
			return root.ID, nil
		}
	}
	parent, err := a.rootParent(ctx, c, parts[:len(parts)-1], ac.RootID)
	if err != nil {
		return "", err
	}
	files, err := listAll(ctx, c, parent)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if f.Name == name && f.ID != root.ID {
			return "", block("目标位置已有同名项目，未合并或覆盖。请更换专用目录路径：" + desired)
		}
	}
	if root.ID == "" {
		marker := ".vault-root-" + a.Store.Get("instance")
		for _, f := range files {
			if f.Name == marker {
				if root.ID != "" || !f.Folder() {
					return "", block("专用目录创建结果不唯一，需要人工检查")
				}
				root = f
			}
		}
		if root.ID == "" {
			root, err = c.Mkdir(ctx, parent, marker)
			if err != nil {
				return "", err
			}
		}
		if root.ID == "" || !root.Folder() {
			return "", block("Root creation returned no valid directory")
		}
		// Persist identity before renaming, so interrupted creation can resume by ID.
		if _, err = a.Store.DB.Exec(`UPDATE accounts SET root_id=? WHERE id=?`, root.ID, account); err != nil {
			return "", err
		}
	}
	if root.Trashed {
		if err = c.Untrash(ctx, []string{root.ID}); err != nil {
			return "", err
		}
		root, err = c.Get(ctx, root.ID)
		if err != nil {
			return "", err
		}
		if root.Trashed {
			return "", wait("等待 PikPak 确认专用目录已还原")
		}
	}
	if root.ParentID != parent {
		for _, f := range files {
			if f.ID != root.ID && f.Name == root.Name {
				return "", block("迁移时发现同名项目，请更换目标路径后重试")
			}
		}
		if err = c.Move(ctx, root.ID, parent); err != nil {
			return "", err
		}
	}
	if root.Name != name {
		if err = c.Rename(ctx, root.ID, name); err != nil {
			return "", err
		}
	}
	check, err := c.Get(ctx, root.ID)
	if err != nil {
		return "", err
	}
	if check.Trashed || !check.Folder() || check.Name != name || check.ParentID != parent {
		return "", wait("等待 PikPak 确认专用目录的位置")
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	for key, value := range map[string]string{"root_path:" + account: desired, "root_parent:" + account: parent} {
		if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	a.Store.Event("root_path", map[string]string{"account_id": account, "path": desired})
	return check.ID, nil
}

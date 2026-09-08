package aria2

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
	"pikpakvault/internal/update"
)

type distribution struct{ URL, ArchiveHash, BinaryHash, Entry string }

// Pin both the downloaded archive and extracted executable. A replaced upstream
// release cannot silently change the program we run. Linux builds are static.
var distributions = map[string]distribution{
	"linux/amd64":   {"https://github.com/abcfy2/aria2-static-build/releases/download/1.37.0/aria2-x86_64-linux-musl_static.zip", "e0a09b12ef67f35f8a8e4fdddbec851d235b7c31da549d0578bff459032b499a", "80e577dc58348b96da46dd12d326bc99794b5021be395a3e890f2d67c8790c22", "aria2c"},
	"linux/arm64":   {"https://github.com/abcfy2/aria2-static-build/releases/download/1.37.0/aria2-aarch64-linux-musl_static.zip", "0c681a89a40e0f82d1f5137608e86257eb0af201459c002941ea098f2b8c26b6", "99a057bd383a28f1d5fab6e8cc5f6f3ed4172f5e65c59548242e965454d654c1", "aria2c"},
	"windows/amd64": {"https://github.com/aria2/aria2/releases/download/release-1.37.0/aria2-1.37.0-win-64bit-build1.zip", "67d015301eef0b612191212d564c5bb0a14b5b9c4796b76454276a4d28d9b288", "be2099c214f63a3cb4954b09a0becd6e2e34660b886d4c898d260febfe9d70c2", "aria2-1.37.0-win-64bit-build1/aria2c.exe"},
}

func ProgramDir() string {
	if v := os.Getenv("VAULT_PROGRAM_DIR"); v != "" {
		if p, e := filepath.Abs(v); e == nil {
			return p
		}
	}
	file, e := os.Executable()
	if e == nil {
		return filepath.Dir(file)
	}
	return "."
}
func executable(home string) string {
	name := "aria2c"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, "runtime", "aria2", "1.37.0", name)
}
func hash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func Installed(home string) (string, error) {
	d, ok := distributions[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("暂不支持此平台的专用 aria2")
	}
	file := executable(home)
	info, e := os.Lstat(file)
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 32<<20 {
		return "", fmt.Errorf("专用 aria2 文件无效")
	}
	f, e := os.Open(file)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, io.LimitReader(f, 32<<20)); e != nil {
		return "", e
	}
	if hex.EncodeToString(h.Sum(nil)) != d.BinaryHash {
		return "", fmt.Errorf("专用 aria2 校验失败，将重新下载")
	}
	return file, nil
}
func Ensure(ctx context.Context, home string) (string, error) {
	d, ok := distributions[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("暂不支持此平台的专用 aria2")
	}
	return install(ctx, home, d, &http.Client{Timeout: 3 * time.Minute})
}
func install(ctx context.Context, home string, d distribution, client *http.Client) (string, error) {
	if file, e := Installed(home); e == nil {
		return file, nil
	}
	file := executable(home)
	dir := filepath.Dir(file)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", fmt.Errorf("无法创建专用 aria2 目录，请检查程序目录权限：%w", e)
	}
	lock := flock.New(filepath.Join(dir, "install.lock"))
	ok, e := lock.TryLockContext(ctx, 100*time.Millisecond)
	if e != nil {
		return "", e
	}
	if !ok {
		return "", fmt.Errorf("正在安装专用 aria2，请稍后重试")
	}
	defer lock.Unlock()
	if file, e := Installed(home); e == nil {
		return file, nil
	}
	req, e := http.NewRequestWithContext(ctx, "GET", d.URL, nil)
	if e != nil {
		return "", e
	}
	req.Header.Set("User-Agent", "PikPakVault/aria2-installer")
	resp, e := client.Do(req)
	if e != nil {
		return "", fmt.Errorf("下载专用 aria2 失败，请检查服务器到 GitHub 的连接后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("下载专用 aria2 失败：HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if e != nil {
		return "", fmt.Errorf("下载专用 aria2 中断，请重试")
	}
	if len(b) > 32<<20 || hash(b) != d.ArchiveHash {
		return "", fmt.Errorf("专用 aria2 安装包 SHA-256 不符，已拒绝运行")
	}
	b, e = extractBinary(b, d)
	if e != nil {
		return "", e
	}
	if e = update.Atomic(file, b, 0700); e != nil {
		return "", e
	}
	return file, nil
}
func extractBinary(b []byte, d distribution) ([]byte, error) {
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		return nil, e
	}
	var binary []byte
	for _, entry := range z.File {
		if entry.Name != d.Entry {
			continue
		}
		if binary != nil || !entry.Mode().IsRegular() || entry.UncompressedSize64 > 32<<20 {
			return nil, fmt.Errorf("aria2 安装包包含无效程序文件")
		}
		r, e := entry.Open()
		if e != nil {
			return nil, e
		}
		binary, e = io.ReadAll(io.LimitReader(r, (32<<20)+1))
		r.Close()
		if e != nil {
			return nil, e
		}
	}
	if hash(binary) != d.BinaryHash {
		return nil, fmt.Errorf("专用 aria2 可执行文件校验失败")
	}
	return binary, nil
}

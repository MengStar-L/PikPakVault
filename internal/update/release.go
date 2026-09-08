// Package update handles public GitHub releases and systemd maintenance.
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const DefaultRepository = "MengStar-L/PikPakVault"

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func Repository() string {
	if v := os.Getenv("VAULT_UPDATE_REPOSITORY"); v != "" {
		return v
	}
	return DefaultRepository
}
func ValidTag(tag string) bool { return len(tag) < 60 && tagPattern.MatchString(tag) }
func Newer(tag, current string) bool {
	if !ValidTag(tag) {
		return false
	}
	current = "v" + strings.TrimPrefix(current, "v")
	if !ValidTag(current) {
		return false
	}
	a, b := strings.Split(tag[1:], "."), strings.Split(current[1:], ".")
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) > len(b[i])
		}
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}
type Release struct {
	Tag        string  `json:"tag_name"`
	Name       string  `json:"name"`
	Body       string  `json:"body"`
	URL        string  `json:"html_url"`
	Published  string  `json:"published_at"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}
type Client struct {
	HTTP       *http.Client
	API        string
	Repository string
	Arch       string
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 10 * time.Minute}, API: "https://api.github.com", Repository: Repository(), Arch: runtime.GOARCH}
}
func (c *Client) get(ctx context.Context, address string, limit int64) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", address, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("User-Agent", "PikPakVault-updater")
	req.Header.Set("Accept", "application/vnd.github+json")
	r, e := c.HTTP.Do(req)
	if e != nil {
		return nil, fmt.Errorf("无法连接 GitHub：%w", e)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		if r.StatusCode == 404 {
			return nil, fmt.Errorf("GitHub 尚无可用的正式 Release，或仓库不存在")
		}
		if r.StatusCode == 403 || r.StatusCode == 429 {
			return nil, fmt.Errorf("GitHub 限流或拒绝访问（HTTP %d），请稍后重试", r.StatusCode)
		}
		return nil, fmt.Errorf("GitHub 返回 HTTP %d", r.StatusCode)
	}
	data, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if e == nil && int64(len(data)) > limit {
		e = fmt.Errorf("GitHub 响应超出大小限制")
	}
	return data, e
}
func (c *Client) Release(ctx context.Context, tag string) (Release, error) {
	var r Release
	if !repoPattern.MatchString(c.Repository) {
		return r, fmt.Errorf("更新仓库必须是 owner/repository 格式")
	}
	endpoint := "latest"
	if tag != "" {
		if !ValidTag(tag) {
			return r, fmt.Errorf("版本号无效")
		}
		endpoint = "tags/" + tag
	}
	b, e := c.get(ctx, c.API+"/repos/"+c.Repository+"/releases/"+endpoint, 2<<20)
	if e != nil {
		return r, e
	}
	if e = json.Unmarshal(b, &r); e != nil {
		return r, e
	}
	if r.Draft || r.Prerelease || !ValidTag(r.Tag) || (tag != "" && tag != r.Tag) {
		return r, fmt.Errorf("仅支持已发布的正式语义版本")
	}
	// Use a canonical link instead of trusting HTML URLs from a response.
	r.URL = "https://github.com/" + c.Repository + "/releases/tag/" + r.Tag
	return r, nil
}
func (c *Client) asset(r Release, name string) (Asset, error) {
	var found []Asset
	for _, a := range r.Assets {
		if a.Name == name {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return Asset{}, fmt.Errorf("Release 缺少唯一的 %s", name)
	}
	a := found[0]
	u, e := url.Parse(a.URL)
	if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/"+c.Repository+"/releases/download/"+r.Tag+"/"+name {
		return a, fmt.Errorf("更新下载地址不属于指定 Release")
	}
	return a, nil
}
func (c *Client) Download(ctx context.Context, r Release, destination string) error {
	if c.Arch != "amd64" && c.Arch != "arm64" {
		return fmt.Errorf("仅支持 Linux amd64 / arm64")
	}
	name := "pikpak-vault-" + strings.TrimPrefix(r.Tag, "v") + "-linux-" + c.Arch + ".tar.gz"
	asset, e := c.asset(r, name)
	if e != nil {
		return e
	}
	if asset.Size < 1 || asset.Size > 256<<20 {
		return fmt.Errorf("安装包大小无效或超过 256 MiB")
	}
	sums, e := c.asset(r, "SHA256SUMS")
	if e != nil {
		return e
	}
	body, e := c.get(ctx, sums.URL, 64<<10)
	if e != nil {
		return e
	}
	var digest string
	count := 0
	for _, line := range strings.Split(string(body), "\n") {
		p := strings.Fields(line)
		if len(p) == 2 && strings.TrimPrefix(p[1], "*") == name {
			digest = p[0]
			count++
		}
	}
	hash, e := hex.DecodeString(digest)
	if e != nil || len(hash) != 32 || count != 1 {
		return fmt.Errorf("SHA256SUMS 未提供唯一有效的安装包校验值")
	}
	body, e = c.get(ctx, asset.URL, 256<<20)
	if e != nil {
		return e
	}
	if int64(len(body)) != asset.Size {
		return fmt.Errorf("安装包下载不完整")
	}
	actual := sha256.Sum256(body)
	if hex.EncodeToString(actual[:]) != strings.ToLower(digest) {
		return fmt.Errorf("安装包 SHA-256 校验失败")
	}
	if e = ExtractBinary(bytes.NewReader(body), destination); e != nil {
		return e
	}
	f, e := elf.Open(destination)
	if e != nil {
		return fmt.Errorf("安装包内不是 Linux 可执行文件")
	}
	defer f.Close()
	expected := elf.EM_X86_64
	if c.Arch == "arm64" {
		expected = elf.EM_AARCH64
	}
	if f.Machine != expected || f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("可执行文件架构不匹配")
	}
	return nil
}

// ExtractBinary checks every tar entry; archive files never choose filesystem paths.
func ExtractBinary(src io.Reader, destination string) (err error) {
	gz, e := gzip.NewReader(src)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, 512<<20))
	found := false
	defer func() {
		if err != nil {
			os.Remove(destination)
		}
	}()
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, "\\") || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("安装包含不安全路径")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("安装包不能含链接或特殊文件")
		}
		if h.Size < 0 || h.Size > 256<<20 {
			return fmt.Errorf("安装包成员过大")
		}
		if name != "vault" {
			continue
		}
		if h.Typeflag != tar.TypeReg || found {
			return fmt.Errorf("安装包可执行文件重复或无效")
		}
		found = true
		f, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if e != nil {
			return e
		}
		_, e = io.Copy(f, tr)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	if !found {
		return fmt.Errorf("安装包缺少 vault")
	}
	return nil
}

func portURL(listen string) string {
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	return "http://" + listen + "/healthz"
}
func HealthURL() string {
	if v := os.Getenv("VAULT_HEALTH_URL"); v != "" {
		return v
	}
	listen := os.Getenv("VAULT_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:5675"
	}
	if strings.HasPrefix(listen, "0.0.0.0:") {
		listen = "127.0.0.1:" + strings.TrimPrefix(listen, "0.0.0.0:")
	}
	return portURL(listen)
}
func PIDString() string { return strconv.Itoa(os.Getpid()) }

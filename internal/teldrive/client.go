// Package teldrive implements the documented TelDrive HTTP API. Credentials
// stay server-side; downloads use its authenticated, decrypted file stream.
package teldrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type File struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	ParentID string `json:"parentId"`
	Size     int64  `json:"size"`
	Mime     string `json:"mimeType"`
	Hash     string `json:"hash"` // TelDrive BLAKE3, never a PikPak GCID.
	Updated  string `json:"updatedAt"`
}
type Page struct {
	Items []File `json:"items"`
	Meta  struct {
		Count   int `json:"count"`
		Pages   int `json:"totalPages"`
		Current int `json:"currentPage"`
	} `json:"meta"`
}
type Entry struct {
	File
	Path string `json:"relative_path"`
}
type Client struct {
	Base, Token string
	HTTP        *http.Client
}

func NormalizeBase(raw string) (string, error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("请输入 TelDrive 的 HTTP(S) 站点地址，不含凭据、查询参数或片段")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}
func New(base, token string) (*Client, error) {
	base, e := NormalizeBase(base)
	if e != nil {
		return nil, e
	}
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
	if token == "" || strings.ContainsAny(token, "\r\n;\t ") {
		return nil, fmt.Errorf("请输入 TelDrive access_token 的值")
	}
	return &Client{Base: base, Token: token, HTTP: &http.Client{}}, nil
}
func (c *Client) request(ctx context.Context, endpoint string, q url.Values, rangeValue string) (*http.Response, error) {
	u := c.Base + "/api" + endpoint
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	r, e := http.NewRequestWithContext(ctx, "GET", u, nil)
	if e != nil {
		return nil, fmt.Errorf("TelDrive 请求地址无效")
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.AddCookie(&http.Cookie{Name: "access_token", Value: c.Token})
	if rangeValue != "" {
		r.Header.Set("Range", rangeValue)
	}
	client := *c.HTTP
	// A redirect must never forward a session to another origin or a login page.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := client.Do(r)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("TelDrive GET %s：网络中断或超时", endpoint)
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		message := "请求失败"
		switch resp.StatusCode {
		case 401, 403:
			message = "认证失效，请更新 TelDrive access_token"
		case 404:
			message = "源文件或目录不存在"
		case 429:
			message = "上游限流，请稍后重试"
		}
		return nil, fmt.Errorf("TelDrive GET %s：HTTP %d，%s", endpoint, resp.StatusCode, message)
	}
	return resp, nil
}
func (c *Client) json(ctx context.Context, endpoint string, q url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	r, e := c.request(ctx, endpoint, q, "")
	if e != nil {
		return e
	}
	defer r.Body.Close()
	if e = json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(out); e != nil {
		return fmt.Errorf("TelDrive %s：响应不是有效的文件数据", endpoint)
	}
	return nil
}
func (c *Client) Get(ctx context.Context, id string) (File, error) {
	var f File
	e := c.json(ctx, "/files/"+url.PathEscape(id), nil, &f)
	if e == nil && f.ID != id {
		e = fmt.Errorf("TelDrive 返回的文件身份不匹配")
	}
	return f, e
}
func (c *Client) List(ctx context.Context, parent string, page int) (Page, error) {
	q := url.Values{"operation": {"list"}, "status": {"active"}, "sort": {"id"}, "order": {"asc"}, "limit": {"1000"}, "page": {strconv.Itoa(page)}}
	if parent == "" {
		q.Set("path", "/")
	} else {
		q.Set("parentId", parent)
	}
	var p Page
	e := c.json(ctx, "/files", q, &p)
	if e == nil && (p.Items == nil || p.Meta.Current != page || p.Meta.Count < 0 || p.Meta.Pages < 0) {
		e = fmt.Errorf("TelDrive 目录分页信息不完整")
	}
	return p, e
}
func (c *Client) Tree(ctx context.Context, root string) ([]Entry, error) {
	if root != "" {
		f, e := c.Get(ctx, root)
		if e != nil {
			return nil, e
		}
		if f.Type != "folder" {
			return nil, fmt.Errorf("监控目标不是文件夹")
		}
	}
	out := []Entry{}
	seen := map[string]bool{}
	var walk func(string, string, int) error
	walk = func(parent, prefix string, depth int) error {
		if depth > 100 {
			return fmt.Errorf("TelDrive 目录层级超过 100 层")
		}
		all := []File{}
		total := -1
		for page := 1; page <= 10000; page++ {
			p, e := c.List(ctx, parent, page)
			if e != nil {
				return e
			}
			if total < 0 {
				total = p.Meta.Count
			}
			if total != p.Meta.Count {
				return fmt.Errorf("TelDrive 目录在分页期间发生变化，请重新扫描")
			}
			for _, f := range p.Items {
				if f.ID == "" || seen[f.ID] || f.ParentID != parent || f.Name == "" || f.Name == "." || f.Name == ".." || strings.ContainsAny(f.Name, "/\\\x00") || (f.Type != "file" && f.Type != "folder") || f.Size < 0 {
					return fmt.Errorf("TelDrive 目录有重复或无效条目，已停止本次扫描")
				}
				seen[f.ID] = true
				all = append(all, f)
			}
			if page >= p.Meta.Pages {
				break
			}
			if len(p.Items) == 0 || page == 10000 {
				return fmt.Errorf("TelDrive 分页未完成")
			}
		}
		if len(all) != total {
			return fmt.Errorf("TelDrive 目录分页数量不一致，请重新扫描")
		}
		for _, f := range all {
			rel := prefix + f.Name
			out = append(out, Entry{f, rel})
			if len(out) > 100000 {
				return fmt.Errorf("单次监控最多支持 100000 项")
			}
			if f.Type == "folder" {
				if e := walk(f.ID, rel+"/", depth+1); e != nil {
					return e
				}
			}
		}
		return nil
	}
	e := walk(root, "", 0)
	return out, e
}

// OpenRange checks both the advertised range and the byte count. Callers also
// read one byte past the requested length to detect incorrect upstream streams.
func (c *Client) OpenRange(ctx context.Context, f File, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 || offset > f.Size || length > f.Size-offset {
		return nil, fmt.Errorf("TelDrive 文件范围无效")
	}
	ctx, cancel := context.WithCancel(ctx)
	idle := time.AfterFunc(90*time.Second, cancel)
	r, e := c.request(ctx, "/files/"+url.PathEscape(f.ID)+"/"+url.PathEscape(f.Name), nil, fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	if e != nil {
		idle.Stop()
		cancel()
		return nil, e
	}
	valid := r.StatusCode == 206 && r.Header.Get("Content-Range") == fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, f.Size)
	if r.StatusCode == 200 && offset == 0 && length == f.Size {
		valid = true
	}
	if !valid || (r.ContentLength >= 0 && r.ContentLength != length) {
		r.Body.Close()
		idle.Stop()
		cancel()
		return nil, fmt.Errorf("TelDrive 文件流的 Range 或长度不匹配")
	}
	return &idleStream{ReadCloser: r.Body, timer: idle, cancel: cancel}, nil
}

type idleStream struct {
	io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
}

func (s *idleStream) Read(p []byte) (int, error) {
	n, e := s.ReadCloser.Read(p)
	if n > 0 {
		s.timer.Reset(90 * time.Second)
	}
	return n, e
}
func (s *idleStream) Close() error { s.timer.Stop(); s.cancel(); return s.ReadCloser.Close() }

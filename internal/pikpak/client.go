package pikpak

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const clientID = "YUMx5nI8ZU8Ap8pm"
const clientSecret = "dbw2OtmVEeuUvIptb1Coyg"
const clientVersion = "2.0.0"
const packageName = "mypikpak.com"

// Protocol constants from rclone's MIT-licensed PikPak backend; see THIRD_PARTY.md.
var salts = []string{"C9qPpZLN8ucRTaTiUMWYS9cQvWOE", "+r6CQVxjzJV6LCV", "F", "pFJRC", "9WXYIDGrwTCz2OiVlgZa90qpECPD6olt", "/750aCr4lm/Sly/c", "RB+DT/gZCrbV", "", "CyLsf7hdkIRxRm215hl", "7xHvLi2tOYP0Y92b", "ZGTXXxu8E/MIWaEDB+Sm/", "1UI3", "E7fP5Pfijd+7K+t6Tg/NhuLq0eEUVChpJSkrKxpO", "ihtqpG6FMt65+Xk+tWUH2", "NhXXU9rg4XXdzo7u5o"}

type Client struct {
	HTTP            *http.Client
	DriveURL        string
	UserURL         string
	Credentials     Credentials
	Save            func(Credentials) error
	BeforeWrite     func() (func(), error)
	RequestInterval time.Duration
	WriteInterval   time.Duration
	lastRequest     time.Time
	lastWrite       time.Time
	cooldownUntil   time.Time
	mu              sync.Mutex
}

func New(c Credentials) *Client {
	if c.DeviceID == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		c.DeviceID = hex.EncodeToString(b)
	}
	return &Client{HTTP: &http.Client{Timeout: 45 * time.Second}, DriveURL: "https://api-drive.mypikpak.com", UserURL: "https://user.mypikpak.com", Credentials: c, RequestInterval: 500 * time.Millisecond, WriteInterval: 2 * time.Second}
}
func (c *Client) persist() error {
	if c.Save != nil {
		return c.Save(c.Credentials)
	}
	return nil
}
func (c *Client) redact(s string) string {
	for _, v := range []string{c.Credentials.Username, c.Credentials.Password, c.Credentials.AccessToken, c.Credentials.RefreshToken, c.Credentials.CaptchaToken} {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}
func (c *Client) raw(ctx context.Context, host, method, path string, q url.Values, body, out any) error {
	if err := c.pace(ctx, method, path); err != nil {
		return err
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	u := host + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 PikPakVault/1.0")
	req.Header.Set("Referer", "https://mypikpak.com/")
	req.Header.Set("X-Client-ID", clientID)
	req.Header.Set("X-Client-Version", clientVersion)
	req.Header.Set("X-Device-ID", c.Credentials.DeviceID)
	if c.Credentials.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.Credentials.AccessToken)
	}
	if c.Credentials.CaptchaToken != "" {
		req.Header.Set("X-Captcha-Token", c.Credentials.CaptchaToken)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Endpoint: path, Code: "network_error", Message: "request failed or timed out; remote result must be reconciled"}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &APIError{Endpoint: path, Code: "response_interrupted", Message: "response incomplete"}
	}
	var envelope struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &envelope)
	if resp.StatusCode >= 400 || envelope.Error != "" {
		e := DecodeError(b, resp.StatusCode, path)
		e.Message = c.redact(e.Message)
		// Upstream errors can echo submitted URLs and extraction codes.
		if bodyMap, ok := body.(map[string]any); ok {
			for _, key := range []string{"pass_code", "pass_code_token"} {
				if value, ok := bodyMap[key].(string); ok && value != "" {
					e.Message = strings.ReplaceAll(e.Message, value, "[redacted]")
				}
			}
		}
		for _, key := range []string{"pass_code", "pass_code_token"} {
			if v := q.Get(key); v != "" {
				e.Message = strings.ReplaceAll(e.Message, v, "[redacted]")
			}
		}
		e.RetryAfter, _ = strconv.Atoi(resp.Header.Get("Retry-After"))
		if stamp, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			e.RetryAfter = max(e.RetryAfter, int(time.Until(stamp).Seconds())+1)
		}
		if e.Status == 429 {
			e.RetryAfter = max(e.RetryAfter, 60)
			c.cooldownUntil = time.Now().Add(time.Duration(e.RetryAfter) * time.Second)
		}
		return e
	}
	if out != nil && len(b) > 0 {
		if err = json.Unmarshal(b, out); err != nil {
			return &APIError{Status: resp.StatusCode, Endpoint: path, Code: "invalid_response", Message: "unexpected JSON response"}
		}
	}
	return nil
}

// Called under mu, so every endpoint (including refresh/captcha and UI reads)
// shares the account's pacing and cooldown. Waiting is cancellable.
func (c *Client) pace(ctx context.Context, method, endpoint string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if remaining := time.Until(c.cooldownUntil); remaining > 0 {
		return &APIError{Status: 429, Endpoint: endpoint, Code: "account_cooldown", Message: "账号请求已降速，等待限流冷却结束", RetryAfter: int(remaining.Seconds()) + 1}
	}
	next := c.lastRequest.Add(c.RequestInterval)
	if method != "GET" && c.lastWrite.Add(c.WriteInterval).After(next) {
		next = c.lastWrite.Add(c.WriteInterval)
	}
	if delay := time.Until(next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	c.lastRequest = time.Now()
	if method != "GET" {
		c.lastWrite = c.lastRequest
	}
	return nil
}
func (c *Client) captcha(ctx context.Context, action string) error {
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	s := clientID + clientVersion + packageName + c.Credentials.DeviceID + stamp
	for _, salt := range salts {
		h := md5.Sum([]byte(s + salt))
		s = hex.EncodeToString(h[:])
	}
	meta := map[string]string{"captcha_sign": "1." + s, "timestamp": stamp, "client_version": clientVersion, "package_name": packageName, "user_id": c.Credentials.UserID}
	if action == "POST:/v1/auth/signin" {
		meta = map[string]string{"username": c.Credentials.Username}
	}
	var res struct {
		Token   string `json:"captcha_token"`
		Expires int64  `json:"expires_in"`
		URL     string `json:"url"`
	}
	err := c.raw(ctx, c.UserURL, "POST", "/v1/shield/captcha/init", nil, map[string]any{"action": action, "client_id": clientID, "device_id": c.Credentials.DeviceID, "captcha_token": c.Credentials.CaptchaToken, "meta": meta}, &res)
	if err != nil {
		return err
	}
	c.Credentials.CaptchaToken = res.Token
	c.Credentials.CaptchaExpiry = time.Now().Unix() + res.Expires
	if err = c.persist(); err != nil {
		return err
	}
	if res.URL != "" {
		return &APIError{Status: 403, Endpoint: "/v1/shield/captcha/init", Code: "verification_required", Message: "account verification required", VerificationURL: res.URL}
	}
	return nil
}
func (c *Client) login(ctx context.Context) error {
	var res struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Sub     string `json:"sub"`
		Expires int64  `json:"expires_in"`
	}
	var err error
	if c.Credentials.RefreshToken != "" {
		err = c.raw(ctx, c.UserURL, "POST", "/v1/auth/token", nil, map[string]any{"client_id": clientID, "client_secret": clientSecret, "grant_type": "refresh_token", "refresh_token": c.Credentials.RefreshToken}, &res)
		if err != nil && (Temporary(err) || c.Credentials.Password == "") {
			return err
		}
	}
	if res.Access == "" {
		if c.Credentials.Username == "" || c.Credentials.Password == "" {
			return &APIError{Status: 401, Code: "credentials_required", Message: "account password or refresh token required"}
		}
		if err = c.captcha(ctx, "POST:/v1/auth/signin"); err != nil {
			return err
		}
		err = c.raw(ctx, c.UserURL, "POST", "/v1/auth/signin", nil, map[string]any{"client_id": clientID, "client_secret": clientSecret, "username": c.Credentials.Username, "password": c.Credentials.Password}, &res)
		if err != nil {
			return err
		}
	}
	if res.Access == "" {
		return &APIError{Status: 502, Code: "invalid_token_response", Message: "access token missing"}
	}
	if c.Credentials.UserID != "" && res.Sub != "" && c.Credentials.UserID != res.Sub {
		return &APIError{Status: 409, Code: "identity_changed", Message: "account identity differs from stored identity"}
	}
	c.Credentials.AccessToken = res.Access
	if res.Refresh != "" {
		c.Credentials.RefreshToken = res.Refresh
	}
	if res.Sub != "" {
		c.Credentials.UserID = res.Sub
	}
	c.Credentials.Expiry = time.Now().Unix() + res.Expires
	return c.persist()
}
func (c *Client) call(ctx context.Context, host, method, path string, q url.Values, body, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Credentials.AccessToken == "" || (c.Credentials.Expiry > 0 && c.Credentials.Expiry < time.Now().Unix()+30) {
		if err := c.login(ctx); err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if c.Credentials.CaptchaToken == "" || (c.Credentials.CaptchaExpiry > 0 && c.Credentials.CaptchaExpiry < time.Now().Unix()+10) {
			if err := c.captcha(ctx, method+":"+path); err != nil {
				return err
			}
		}
		var unlock func()
		if method != "GET" && c.BeforeWrite != nil {
			var err error
			unlock, err = c.BeforeWrite()
			if err != nil {
				return err
			}
		}
		err := c.raw(ctx, host, method, path, q, body, out)
		if unlock != nil {
			unlock()
		}
		if e, ok := err.(*APIError); ok && attempt == 0 {
			if e.Status == 401 || e.Code == "unauthenticated" {
				if err = c.login(ctx); err != nil {
					return err
				}
				continue
			}
			if e.Code == "captcha_invalid" || e.Code == "captcha_token_expired" || e.Code == "captcha_required" {
				c.Credentials.CaptchaToken = ""
				continue
			}
		}
		return err
	}
	return fmt.Errorf("authentication retry exhausted")
}
func (c *Client) drive(ctx context.Context, method, path string, q url.Values, body, out any) error {
	return c.call(ctx, c.DriveURL, method, path, q, body, out)
}
func (c *Client) Me(ctx context.Context) (Identity, error) {
	var r Identity
	e := c.call(ctx, c.UserURL, "GET", "/v1/user/me", nil, nil, &r)
	if e == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.Sub == "" {
			return r, &APIError{Status: 502, Code: "invalid_identity", Message: "account identity missing"}
		}
		if c.Credentials.UserID != "" && c.Credentials.UserID != r.Sub {
			return r, &APIError{Status: 409, Code: "identity_changed", Message: "credentials belong to a different account"}
		}
		if c.Credentials.UserID == "" {
			c.Credentials.UserID = r.Sub
			e = c.persist()
		}
	}
	return r, e
}
func (c *Client) Quota(ctx context.Context) (Quota, error) {
	var r struct {
		Quota Quota `json:"quota"`
	}
	e := c.drive(ctx, "GET", "/drive/v1/about", nil, nil, &r)
	return r.Quota, e
}
func (c *Client) List(ctx context.Context, parent, page string) (Page, error) {
	var r Page
	e := c.drive(ctx, "GET", "/drive/v1/files", url.Values{"parent_id": {parent}, "page_token": {page}, "limit": {"100"}, "thumbnail_size": {"SIZE_LARGE"}, "filters": {`{"trashed":{"eq":false}}`}}, nil, &r)
	return r, e
}
func (c *Client) Get(ctx context.Context, id string) (File, error) {
	var r File
	e := c.drive(ctx, "GET", "/drive/v1/files/"+url.PathEscape(id), url.Values{"usage": {"FETCH"}, "thumbnail_size": {"SIZE_LARGE"}}, nil, &r)
	var up *APIError
	if errors.As(e, &up) && up.Code == "file_in_recycle_bin" {
		// Some responses carry only a tombstone, without name/kind metadata.
		// Callers must untrash and re-read before validating content or type.
		return File{ID: id, Trashed: true}, nil
	}
	return r, e
}
func (c *Client) Mkdir(ctx context.Context, parent, name string) (File, error) {
	var r Transfer
	e := c.drive(ctx, "POST", "/drive/v1/files", nil, map[string]any{"kind": "drive#folder", "parent_id": parent, "name": name, "folder_type": ""}, &r)
	if e == nil && r.File == nil {
		e = fmt.Errorf("folder response has no file")
	}
	if r.File != nil {
		return *r.File, e
	}
	return File{}, e
}
func (c *Client) batch(ctx context.Context, action string, ids []string, to string) error {
	b := map[string]any{"ids": ids}
	if to != "" {
		b["to"] = map[string]string{"parent_id": to}
	}
	return c.drive(ctx, "POST", "/drive/v1/files:"+action, nil, b, nil)
}
func (c *Client) Move(ctx context.Context, id, parent string) error {
	return c.drive(ctx, "POST", "/drive/v1/files:batchMove", nil, map[string]any{"ids": []string{id}, "to": map[string]string{"parent_id": parent}}, nil)
}
func (c *Client) Rename(ctx context.Context, id, name string) error {
	return c.drive(ctx, "PATCH", "/drive/v1/files/"+url.PathEscape(id), nil, map[string]string{"name": name}, nil)
}
func (c *Client) Trash(ctx context.Context, ids []string) error {
	return c.batch(ctx, "batchTrash", ids, "")
}
func (c *Client) Untrash(ctx context.Context, ids []string) error {
	return c.batch(ctx, "batchUntrash", ids, "")
}
func (c *Client) Offline(ctx context.Context, link, parent string) (Transfer, error) {
	var r Transfer
	e := c.drive(ctx, "POST", "/drive/v1/files", nil, map[string]any{"kind": "drive#file", "parent_id": parent, "upload_type": "UPLOAD_TYPE_URL", "folder_type": "", "url": map[string]string{"url": link}}, &r)
	return r, e
}
func (c *Client) Tasks(ctx context.Context) ([]Task, error) {
	out := []Task{}
	next := ""
	seen := map[string]bool{}
	for {
		var r struct {
			Tasks []Task `json:"tasks"`
			Next  string `json:"next_page_token"`
		}
		e := c.drive(ctx, "GET", "/drive/v1/tasks", url.Values{"limit": {"100"}, "page_token": {next}}, nil, &r)
		if e != nil {
			return nil, e
		}
		out = append(out, r.Tasks...)
		if r.Next == "" {
			return out, nil
		}
		if seen[r.Next] {
			return nil, fmt.Errorf("repeated task page token")
		}
		seen[r.Next] = true
		next = r.Next
	}
}
func (c *Client) Share(ctx context.Context, id, pass, token, parent, page string) (Share, error) {
	var r Share
	path := "/drive/v1/share"
	q := url.Values{"share_id": {id}, "pass_code": {pass}, "parent_id": {parent}, "page_token": {page}, "limit": {"100"}, "thumbnail_size": {"SIZE_LARGE"}}
	if token != "" {
		path += "/detail"
		q.Del("pass_code")
		q.Set("pass_code_token", token)
	}
	e := c.drive(ctx, "GET", path, q, nil, &r)
	if e == nil && r.Status != "" && r.Status != "OK" {
		e = &APIError{Status: 400, Endpoint: path, Code: r.Status, Message: "share unavailable or extraction code incorrect"}
	}
	return r, e
}
func (c *Client) RestoreShare(ctx context.Context, id, token string, ids []string, parent string) (Transfer, error) {
	var r Transfer
	e := c.drive(ctx, "POST", "/drive/v1/share/restore", nil, map[string]any{
		"share_id": id, "pass_code_token": token, "file_ids": ids,
		"parent_id": parent, "specify_parent_id": true, "ancestor_ids": []string{},
		"params": map[string]string{"trace_file_ids": strings.Join(ids, ",")},
	}, &r)
	return r, e
}
func (c *Client) Task(ctx context.Context, id string) (Task, error) {
	var r Task
	e := c.drive(ctx, "GET", "/drive/v1/tasks/"+url.PathEscape(id), nil, nil, &r)
	return r, e
}
func (c *Client) Instant(ctx context.Context, parent string, f File) (Transfer, error) {
	var r Transfer
	e := c.drive(ctx, "POST", "/drive/v1/files", nil, map[string]any{"kind": "drive#file", "name": f.Name, "parent_id": parent, "hash": f.Hash, "size": int64(f.Size), "upload_type": "UPLOAD_TYPE_RESUMABLE", "resumable": map[string]string{"provider": "PROVIDER_ALIYUN"}}, &r)
	return r, e
}

package vault

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func publicIP(ip net.IP) bool {
	return ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified() && !ip.IsMulticast() && ip.IsGlobalUnicast() && !ip.Equal(net.ParseIP("169.254.169.254"))
}
func safeMediaURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("invalid media URL")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !publicIP(ip) {
		return fmt.Errorf("private media destination denied")
	}
	return nil
}
func mediaClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 32, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(addr)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil {
			return nil, e
		}
		for _, v := range ips {
			if !publicIP(v.IP) {
				return nil, fmt.Errorf("private media destination denied")
			}
		}
		var last error
		for _, v := range ips {
			d := net.Dialer{Timeout: 10 * time.Second}
			conn, e := d.DialContext(ctx, network, net.JoinHostPort(v.IP.String(), port))
			if e == nil {
				return conn, nil
			}
			last = e
		}
		if last == nil {
			last = fmt.Errorf("no media addresses")
		}
		return nil, last
	}}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return fmt.Errorf("too many media redirects")
		}
		return safeMediaURL(req.URL.String())
	}}
}
func (a *App) fetchMedia(ctx context.Context, method, raw, rangeHeader string) (*http.Response, error) {
	if e := safeMediaURL(raw); e != nil {
		return nil, fail(502, "PikPak returned an invalid media destination")
	}
	req, e := http.NewRequestWithContext(ctx, method, raw, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Referer", "https://mypikpak.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	client := a.MediaHTTP
	if client == nil {
		client = mediaClient()
	}
	res, e := client.Do(req)
	if e != nil {
		return nil, fail(502, "Unable to connect to the PikPak media endpoint")
	}
	return res, nil
}

type mediaPart struct {
	Node    string `json:"node"`
	Account string `json:"account"`
	URL     string `json:"url"`
	Expiry  int64  `json:"expiry"`
	Depth   int    `json:"depth"`
}

var uriAttribute = regexp.MustCompile(`URI="([^"]+)"`)

func (a *App) playlist(raw, base, node, account string, depth int) (string, error) {
	if depth > 4 {
		return "", fail(502, "Playlist nesting is too deep")
	}
	u, e := url.Parse(base)
	if e != nil {
		return "", e
	}
	rewrite := func(ref string) (string, error) {
		r, e := url.Parse(ref)
		if e != nil {
			return "", e
		}
		target := u.ResolveReference(r).String()
		if e = safeMediaURL(target); e != nil {
			return "", e
		}
		token, e := a.Store.Seal(mediaPart{node, account, target, now() + 1800, depth + 1})
		if e != nil {
			return "", e
		}
		return "/api/v1/files/" + node + "/content?proxy=1&part=" + url.QueryEscape(token), nil
	}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	lines := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			var inner error
			line = uriAttribute.ReplaceAllStringFunc(line, func(s string) string {
				parts := uriAttribute.FindStringSubmatch(s)
				v, e := rewrite(parts[1])
				if e != nil {
					inner = e
				}
				return `URI="` + v + `"`
			})
			if inner != nil {
				return "", inner
			}
		} else if strings.TrimSpace(line) != "" {
			line, e = rewrite(strings.TrimSpace(line))
			if e != nil {
				return "", e
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), scanner.Err()
}
func (a *App) content(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	account := a.active()
	n, e := a.Store.Node(id, account)
	if e != nil {
		return e
	}
	if n.Trashed || n.RemoteID == "" || n.Kind == "folder" {
		return fail(409, "Restore this file before opening it")
	}
	raw := ""
	depth := 0
	if token := r.URL.Query().Get("part"); token != "" {
		var part mediaPart
		if e = a.Store.Unseal(token, &part); e != nil || part.Node != id || part.Account != account || part.Expiry < now() {
			return fail(403, "Media segment link expired")
		}
		raw = part.URL
		depth = part.Depth
	} else {
		_, f, e := a.remoteFile(r.Context(), id)
		if e != nil {
			return e
		}
		raw = mediaURL(f, r.URL.Query().Get("media"))
	}
	if raw == "" {
		return fail(409, "No media link is available for this file")
	}
	if e = safeMediaURL(raw); e != nil {
		return fail(502, "PikPak returned an invalid media URL")
	}
	useProxy := r.URL.Query().Get("proxy") == "1" || (r.URL.Query().Get("proxy") == "" && a.Store.Get("proxy_default") == "true")
	if !useProxy {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, raw, 302)
		return nil
	}
	res, e := a.fetchMedia(r.Context(), r.Method, raw, r.Header.Get("Range"))
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if (res.StatusCode == 401 || res.StatusCode == 403) && r.URL.Query().Get("part") == "" {
		res.Body.Close()
		_, f, e := a.remoteFile(r.Context(), id)
		if e != nil {
			return e
		}
		raw = mediaURL(f, r.URL.Query().Get("media"))
		res, e = a.fetchMedia(r.Context(), r.Method, raw, r.Header.Get("Range"))
		if e != nil {
			return e
		}
		defer res.Body.Close()
	}
	if res.StatusCode != 200 && res.StatusCode != 206 && res.StatusCode != 416 {
		return fail(502, fmt.Sprintf("PikPak media endpoint returned HTTP %d", res.StatusCode))
	}
	ct := res.Header.Get("Content-Type")
	u, _ := url.Parse(raw)
	isPlaylist := strings.Contains(strings.ToLower(ct), "mpegurl") || strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
	if isPlaylist && r.Method != "HEAD" {
		b, e := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
		if e != nil {
			return e
		}
		if len(b) > 2<<20 {
			return fail(502, "Playlist is too large")
		}
		body, e := a.playlist(string(b), raw, id, account, depth)
		if e != nil {
			return fail(502, "Could not resolve the media playlist")
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, body)
		return nil
	}
	if r.URL.Query().Get("text") == "1" {
		b, e := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
		if e != nil {
			return e
		}
		if len(b) > 1<<20 {
			return fail(413, "Text preview is limited to 1 MiB; download this file instead")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write(b)
		return nil
	}
	for _, header := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"} {
		if value := res.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	if ct == "" {
		ct = n.Mime
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	disposition := "inline"
	lower := strings.ToLower(ct)
	if r.URL.Query().Get("download") == "1" || strings.Contains(lower, "html") || strings.Contains(lower, "svg") || strings.Contains(lower, "xml") {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": n.Name}))
	w.WriteHeader(res.StatusCode)
	if r.Method != "HEAD" {
		_, _ = io.Copy(w, res.Body)
	}
	return nil
}
func (a *App) thumbnail(w http.ResponseWriter, r *http.Request) error {
	account := a.active()
	q := r.URL.Query()
	if q.Get("account") != "" && q.Get("account") != account {
		return fail(409, "Thumbnail belongs to a different account")
	}
	n, e := a.Store.Node(r.PathValue("id"), account)
	if e != nil {
		return e
	}
	if n.Kind != "file" || n.Trashed || n.RemoteID == "" || (q.Get("remote") != "" && q.Get("remote") != n.RemoteID) || (n.State != "present" && n.State != "drift") {
		return fail(404, "Thumbnail file is not available")
	}
	if folder := q.Get("folder"); folder != "" && (a.Store.Get("folder_previews") != "true" || !a.previewAncestor(n, folder, account)) {
		return fail(404, "Folder preview is not available")
	}
	// Existing links usually remain valid for many requests. Refresh metadata
	// only when absent or when the CDN reports an expired/missing link.
	refresh := func() (string, error) {
		c, err := a.client(account)
		if err != nil {
			return "", err
		}
		f, err := c.Get(r.Context(), n.RemoteID)
		if err != nil {
			return "", err
		}
		if f.Trashed || !f.Complete() || !compatible(n, f) {
			return "", fail(404, "Thumbnail file has changed")
		}
		_, err = a.Store.DB.Exec(`UPDATE bindings SET thumbnail=? WHERE account_id=? AND node_id=? AND remote_id=?`, f.Thumbnail, account, n.ID, n.RemoteID)
		return f.Thumbnail, err
	}
	raw := n.Thumbnail
	fresh := false
	if raw == "" {
		raw, e = refresh()
		if e != nil {
			return e
		}
		fresh = true
	}
	if raw == "" {
		w.Header().Set("Cache-Control", "private, max-age=300")
		return fail(404, "No thumbnail")
	}
	res, e := a.fetchMedia(r.Context(), "GET", raw, "")
	if e != nil {
		return e
	}
	if !fresh && (res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 404 || res.StatusCode == 410) {
		res.Body.Close()
		raw, e = refresh()
		if e != nil {
			return e
		}
		if raw == "" {
			w.Header().Set("Cache-Control", "private, max-age=300")
			return fail(404, "No thumbnail")
		}
		res, e = a.fetchMedia(r.Context(), "GET", raw, "")
		if e != nil {
			return e
		}
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fail(502, "Thumbnail endpoint unavailable")
	}
	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") || strings.Contains(ct, "svg") {
		return fail(502, "Unexpected thumbnail type")
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (5<<20)+1))
	if e != nil {
		return e
	}
	if len(b) > 5<<20 {
		return fail(502, "Thumbnail is too large")
	}
	if a.active() != account || (q.Get("folder") != "" && a.Store.Get("folder_previews") != "true") {
		return fail(409, "Thumbnail settings or account changed")
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Vary", "Cookie")
	_, _ = w.Write(b)
	return nil
}

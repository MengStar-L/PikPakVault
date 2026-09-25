// Package rss reads RSS and Atom subscriptions without storing feed credentials
// or fetching the resources referenced by feed entries.
package rss

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxFeedBytes = 8 << 20
	maxEntries   = 2000
	fetchTimeout = 45 * time.Second
)

type Link struct {
	URL        string
	Attachment bool
}

type Entry struct {
	Key       string
	Title     string
	Published int64
	Links     []Link
}

type Result struct {
	Title        string
	Entries      []Entry
	ETag         string
	LastModified string
	NotModified  bool
}

// NormalizeURL accepts administrator-configured HTTP(S) feeds, including
// private feeds on a local network. Query strings may contain feed tokens and
// must never be included in logs or returned errors.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if u != nil {
		u.Scheme = strings.ToLower(u.Scheme)
	}
	if err != nil || u == nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("RSS 地址必须是 HTTP 或 HTTPS 链接，且不能包含用户名和密码")
	}
	if port := u.Port(); port != "" {
		var number int
		if _, err := fmt.Sscanf(port, "%d", &number); err != nil || number < 1 || number > 65535 {
			return "", errors.New("RSS 地址端口无效")
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// Fetch retrieves and validates the complete document before returning entries.
// It deliberately sanitizes transport and XML errors: both can otherwise echo
// private feed URLs or authentication tokens from the response body.
func Fetch(ctx context.Context, client *http.Client, rawURL, etag, modified string) (Result, error) {
	feedURL, err := NormalizeURL(rawURL)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if client == nil {
		client = http.DefaultClient
	}
	local := *client
	previousRedirect := local.CheckRedirect
	local.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("RSS 重定向次数过多")
		}
		if _, err := NormalizeURL(req.URL.String()); err != nil {
			return errors.New("RSS 重定向地址无效")
		}
		if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
			for _, name := range []string{"Authorization", "Cookie", "If-None-Match", "If-Modified-Since"} {
				req.Header.Del(name)
			}
			// Go's Referer includes the previous URL query, which can contain
			// private tracker passkeys. Never send it to redirect destinations.
		}
		req.Header.Del("Referer")
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return Result{}, errors.New("无法创建 RSS 请求")
	}
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/rdf+xml, application/xml, text/xml;q=0.9")
	req.Header.Set("User-Agent", "PikPakVault-RSS/1.0")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if modified != "" {
		req.Header.Set("If-Modified-Since", modified)
	}
	resp, err := local.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("RSS 请求超时: %w", context.DeadlineExceeded)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return Result{}, fmt.Errorf("RSS 请求已取消: %w", context.Canceled)
		}
		return Result{}, errors.New("RSS 请求失败，请检查地址及网络连接")
	}
	defer resp.Body.Close()
	result := Result{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	if resp.StatusCode == http.StatusNotModified {
		result.NotModified = true
		if result.ETag == "" {
			result.ETag = etag
		}
		if result.LastModified == "" {
			result.LastModified = modified
		}
		return result, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("RSS 服务返回 HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxFeedBytes {
		return Result{}, errors.New("RSS 内容超过 8 MiB 限制")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return Result{}, errors.New("RSS 内容读取中断，请重试")
	}
	if len(data) > maxFeedBytes {
		return Result{}, errors.New("RSS 内容超过 8 MiB 限制")
	}
	parsed, err := parse(data, resp.Request.URL)
	if err != nil {
		return Result{}, err
	}
	parsed.ETag, parsed.LastModified = result.ETag, result.LastModified
	return parsed, nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

type element struct {
	name     xml.Name
	attrs    []xml.Attr
	text     strings.Builder
	children []*element
}

func (e *element) attr(name string) string {
	for _, attr := range e.attrs {
		if attr.Name.Local == name {
			return strings.TrimSpace(attr.Value)
		}
	}
	return ""
}

func (e *element) value() string {
	var value strings.Builder
	value.WriteString(e.text.String())
	for _, child := range e.children {
		value.WriteByte(' ')
		value.WriteString(child.value())
	}
	return strings.TrimSpace(value.String())
}

func (e *element) first(name string) *element {
	for _, child := range e.children {
		if child.name.Local == name {
			return child
		}
	}
	return nil
}

func (e *element) field(names ...string) string {
	for _, name := range names {
		if child := e.first(name); child != nil {
			if value := child.value(); value != "" {
				return value
			}
		}
	}
	return ""
}

func decode(data []byte) (*element, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root *element
	var stack []*element
	nodes := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("RSS XML 不完整或格式无效")
		}
		switch token := token.(type) {
		case xml.StartElement:
			nodes++
			if len(stack) >= 64 || nodes > 100000 {
				return nil, errors.New("RSS XML 结构过于复杂")
			}
			node := &element{name: token.Name, attrs: token.Attr}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("RSS XML 包含多个根节点")
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}
			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(token)
			} else if len(bytes.TrimSpace(token)) != 0 {
				return nil, errors.New("RSS XML 包含无效的文档内容")
			}
		case xml.Directive:
			return nil, errors.New("RSS XML 不支持 DTD 或外部实体")
		}
	}
	if root == nil || len(stack) != 0 {
		return nil, errors.New("RSS XML 为空或不完整")
	}
	return root, nil
}

func parse(data []byte, base *url.URL) (Result, error) {
	root, err := decode(data)
	if err != nil {
		return Result{}, err
	}
	container, entryName := root, "item"
	switch root.name.Local {
	case "rss":
		container = root.first("channel")
		if container == nil {
			return Result{}, errors.New("RSS 缺少 channel 节点")
		}
	case "feed":
		entryName = "entry"
	case "RDF":
	default:
		return Result{}, errors.New("该地址未返回有效的 RSS 或 Atom 订阅")
	}
	result := Result{Title: plainTitle(container.field("title")), Entries: []Entry{}}
	if root.name.Local == "RDF" {
		if channel := root.first("channel"); channel != nil {
			result.Title = plainTitle(channel.field("title"))
		}
	}
	base = elementBase(base, root)
	if container != root {
		base = elementBase(base, container)
	}
	seen, count := map[string]int{}, 0
	for _, item := range container.children {
		if item.name.Local != entryName {
			continue
		}
		count++
		if count > maxEntries {
			return Result{}, errors.New("RSS 条目超过 2000 条限制")
		}
		entry := parseEntry(item, elementBase(base, item))
		if index, found := seen[entry.Key]; found {
			result.Entries[index].Links = orderedLinks(append(result.Entries[index].Links, entry.Links...))
			continue
		}
		seen[entry.Key] = len(result.Entries)
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}

func elementBase(parent *url.URL, node *element) *url.URL {
	for _, attr := range node.attrs {
		if attr.Name.Space == "http://www.w3.org/XML/1998/namespace" && attr.Name.Local == "base" {
			if resolved := resolveLink(attr.Value, parent); resolved != "" && !strings.HasPrefix(resolved, "magnet:") {
				base, _ := url.Parse(resolved)
				return base
			}
		}
	}
	return parent
}

func parseEntry(item *element, base *url.URL) Entry {
	date := item.field("pubDate", "published", "date", "updated")
	entry := Entry{Title: plainTitle(item.field("title")), Published: parseDate(date), Links: []Link{}}
	identity := item.field("guid", "id")
	var canonical string
	var walk func(*element, *url.URL)
	add := func(raw string, attachment bool, base *url.URL) {
		if link := resolveLink(raw, base); link != "" {
			entry.Links = append(entry.Links, Link{URL: link, Attachment: attachment})
		}
	}
	walk = func(node *element, parent *url.URL) {
		currentBase := elementBase(parent, node)
		switch node.name.Local {
		case "link":
			raw := node.attr("href")
			if raw == "" {
				raw = node.value()
			}
			rel := strings.ToLower(node.attr("rel"))
			if rel == "" || rel == "alternate" || rel == "enclosure" {
				add(raw, rel == "enclosure", currentBase)
				if canonical == "" && rel != "enclosure" {
					canonical = resolveLink(raw, currentBase)
				}
			}
		case "enclosure":
			add(node.attr("url"), true, currentBase)
		case "content":
			add(node.attr("url"), true, currentBase) // media:content
			add(node.attr("src"), true, currentBase) // Atom external content
		case "magnetURI", "magnet", "magneturi":
			add(node.value(), false, currentBase)
		}
		if node.name.Local == "description" || node.name.Local == "summary" || node.name.Local == "content" || node.name.Local == "encoded" {
			for _, raw := range textLinks(node.value()) {
				add(raw, false, currentBase)
			}
		}
		if node.name.Local == "a" {
			add(node.attr("href"), false, currentBase)
		}
		for _, child := range node.children {
			walk(child, currentBase)
		}
	}
	for _, child := range item.children {
		walk(child, base)
	}
	// Some torrent feeds use a magnet as the GUID with no separate link.
	if strings.HasPrefix(strings.ToLower(identity), "magnet:") {
		add(identity, false, base)
	}
	entry.Links = orderedLinks(entry.Links)
	keyValue := "id:" + identity
	if identity == "" {
		if canonical == "" && len(entry.Links) > 0 {
			canonical = entry.Links[0].URL
		}
		if canonical != "" {
			keyValue = "link:" + canonicalKeyLink(canonical)
		} else {
			keyValue = "title-date:" + entry.Title + "\x00" + date
		}
	}
	digest := sha256.Sum256([]byte(keyValue))
	entry.Key = hex.EncodeToString(digest[:])
	return entry
}

var (
	urlPattern  = regexp.MustCompile(`(?i)(?:magnet:\?|https?://)[^\s<>"'` + "`" + `]+`)
	hrefPattern = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]+)"|'([^']+)')`)
	tagPattern  = regexp.MustCompile(`<[^>]*>`)
)

func textLinks(value string) []string {
	value = html.UnescapeString(value)
	links := urlPattern.FindAllString(value, -1)
	for _, match := range hrefPattern.FindAllStringSubmatch(value, -1) {
		if match[1] != "" {
			links = append(links, match[1])
		} else {
			links = append(links, match[2])
		}
	}
	return links
}

func resolveLink(raw string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n\t") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return ""
	}
	if strings.EqualFold(u.Scheme, "magnet") {
		u.Scheme = "magnet"
		if u.RawQuery == "" || u.Host != "" || u.Opaque != "" {
			return ""
		}
		u.Fragment = ""
		return u.String()
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	result, err := NormalizeURL(u.String())
	if err != nil {
		return ""
	}
	return result
}

func orderedLinks(links []Link) []Link {
	result, seen := []Link{}, map[string]int{}
	for _, link := range links {
		if index, found := seen[link.URL]; found {
			result[index].Attachment = result[index].Attachment || link.Attachment
			continue
		}
		seen[link.URL] = len(result)
		result = append(result, link)
	}
	sort.SliceStable(result, func(i, j int) bool { return linkPriority(result[i]) < linkPriority(result[j]) })
	return result
}

func linkPriority(link Link) int {
	if strings.HasPrefix(link.URL, "magnet:") {
		return 0
	}
	u, _ := url.Parse(link.URL)
	if u != nil {
		host := strings.ToLower(u.Hostname())
		if host == "mypikpak.com" || strings.HasSuffix(host, ".mypikpak.com") || host == "mypikpak.net" || strings.HasSuffix(host, ".mypikpak.net") || host == "pikpak.me" || strings.HasSuffix(host, ".pikpak.me") {
			return 1
		}
	}
	if link.Attachment {
		return 2
	}
	return 3
}

func canonicalKeyLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = u.Query().Encode()
	return u.String()
}

func plainTitle(raw string) string {
	return strings.TrimSpace(html.UnescapeString(tagPattern.ReplaceAllString(raw, "")))
}

func parseDate(raw string) int64 {
	for _, layout := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC850, time.ANSIC, "Mon, 2 Jan 2006 15:04:05 -0700", "2006-01-02T15:04:05-0700", "2006-01-02 15:04:05 -0700", "2006-01-02"} {
		if date, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return date.Unix()
		}
	}
	return 0
}

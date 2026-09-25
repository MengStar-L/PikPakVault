package rss

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testMagnet = "magnet:?xt=urn:btih:ABCDEFGHIJKLMNOPQRSTUVWXYZ234567&dn=Example&tr=https%3A%2F%2Ftracker.example%2Fannounce"

func readTestFeed(t *testing.T, document string) Result {
	t.Helper()
	base, _ := url.Parse("https://feeds.example/subscriptions/feed.xml?private=secret")
	result, err := parse([]byte(document), base)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRSSMagnetEnclosure(t *testing.T) {
	// Animes Garden uses an ordinary article GUID/link and a magnet enclosure.
	result := readTestFeed(t, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>动画订阅</title><item>
<title><![CDATA[[Example] Episode 01 [1080p]]]></title>
<link>https://articles.example/view/123</link><guid isPermaLink="true">https://articles.example/view/123</guid>
<pubDate>Fri, 25 Sep 2026 10:00:00 +0000</pubDate>
<enclosure url="`+strings.ReplaceAll(testMagnet, "&", "&amp;")+`" type="application/x-bittorrent" length="0"/>
</item></channel></rss>`)
	if result.Title != "动画订阅" || len(result.Entries) != 1 {
		t.Fatalf("bad feed: %#v", result)
	}
	entry := result.Entries[0]
	if entry.Title != "[Example] Episode 01 [1080p]" || entry.Published == 0 || len(entry.Key) != 64 {
		t.Fatalf("bad entry: %#v", entry)
	}
	if len(entry.Links) != 2 || entry.Links[0].URL != testMagnet || !entry.Links[0].Attachment || entry.Links[1].Attachment {
		t.Fatalf("magnet must be preferred to article: %#v", entry.Links)
	}
}

func TestNamespacedMagnetsCDATAAndDuplicates(t *testing.T) {
	result := readTestFeed(t, `<rss version="2.0" xmlns:torrent="http://xmlns.ezrss.it/0.1/" xmlns:media="http://search.yahoo.com/mrss/"><channel><title>Example</title>
<item><guid>stable-file-id</guid><title>Example</title><link>https://articles.example/post</link>
<description><![CDATA[<p>Download <a href="magnet:?xt=urn:btih:ABC&amp;dn=Name">magnet</a> https://mypikpak.com/s/example</p>]]></description>
<torrent:magnetURI>magnet:?xt=urn:btih:ABC&amp;dn=Name</torrent:magnetURI>
<media:content url="../files/video.mp4?a=1&amp;not=2" type="video/mp4"/></item>
<item><guid>stable-file-id</guid><title>Duplicate title</title><enclosure url="../files/video.mp4?a=1&amp;not=2"/></item>
</channel></rss>`)
	if len(result.Entries) != 1 {
		t.Fatalf("duplicate GUID was not merged: %#v", result.Entries)
	}
	links := result.Entries[0].Links
	want := []string{"magnet:?xt=urn:btih:ABC&dn=Name", "https://mypikpak.com/s/example", "https://feeds.example/files/video.mp4?a=1&not=2", "https://articles.example/post"}
	if len(links) != len(want) {
		t.Fatalf("links: %#v", links)
	}
	for index := range want {
		if links[index].URL != want[index] {
			t.Fatalf("link %d: got %q, want %q", index, links[index].URL, want[index])
		}
	}
	if !links[2].Attachment {
		t.Fatal("media content must be an attachment")
	}
}

func TestAtomRelativeLinksXMLBaseAndXHTML(t *testing.T) {
	result := readTestFeed(t, `<feed xmlns="http://www.w3.org/2005/Atom" xml:base="https://media.example/base/"><title>Atom</title>
<entry xml:base="series/"><id>urn:entry:1</id><title type="html">An &lt;b&gt;episode&lt;/b&gt;</title><updated>2026-09-25T12:00:00Z</updated>
<link rel="alternate" href="./episode"/><link rel="enclosure" href="../video.mp4?a=1&amp;b=2"/>
<link rel="self" href="metadata.xml"/>
<summary type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><a href="magnet:?xt=urn:btih:ABC">Get file</a></div></summary>
<content src="subtitles.vtt"/>
</entry></feed>`)
	entry := result.Entries[0]
	if entry.Title != "An episode" || entry.Published == 0 {
		t.Fatalf("bad metadata: %#v", entry)
	}
	want := []string{"magnet:?xt=urn:btih:ABC", "https://media.example/base/video.mp4?a=1&b=2", "https://media.example/base/series/subtitles.vtt", "https://media.example/base/series/episode"}
	if len(entry.Links) != len(want) {
		t.Fatalf("bad links: %#v", entry.Links)
	}
	for index := range want {
		if entry.Links[index].URL != want[index] {
			t.Errorf("link %d: got %q, want %q", index, entry.Links[index].URL, want[index])
		}
	}
}

func TestRSS1RDF(t *testing.T) {
	result := readTestFeed(t, `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel rdf:about="https://example.org/feed"><title>RSS 1</title></channel>
<item rdf:about="https://example.org/a"><title>File</title><link>magnet:?xt=urn:btih:ABC</link><dc:date>2026-09-25T00:00:00Z</dc:date></item></rdf:RDF>`)
	if result.Title != "RSS 1" || len(result.Entries) != 1 || result.Entries[0].Published == 0 || result.Entries[0].Links[0].URL != "magnet:?xt=urn:btih:ABC" {
		t.Fatalf("RDF parsing failed: %#v", result)
	}
}

func TestStableIdentity(t *testing.T) {
	for _, test := range []struct{ name, first, second string }{
		{"guid survives title and enclosure change", `<guid>same</guid><title>Old</title><enclosure url="https://cdn.example/a?token=1"/>`, `<guid>same</guid><title>New</title><enclosure url="https://cdn.example/a?token=2"/>`},
		{"article survives enclosure change", `<link>https://example.org/article?a=1&amp;b=2</link><enclosure url="https://cdn.example/a?token=1"/>`, `<link>https://example.org/article?b=2&amp;a=1</link><enclosure url="https://cdn.example/a?token=2"/>`},
		{"title and date fallback", `<title>Same</title><pubDate>Fri, 25 Sep 2026 00:00:00 GMT</pubDate><description>Old text</description>`, `<title>Same</title><pubDate>Fri, 25 Sep 2026 00:00:00 GMT</pubDate><description>New text</description>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := readTestFeed(t, "<rss><channel><item>"+test.first+"</item></channel></rss>")
			second := readTestFeed(t, "<rss><channel><item>"+test.second+"</item></channel></rss>")
			if first.Entries[0].Key != second.Entries[0].Key {
				t.Fatal("entry identity changed")
			}
		})
	}
}

func TestMalformedFeedNeverReturnsPartialEntries(t *testing.T) {
	for _, document := range []string{
		`<rss><channel><item><guid>1</guid><link>magnet:?xt=urn:btih:ABC</link></item><item>`,
		`<rss><channel/></rss><feed/>`,
		`<rss><channel/></rss>trailing garbage`,
		`<html><body><item>this is not a feed</item></body></html>`,
		`<!DOCTYPE rss [<!ENTITY secret SYSTEM "file:///etc/passwd">]><rss><channel><title>&secret;</title></channel></rss>`,
		`<rss><channel><title>&private-token;</title></channel></rss>`,
		`<rss><channel><item><guid>1</guid></item><title>&</title></channel></rss>`,
		`<rss/>`,
		``,
		strings.Repeat("<a>", 65) + strings.Repeat("</a>", 65),
	} {
		result, err := parse([]byte(document), nil)
		if err == nil || len(result.Entries) != 0 {
			t.Fatalf("malformed XML returned entries or no error: %#v / %v", result, err)
		}
		if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "etc/passwd") {
			t.Fatal("parse error leaked response content")
		}
	}
}

func TestTooManyEntriesRejected(t *testing.T) {
	var document strings.Builder
	document.WriteString("<rss><channel>")
	for index := 0; index <= maxEntries; index++ {
		fmt.Fprintf(&document, "<item><guid>%d</guid></item>", index)
	}
	document.WriteString("</channel></rss>")
	result, err := parse([]byte(document.String()), nil)
	if err == nil || len(result.Entries) != 0 {
		t.Fatalf("over-limit feed returned partial entries: %#v / %v", result, err)
	}
}

func TestNormalizeURL(t *testing.T) {
	value, err := NormalizeURL("  HTTPS://Example.ORG/feed?passkey=secret#section ")
	if err != nil || value != "https://example.org/feed?passkey=secret" {
		t.Fatalf("unexpected normalization: %q %v", value, err)
	}
	for _, raw := range []string{"", "feed://example.org/file", "file:///etc/passwd", "https://user:password@example.org/feed", "https:///feed", "https://example.org:65536/feed", "https://example.org:invalid/feed", "https://example.org/%XX?secret-token"} {
		_, err := NormalizeURL(raw)
		if err == nil {
			t.Fatalf("invalid URL accepted: %q", raw)
		}
		if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "password") {
			t.Fatal("error leaked URL credentials")
		}
	}
	for _, raw := range []string{"http://localhost/feed", "http://127.0.0.1:8080/feed", "http://[::1]:8080/feed"} {
		if _, err := NormalizeURL(raw); err != nil {
			t.Fatalf("self-hosted administrator feed rejected: %v", err)
		}
	}
}

func TestFetchConditionalRequestAndNotModified(t *testing.T) {
	const modified = "Fri, 25 Sep 2026 00:00:00 GMT"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("ETag", `"version-1"`)
			w.Header().Set("Last-Modified", modified)
			io.WriteString(w, `<rss><channel><title>Example</title><item><guid>1</guid><enclosure url="file.mp4"/></item></channel></rss>`)
			return
		}
		if r.Header.Get("If-None-Match") != `"version-1"` || r.Header.Get("If-Modified-Since") != modified {
			t.Error("conditional request headers missing")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	first, err := Fetch(context.Background(), server.Client(), server.URL+"/feed", "", "")
	if err != nil || first.Entries[0].Links[0].URL != server.URL+"/file.mp4" {
		t.Fatalf("initial feed failed: %#v %v", first, err)
	}
	second, err := Fetch(context.Background(), server.Client(), server.URL+"/feed", first.ETag, first.LastModified)
	if err != nil || !second.NotModified || second.ETag != first.ETag || second.LastModified != modified || len(second.Entries) != 0 {
		t.Fatalf("conditional feed failed: %#v %v", second, err)
	}
}

func TestFetchRedirectResolvesFinalURLAndHidesTokens(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, header := range []string{"Referer", "If-None-Match", "If-Modified-Since"} {
			if r.Header.Get(header) != "" {
				t.Errorf("redirect forwarded %s", header)
			}
		}
		if r.URL.RawQuery != "" {
			t.Error("feed query was copied to redirect destination")
		}
		io.WriteString(w, `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>1</id><link rel="enclosure" href="video.mp4"/></entry></feed>`)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/final/feed", http.StatusFound)
	}))
	defer origin.Close()
	result, err := Fetch(context.Background(), origin.Client(), origin.URL+"/feed?private=secret", `"etag"`, "old")
	if err != nil || result.Entries[0].Links[0].URL != destination.URL+"/final/video.mp4" {
		t.Fatalf("redirect feed failed: %#v %v", result, err)
	}
}

func TestFetchRedirectLoopAndUnsafeSchemeAreSanitized(t *testing.T) {
	for _, target := range []string{"/loop?private=secret-token", "file:///secret-token", "http://user:secret-token@example.org/feed"} {
		t.Run(target, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target)
				w.WriteHeader(http.StatusFound)
			}))
			defer server.Close()
			_, err := Fetch(context.Background(), server.Client(), server.URL+"/feed?secret-token", "", "")
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("unsafe redirect accepted or leaked private URL: %v", err)
			}
		})
	}
}

func TestFetchBodyLimitAndHTTPError(t *testing.T) {
	for _, test := range []struct {
		name string
		run  http.HandlerFunc
	}{
		{"oversized content length", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(maxFeedBytes+1))
			w.WriteHeader(http.StatusOK)
		}},
		{"oversized streamed body", func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			io.WriteString(w, strings.Repeat(" ", maxFeedBytes+1))
		}},
		{"error response body", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "secret-token")
		}},
		{"HTML authentication page", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "<html><body>secret-token</body></html>")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.run)
			defer server.Close()
			result, err := Fetch(context.Background(), server.Client(), server.URL+"/?secret-token", "", "")
			if err == nil || len(result.Entries) != 0 || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("bad failure handling: %#v %v", result, err)
			}
		})
	}
}

func TestFetchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Fetch(ctx, nil, "https://example.org/feed?secret-token", "", "")
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("context cancellation lost or URL leaked: %v", err)
	}
}

func TestUnsupportedAndPlainTextLinks(t *testing.T) {
	result := readTestFeed(t, `<rss><channel><item><title>Links</title><description><![CDATA[
Plain magnet:?xt=urn:btih:ABC&dn=Sample
<a href="/file?a=1&amp;b=2">File</a><a href="javascript:alert(1)">Bad</a>
<a href="file:///etc/passwd">Bad</a><a href="https://user:password@example.org/file">Bad</a>
]]></description></item><item><title>Text only</title></item></channel></rss>`)
	if len(result.Entries) != 2 || len(result.Entries[0].Links) != 2 || len(result.Entries[1].Links) != 0 {
		t.Fatalf("unexpected links: %#v", result.Entries)
	}
	for _, link := range result.Entries[0].Links {
		if link.Attachment {
			t.Fatal("ordinary text links must not be marked as downloads")
		}
	}
}

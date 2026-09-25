package vault

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pikpakvault/internal/pikpak"
	"pikpakvault/internal/rss"
)

func rssItem(id, title, link string) string {
	return `<item><guid>` + html.EscapeString(id) + `</guid><title>` + html.EscapeString(title) + `</title><link>https://example.test/details/` + id + `</link><enclosure type="application/x-bittorrent" url="` + html.EscapeString(link) + `"/></item>`
}
func rssDocument(items ...string) string {
	return `<rss version="2.0"><channel><title>RSS test</title>` + strings.Join(items, "") + `</channel></rss>`
}
func newRSSTestSubscription(t *testing.T, a *App, feed string, existing bool) string {
	t.Helper()
	w := request(t, a.Handler(nil), "POST", "/api/v1/rss", map[string]any{"name": "RSS 测试", "url": feed, "parent_id": "root", "interval_minutes": 5, "enabled": true, "import_existing": existing}, "test-csrf")
	if w.Code != 200 {
		t.Fatalf("create subscription: %d %s", w.Code, w.Body.String())
	}
	var v struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.ID
}
func runRSSTestScan(t *testing.T, a *App, id string) Job {
	t.Helper()
	j, err := a.newRSSCheck(id, a.active(), true)
	if err != nil {
		t.Fatal(err)
	}
	return execute(t, a, &j)
}
func rssImportJobs(t *testing.T, a *App, id string) []Job {
	t.Helper()
	rows, err := a.Store.DB.Query(`SELECT `+jobCols+` FROM jobs WHERE kind='import' AND json_extract(data,'$.rss_id')=? ORDER BY created,id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, e := jobScan(rows)
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, j)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestRSSBaselineNewItemsDedupeMoveRestartAndAccountSwitch(t *testing.T) {
	a, f := testApp(t)
	sessionForTest(t, a)
	magnetA := "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)
	magnetB := "magnet:?xt=urn:btih:" + strings.Repeat("b", 40)
	var body atomic.Value
	body.Store(rssDocument(rssItem("old", "Old episode", magnetA)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body.Load().(string)) }))
	defer srv.Close()
	id := newRSSTestSubscription(t, a, srv.URL, false)
	requireComplete(t, runRSSTestScan(t, a, id))
	if len(rssImportJobs(t, a, id)) != 0 {
		t.Fatal("baseline downloaded history")
	}
	body.Store(rssDocument(rssItem("old", "Old episode renamed", magnetA), rssItem("new", "New episode", magnetB)))
	requireComplete(t, runRSSTestScan(t, a, id))
	jobs := rssImportJobs(t, a, id)
	if len(jobs) != 1 {
		t.Fatalf("want one new import, got %d", len(jobs))
	}
	f.outputs = []RemoteEntry{{Path: "Episode.mkv", File: pikpak.File{Name: "Episode.mkv", Kind: "drive#file", Phase: "PHASE_TYPE_COMPLETE", Size: 42, Hash: "test-hash"}}}
	requireComplete(t, execute(t, a, &jobs[0]))
	nodes, err := a.Store.AllNodes("a")
	if err != nil || len(nodes) != 1 {
		t.Fatalf("import result: %v %+v", err, nodes)
	}
	if err = a.Store.InsertNode(Node{ID: "archive", ParentID: "root", Name: "归档", Kind: "folder"}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Store.DB.Exec(`UPDATE nodes SET parent_id='archive',name='已整理.mkv',favorite=1,position=123 WHERE id=?`, nodes[0].ID); err != nil {
		t.Fatal(err)
	}
	// A changed GUID, tracker and display name still identify the same torrent.
	binaryHash := strings.Repeat("\xbb", 20)
	base32Hash := base32.StdEncoding.EncodeToString([]byte(binaryHash))
	body.Store(rssDocument(rssItem("different-guid", "Republished", "magnet:?xt=urn:btih:"+base32Hash+"&dn=Other&tr=https%3A%2F%2Ftracker.test")))
	requireComplete(t, runRSSTestScan(t, a, id))
	// Reopen the database with a fresh App, then switch accounts: prior entries do
	// not become new downloads simply because the original file moved or switched.
	dir := a.Store.Dir
	if err = a.Store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	a = NewApp(s)
	a.Factory = func(Account) (pikpak.Provider, error) { return f, nil }
	addAccount(t, a, "b", "user-b")
	if err = s.Set("active_account", "b"); err != nil {
		t.Fatal(err)
	}
	requireComplete(t, runRSSTestScan(t, a, id))
	if len(rssImportJobs(t, a, id)) != 1 || f.calls["offline"] != 1 {
		t.Fatal("duplicate import after move/restart/account switch")
	}
	n, err := s.Node(nodes[0].ID, "a")
	if err != nil || n.ParentID != "archive" || n.Name != "已整理.mkv" || !n.Favorite || n.Position != 123 {
		t.Fatalf("local intent lost: %+v %v", n, err)
	}
}

func TestRSSAttachmentLostResponseAndPikPakShareSelection(t *testing.T) {
	a, f := testApp(t)
	sessionForTest(t, a)
	direct := "https://cdn.example.test/episode.mp4?signature=keep-me"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, rssDocument(rssItem("file", "HTTP attachment", direct)))
	}))
	defer srv.Close()
	id := newRSSTestSubscription(t, a, srv.URL, true)
	requireComplete(t, runRSSTestScan(t, a, id))
	jobs := rssImportJobs(t, a, id)
	if len(jobs) != 1 {
		t.Fatal("attachment not scheduled")
	}
	f.outputs = []RemoteEntry{{Path: "episode.mp4", File: pikpak.File{Name: "episode.mp4", Kind: "drive#file", Phase: "PHASE_TYPE_COMPLETE", Size: 8, Hash: "http-hash"}}}
	f.lostOffline = true
	j := execute(t, a, &jobs[0])
	if j.State != "retry" {
		t.Fatalf("lost response should retry: %+v", j)
	}
	requireComplete(t, execute(t, a, &j))
	if f.calls["offline"] != 1 || f.tasks[0].Params.URL != direct {
		t.Fatal("HTTP attachment retried as duplicate or lost signature")
	}
	list := request(t, a.Handler(nil), "GET", "/api/v1/rss/"+id+"/entries?page=0&limit=1", nil, "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"state":"saved"`) || !strings.Contains(list.Body.String(), jobs[0].ID) {
		t.Fatal("RSS result did not follow the original import job", list.Body.String())
	}
	var data JobData
	json.Unmarshal(jobs[0].Data, &data)
	updated := "https://cdn.example.test/episode.mp4?signature=refreshed"
	if w := request(t, a.Handler(nil), "PUT", "/api/v1/sources/"+data.SourceID, map[string]string{"link": updated}, "test-csrf"); w.Code != 200 {
		t.Fatal("cannot refresh attachment recovery source", w.Body.String())
	}
	if w := request(t, a.Handler(nil), "DELETE", "/api/v1/rss/"+id, nil, "test-csrf"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if nodes, _ := a.Store.AllNodes("a"); len(nodes) != 1 {
		t.Fatal("removing subscription deleted saved resources")
	}
	if saved, err := a.Store.Source(data.SourceID); err != nil || saved.Link != updated || len(saved.Manifest) != 1 {
		t.Fatal("removing subscription lost recovery source")
	}
	if job, err := a.Store.Job(jobs[0].ID); err != nil || job.State != "completed" {
		t.Fatal("removing subscription changed existing import")
	}
	source, pass, key := rssSource(rss.Entry{Links: []rss.Link{{URL: "https://mypikpak.com/s/shared?pwd=private-code"}}})
	if source.Kind != "share" || source.ShareID != "shared" || strings.Contains(source.Link, "private-code") || pass != "private-code" || key == "" {
		t.Fatal("PikPak source extraction failed")
	}
	_, _, key = rssSource(rss.Entry{Links: []rss.Link{{URL: "https://example.test/article"}}})
	if key != "" {
		t.Fatal("article page treated as downloadable file")
	}
}

func TestRSSMalformedFeedDoesNotAdvanceBaselineAndConditionalRequests(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("d", 40)
	var valid atomic.Bool
	var conditional atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !valid.Load() {
			fmt.Fprint(w, `<rss><channel>`+rssItem("one", "one", magnet))
			return
		}
		if r.Header.Get("If-None-Match") == `"feed-v1"` {
			conditional.Store(true)
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"feed-v1"`)
		fmt.Fprint(w, rssDocument(rssItem("one", "one", magnet)))
	}))
	defer srv.Close()
	id := newRSSTestSubscription(t, a, srv.URL, true)
	j := runRSSTestScan(t, a, id)
	if j.State != "failed" {
		t.Fatalf("truncated feed accepted: %+v", j)
	}
	s, _ := a.Store.rssSubscription(id)
	if s.Initialized || s.ETag != "" || s.LastError == "" || len(rssImportJobs(t, a, id)) != 0 {
		t.Fatalf("partial feed committed: %+v", s)
	}
	valid.Store(true)
	requireComplete(t, runRSSTestScan(t, a, id))
	requireComplete(t, runRSSTestScan(t, a, id))
	if !conditional.Load() || len(rssImportJobs(t, a, id)) != 1 {
		t.Fatal("conditional fetch or dedupe broken")
	}
	s, _ = a.Store.rssSubscription(id)
	if s.LastError != "" || !s.Initialized {
		t.Fatal("success did not clear failure")
	}
}

func TestRSSSchedulerAPIAndDeletedTarget(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	h := a.Handler(nil)
	id := newRSSTestSubscription(t, a, "https://example.test/feed?token=private-token", false)
	if w := request(t, h, "PATCH", "/api/v1/rss/"+id, map[string]bool{"enabled": false}, ""); w.Code != 403 {
		t.Fatal("mutation without CSRF allowed")
	}
	for _, payload := range []map[string]any{{"interval_minutes": 1}, {"url": "https://different.test/feed"}, {"parent_id": "missing"}} {
		if w := request(t, h, "PATCH", "/api/v1/rss/"+id, payload, "test-csrf"); w.Code < 400 {
			t.Fatalf("invalid config accepted: %+v", payload)
		}
	}
	a.scheduleRSS()
	a.scheduleRSS()
	var count int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='rss_scan'`).Scan(&count)
	if count != 1 {
		t.Fatal("duplicate scheduled scans")
	}
	if w := request(t, h, "PATCH", "/api/v1/rss/"+id, map[string]bool{"enabled": false}, "test-csrf"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a.scheduleRSS()
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='rss_scan' AND state='queued'`).Scan(&count)
	if count != 0 {
		t.Fatal("paused subscription remained scheduled")
	}
	if w := request(t, h, "POST", "/api/v1/rss/"+id+"/check", map[string]any{}, "test-csrf"); w.Code != 202 {
		t.Fatal("manual paused check unavailable", w.Body.String())
	}
	if w := request(t, h, "GET", "/api/v1/rss", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"subscriptions"`) {
		t.Fatal(w.Body.String())
	}
	var secret string
	a.Store.DB.QueryRow(`SELECT secret FROM rss_subscriptions WHERE id=?`, id).Scan(&secret)
	if strings.Contains(secret, "private-token") {
		t.Fatal("RSS URL not encrypted")
	}
	if w := request(t, h, "DELETE", "/api/v1/rss/"+id, nil, "test-csrf"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='rss_scan' AND state IN ('queued','running')`).Scan(&count)
	if count != 0 {
		t.Fatal("deleted rule still active")
	}
}

func TestRSSInFlightChangesCannotCreateObsoleteImports(t *testing.T) {
	for _, change := range []string{"pause", "delete", "switch", "target", "new-target"} {
		t.Run(change, func(t *testing.T) {
			a, _ := testApp(t)
			sessionForTest(t, a)
			entered, release := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				fmt.Fprint(w, rssDocument(rssItem("new", "New", "magnet:?xt=urn:btih:"+strings.Repeat("e", 40))))
			}))
			defer srv.Close()
			id := newRSSTestSubscription(t, a, srv.URL, true)
			j, err := a.newRSSCheck(id, "a", false)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { a.Execute(context.Background(), &j); close(done) }()
			<-entered
			switch change {
			case "pause":
				request(t, a.Handler(nil), "PATCH", "/api/v1/rss/"+id, map[string]bool{"enabled": false}, "test-csrf")
			case "delete":
				request(t, a.Handler(nil), "DELETE", "/api/v1/rss/"+id, nil, "test-csrf")
			case "switch":
				a.gate.Lock()
				a.Store.Set("active_account", "b")
				a.gate.Unlock()
			case "target":
				a.Store.DB.Exec(`UPDATE nodes SET trashed=1 WHERE id='root'`)
			case "new-target":
				if err := a.Store.InsertNode(Node{ID: "new-target", ParentID: "root", Name: "新保存目录", Kind: "folder"}); err != nil {
					t.Fatal(err)
				}
				if w := request(t, a.Handler(nil), "PATCH", "/api/v1/rss/"+id, map[string]string{"parent_id": "new-target"}, "test-csrf"); w.Code != 200 {
					t.Fatal(w.Body.String())
				}
			}
			close(release)
			<-done
			if change == "new-target" {
				imports := rssImportJobs(t, a, id)
				if len(imports) != 1 {
					t.Fatal("new target did not receive discovered item")
				}
				var d JobData
				json.Unmarshal(imports[0].Data, &d)
				if d.ParentID != "new-target" {
					t.Fatal("in-flight check used stale target")
				}
				return
			}
			if len(rssImportJobs(t, a, id)) != 0 {
				t.Fatal("created import after intent changed")
			}
			var count int
			a.Store.DB.QueryRow(`SELECT COUNT(*) FROM rss_entries WHERE subscription_id=?`, id).Scan(&count)
			if count != 0 {
				t.Fatal("advanced dedupe despite cancelled scan")
			}
		})
	}
}

package vault

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"pikpakvault/internal/pikpak"
	"pikpakvault/web"
)

// This server exists only in the test binary. It cannot be enabled in a release.
func TestServeE2E(t *testing.T) {
	if os.Getenv("VAULT_E2E") != "1" {
		t.Skip("opt-in browser fixture")
	}
	a, f := testApp(t)
	a.Store.Set("password", passwordHash("vault-e2e-password"))
	a.Store.Set("scan_minutes", "0")
	a.Store.DB.Exec(`UPDATE accounts SET name='我的主账号',quota_limit=?,quota_used=? WHERE id='a'`, int64(10)<<40, int64(327)<<30)
	root, e := a.ensureRoot(context.Background(), f, "a")
	if e != nil {
		t.Fatal(e)
	}
	mu := &sync.Mutex{}
	a.Factory = func(ac Account) (pikpak.Provider, error) { return lockedProvider{f, mu}, nil }
	addAccount(t, a, "b", "user-b")
	b := newFake("user-b")
	b.outputs = sampleOutputs()
	bmu := &sync.Mutex{}
	a.Factory = func(ac Account) (pikpak.Provider, error) {
		if ac.ID == "b" {
			return lockedProvider{b, bmu}, nil
		}
		return lockedProvider{f, mu}, nil
	}
	f.outputs = sampleOutputs()
	source, e := ParseSource("magnet:?xt=urn:btih:"+strings.Repeat("a", 40), "")
	if e != nil {
		t.Fatal(e)
	}
	source.Secret, _ = a.Store.Seal("")
	a.Store.SaveSource(source)
	add := func(id, parent, name, kind, mime string, size int64, thumb bool) {
		n := Node{ID: id, ParentID: parent, Name: name, Kind: kind, Mime: mime, Size: size, Hash: "hash-" + id, SourceID: source.ID, SourcePath: name, Created: now() - 86400, Modified: now()}
		if err := a.Store.InsertNode(n); err != nil {
			t.Fatal(err)
		}
		remoteParent := root
		if parent != "root" {
			remoteParent = "r-" + parent
		}
		k := "drive#file"
		if kind == "folder" {
			k = "drive#folder"
		}
		remote := pikpak.File{ID: "r-" + id, Name: name, ParentID: remoteParent, Kind: k, Size: pikpak.Number(size), Hash: n.Hash, MimeType: mime, Phase: "PHASE_TYPE_COMPLETE", WebContentLink: "https://fixture.invalid/content/" + id}
		if thumb {
			remote.Thumbnail = "https://fixture.invalid/thumb/" + id
		}
		f.files[remote.ID] = remote
		if err := a.Store.Bind("a", id, remote.ID, "present", name, remoteParent, remote.Thumbnail); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"电影时光", "旅途与风景", "设计灵感", "学习与探索"} {
		add(fmt.Sprint("folder-", i), "root", name, "folder", "", 0, false)
	}
	for i, name := range []string{"云间航行", "山野日记", "海边散步"} {
		add(fmt.Sprint("folder-preview-", i), "folder-0", name+".mp4", "file", "video/mp4", 102400, true)
	}
	add("coast", "root", "海岸线之旅 · 一场关于自由的电影.mp4", "file", "video/mp4", 2834534231, true)
	add("forest", "root", "山野之间 · The quiet moments.jpg", "file", "image/jpeg", 8412451, true)
	add("night", "root", "City Lights — 城市的另一面.mp4", "file", "video/mp4", 1413452131, true)
	add("desert", "root", "日落是最温柔的告白.jpg", "file", "image/jpeg", 5321342, true)
	add("audio", "root", "Sunday Morning — 慢生活歌单.wav", "file", "audio/wav", 88244, false)
	add("guide", "root", "设计中的设计 · 阅读笔记.txt", "file", "text/plain", 23941, false)
	add("archive", "root", "灵感收集 — Autumn collection.zip", "file", "application/zip", 128349174, false)
	add("long", "root", strings.Repeat("这是一个需要优雅处理的很长文件名", 12)+".mp4", "file", "video/mp4", 1234500, false)
	for i := 0; i < 1200; i++ {
		add(fmt.Sprintf("large-%04d", i), "folder-3", fmt.Sprintf("课程 %04d — 长目录的流畅浏览.mp4", i), "file", "video/mp4", 204857600, false)
	}
	a.Store.DB.Exec(`UPDATE nodes SET favorite=1 WHERE id IN ('coast','forest','folder-0')`)
	a.Store.DB.Exec(`UPDATE nodes SET opened=? WHERE id IN ('coast','forest','guide')`, now())
	a.MediaHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		ct := "text/plain; charset=utf-8"
		if strings.Contains(r.URL.Path, "/thumb/") || strings.Contains(r.URL.Path, "forest") || strings.Contains(r.URL.Path, "desert") {
			body = fixtureImage(r.URL.Path)
			ct = "image/png"
		} else if strings.Contains(r.URL.Path, "audio") {
			body = fixtureWAV()
			ct = "audio/wav"
		} else {
			body = []byte("设计中的设计\n\n真正好的设计，让复杂的事情变得简单。\n你的收藏，值得一直保存。")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})}
	h := http.NewServeMux()
	h.HandleFunc("POST /__fixture/teldrive-pending", func(w http.ResponseWriter, r *http.Request) {
		a.jobMu.Lock()
		defer a.jobMu.Unlock()
		id := ID()
		folderID := ID()
		if err := a.Store.InsertNode(Node{ID: folderID, ParentID: "root", Name: "TelDrive 同步目录", Kind: "folder"}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		directoryJob, err := a.Store.NewJob(a.active(), "sync", "同步 TelDrive 文件夹", JobData{MonitorID: "fixture", NodeIDs: []string{folderID}})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		directoryJob.State = "paused"
		if err = a.Store.SaveJob(&directoryJob, nil); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		source := Source{ID: ID(), Kind: "teldrive", Link: "https://fixture.invalid/files/pending", Manifest: []Entry{{Name: "TelDrive 首次上传.mp4", Path: "TelDrive 首次上传.mp4", Kind: "file", Size: 1500000000}}}
		if err := a.Store.SaveSource(source); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if err := a.Store.InsertNode(Node{ID: id, ParentID: folderID, Name: "TelDrive 首次上传.mp4", Kind: "file", Mime: "video/mp4", Size: 1500000000, SourceID: source.ID, SourcePath: source.Manifest[0].Path}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		j, err := a.Store.NewJob(a.active(), "teldrive_upload", "上传 · TelDrive 首次上传.mp4", JobData{MonitorID: "fixture", NodeIDs: []string{id}, SourceID: source.ID, ParentID: folderID})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		j.State, j.Progress, j.Message = "failed", 15, "读取 TelDrive 文件时连接提前断开，请重试原任务"
		if err = a.Store.SaveJob(&j, nil); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 200, map[string]string{"id": id, "job_id": j.ID})
	})
	h.HandleFunc("POST /__fixture/transfer/{id}/{state}", func(w http.ResponseWriter, r *http.Request) {
		a.jobMu.Lock()
		defer a.jobMu.Unlock()
		j, err := a.Store.Job(r.PathValue("id"))
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		if j.State == "running" {
			http.Error(w, "wait for checkpoint", 409)
			return
		}
		if r.PathValue("state") == "fail" {
			j.State, j.Message = "failed", "模拟来源暂时不可用，请重试"
		} else {
			mu.Lock()
			for id, file := range f.files {
				file.Phase = "PHASE_TYPE_COMPLETE"
				f.files[id] = file
			}
			for i := range f.tasks {
				f.tasks[i].Phase = "PHASE_TYPE_COMPLETE"
				f.tasks[i].Progress = 100
			}
			mu.Unlock()
			j.State, j.NextRun = "queued", 0
		}
		if err = a.Store.SaveJob(&j, nil); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		a.notify()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	h.HandleFunc("POST /__fixture/delete/{id}", func(w http.ResponseWriter, r *http.Request) {
		n, err := a.Store.Node(r.PathValue("id"), a.active())
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		mu.Lock()
		delete(f.files, n.RemoteID)
		mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	h.Handle("/", a.Handler(web.Assets()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	freshStore, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer freshStore.DB.Close()
	fresh := NewApp(freshStore)
	fresh.SetupToken = "vault-e2e-setup"
	fresh.Factory = a.Factory
	freshServer := &http.Server{Addr: "127.0.0.1:8089", Handler: fresh.Handler(web.Assets()), ReadHeaderTimeout: 5 * time.Second}
	defer freshServer.Close()
	go freshServer.ListenAndServe()
	server := &http.Server{Addr: "127.0.0.1:8088", Handler: h, ReadHeaderTimeout: 5 * time.Second}
	t.Log("E2E fixture listening at http://127.0.0.1:8088")
	if e = server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		t.Fatal(e)
	}
}

func fixtureImage(seed string) []byte {
	w, h := 640, 400
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	kind := 0
	for _, r := range seed {
		kind += int(r)
	}
	kind %= 4
	palettes := [][3]color.RGBA{{{166, 188, 194, 255}, {88, 130, 153, 255}, {39, 76, 91, 255}}, {{215, 181, 157, 255}, {187, 136, 109, 255}, {135, 95, 81, 255}}, {{101, 119, 150, 255}, {66, 87, 116, 255}, {34, 54, 83, 255}}, {{170, 189, 174, 255}, {112, 145, 126, 255}, {54, 95, 85, 255}}}
	p := palettes[kind]
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64(y) / float64(h)
			c := p[0]
			c.R = uint8(float64(c.R) * (1 - .15*t))
			c.G = uint8(float64(c.G) * (1 - .15*t))
			c.B = uint8(float64(c.B) * (1 - .12*t))
			sun := math.Hypot(float64(x-465), float64(y-91))
			if sun < 30 {
				c = color.RGBA{235, 226, 204, 255}
			}
			hill1 := 210 + 45*math.Sin(float64(x)/120) + 25*math.Cos(float64(x)/58)
			hill2 := 295 + 37*math.Sin(float64(x)/100+2)
			if float64(y) > hill1 {
				c = p[1]
			}
			if float64(y) > hill2 {
				c = p[2]
			}
			noise := (x*73+y*31)%5 - 2
			c.R = uint8(max(0, min(255, int(c.R)+noise)))
			c.G = uint8(max(0, min(255, int(c.G)+noise)))
			c.B = uint8(max(0, min(255, int(c.B)+noise)))
			img.SetRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}
func fixtureWAV() []byte {
	const rate = 22050
	const seconds = 2
	samples := rate * seconds
	data := make([]byte, 44+samples*2)
	copy(data, "RIFF")
	put := func(at, n int) {
		for i := 0; i < 4; i++ {
			data[at+i] = byte(n >> (8 * i))
		}
	}
	put(4, len(data)-8)
	copy(data[8:], "WAVEfmt ")
	put(16, 16)
	data[20] = 1
	data[22] = 1
	put(24, rate)
	put(28, rate*2)
	data[32] = 2
	data[34] = 16
	copy(data[36:], "data")
	put(40, samples*2)
	return data
}

type lockedProvider struct {
	f  *fakeDrive
	mu *sync.Mutex
}

func (l lockedProvider) Me(c context.Context) (pikpak.Identity, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Me(c)
}
func (l lockedProvider) Quota(c context.Context) (pikpak.Quota, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Quota(c)
}
func (l lockedProvider) List(c context.Context, p, n string) (pikpak.Page, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.List(c, p, n)
}
func (l lockedProvider) Get(c context.Context, id string) (pikpak.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Get(c, id)
}
func (l lockedProvider) Mkdir(c context.Context, p, n string) (pikpak.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Mkdir(c, p, n)
}
func (l lockedProvider) Move(c context.Context, id, p string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Move(c, id, p)
}
func (l lockedProvider) Rename(c context.Context, id, n string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Rename(c, id, n)
}
func (l lockedProvider) Trash(c context.Context, ids []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Trash(c, ids)
}
func (l lockedProvider) Untrash(c context.Context, ids []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Untrash(c, ids)
}
func (l lockedProvider) Offline(c context.Context, url, p string) (pikpak.Transfer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	result, err := l.f.Offline(c, url, p)
	if err == nil && strings.Contains(url, "E2E-InPlace") {
		for id, file := range l.f.files {
			if file.ParentID != "" && !file.Folder() && strings.HasPrefix(id, l.f.identity) {
				file.Phase = "PHASE_TYPE_RUNNING"
				l.f.files[id] = file
			}
		}
		if len(l.f.tasks) > 0 {
			task := &l.f.tasks[len(l.f.tasks)-1]
			task.Phase = "PHASE_TYPE_RUNNING"
			task.Progress = 43
			result.TaskID = task.ID
		}
	}
	return result, err
}
func (l lockedProvider) Tasks(c context.Context) ([]pikpak.Task, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Tasks(c)
}
func (l lockedProvider) Share(c context.Context, id, p, t, d, n string) (pikpak.Share, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Share(c, id, p, t, d, n)
}
func (l lockedProvider) RestoreShare(c context.Context, id, t string, ids []string, p string) (pikpak.Transfer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.RestoreShare(c, id, t, ids, p)
}
func (l lockedProvider) Instant(c context.Context, p string, f pikpak.File) (pikpak.Transfer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Instant(c, p, f)
}

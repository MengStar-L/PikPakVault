package aria2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func requireBinary(t *testing.T) string {
	t.Helper()
	p, e := Ensure(context.Background(), ProgramDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}

type slowReader struct{ r io.Reader }

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(30 * time.Millisecond)
	return s.r.Read(p[:min(len(p), 32768)])
}
func TestAria2PausePersistsAndResumes(t *testing.T) {
	bin := requireBinary(t)
	body := bytes.Repeat([]byte("resumable-content"), 500000)
	var resumeOffset atomic.Int64
	s := Source{Binary: bin, Identity: "resume", Size: int64(len(body)), Open: func(_ context.Context, start, length int64) (io.ReadCloser, error) {
		if start > 0 {
			resumeOffset.Store(start)
		}
		return io.NopCloser(slowReader{bytes.NewReader(body[start : start+length])}), nil
	}}
	dir := filepath.Join(t.TempDir(), "download")
	paused := errors.New("paused by user")
	_, e := Fetch(context.Background(), dir, s, func(p Progress) error {
		if p.Completed > 2<<20 {
			return paused
		}
		return nil
	})
	if !errors.Is(e, paused) {
		t.Fatal("pause ignored", e)
	}
	if _, e = os.Stat(filepath.Join(dir, "content.aria2")); e != nil {
		t.Fatal("resume control lost", e)
	}
	if _, e = os.Stat(filepath.Join(dir, "runtime.json")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("child still attached", e)
	}
	file, e := Fetch(context.Background(), dir, s, func(Progress) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if resumeOffset.Load() <= 0 {
		t.Fatal("download restarted from zero")
	}
	got, _ := os.ReadFile(file)
	if !bytes.Equal(got, body) {
		t.Fatal("resumed content mismatch")
	}
}

func TestAria2SourceDisconnectRetriesWithoutNewFile(t *testing.T) {
	bin := requireBinary(t)
	body := bytes.Repeat([]byte("recover-disconnect"), 300000)
	var calls atomic.Int32
	var resumed atomic.Bool
	s := Source{Binary: bin, Identity: "disconnect", Size: int64(len(body)), Open: func(_ context.Context, start, length int64) (io.ReadCloser, error) {
		if start > 0 {
			resumed.Store(true)
		}
		if calls.Add(1) == 1 {
			length = min(length, 2<<20)
		}
		return io.NopCloser(bytes.NewReader(body[start : start+length])), nil
	}}
	dir := filepath.Join(t.TempDir(), "download")
	file, e := Fetch(context.Background(), dir, s, func(Progress) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(file)
	if !bytes.Equal(got, body) || calls.Load() < 2 || !resumed.Load() {
		t.Fatal("disconnect was not resumed", calls.Load(), resumed.Load())
	}
	if _, e = os.Stat(filepath.Join(dir, "content.1")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("renamed duplicate created")
	}
}

func TestCacheCleanupRejectsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "unrelated.txt")
	os.WriteFile(file, []byte("keep"), 0600)
	if e := Remove(dir); e == nil {
		t.Fatal("removed unrelated file")
	}
	if _, e := os.Stat(file); e != nil {
		t.Fatal(e)
	}
}

func TestAria2InsufficientSpaceNeverReadsSource(t *testing.T) {
	bin := requireBinary(t)
	_, e := Fetch(context.Background(), filepath.Join(t.TempDir(), "cache"), Source{Binary: bin, Identity: "huge", Size: 1 << 62, Open: func(context.Context, int64, int64) (io.ReadCloser, error) {
		t.Error("read source without disk space")
		return nil, io.EOF
	}}, func(Progress) error { return nil })
	if e == nil || !strings.Contains(e.Error(), "空间不足") {
		t.Fatal(e)
	}
}

func TestAria2CrashHelper(t *testing.T) {
	if os.Getenv("VAULT_ARIA2_CRASH_HELPER") != "1" {
		t.Skip("subprocess fixture")
	}
	body := bytes.Repeat([]byte("crash-content"), 800000)
	_, e := Fetch(context.Background(), os.Getenv("VAULT_ARIA2_CRASH_DIR"), Source{Binary: requireBinary(t), Identity: "crash", Size: int64(len(body)), Open: func(_ context.Context, start, length int64) (io.ReadCloser, error) {
		return io.NopCloser(slowReader{bytes.NewReader(body[start : start+length])}), nil
	}}, func(Progress) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
}

func TestAria2ParentCrashRetainsResumeState(t *testing.T) {
	requireBinary(t)
	dir := filepath.Join(t.TempDir(), "download")
	cmd := exec.Command(os.Args[0], "-test.run=^TestAria2CrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "VAULT_ARIA2_CRASH_HELPER=1", "VAULT_ARIA2_CRASH_DIR="+dir)
	configureProcess(cmd)
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill() }()
	deadline := time.Now().Add(25 * time.Second)
	var running runtimeState
	for {
		info, e := os.Stat(filepath.Join(dir, "content"))
		if e == nil && info.Size() > 3<<20 && readJSON(filepath.Join(dir, "runtime.json"), &running) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start downloading")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if e := cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	if _, e := os.Stat(filepath.Join(dir, "content.aria2")); e != nil {
		t.Fatal("crash lost control file", e)
	}
	body := bytes.Repeat([]byte("crash-content"), 800000)
	var resumed atomic.Bool
	file, e := Fetch(context.Background(), dir, Source{Binary: requireBinary(t), Identity: "crash", Size: int64(len(body)), Open: func(_ context.Context, start, length int64) (io.ReadCloser, error) {
		if start > 0 {
			resumed.Store(true)
		}
		return io.NopCloser(bytes.NewReader(body[start : start+length])), nil
	}}, func(Progress) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(file)
	if !bytes.Equal(got, body) || !resumed.Load() {
		t.Fatal("crash resume failed")
	}
}
func TestAria2DownloadsCachesAndCleans(t *testing.T) {
	bin := requireBinary(t)
	b := bytes.Repeat([]byte("test-content"), 200000)
	var requests atomic.Int32
	s := Source{Binary: bin, Identity: "fixture", Size: int64(len(b)), Open: func(_ context.Context, offset, length int64) (io.ReadCloser, error) {
		requests.Add(1)
		return io.NopCloser(bytes.NewReader(b[offset : offset+length])), nil
	}}
	dir := filepath.Join(t.TempDir(), "download")
	file, e := Fetch(context.Background(), dir, s, func(p Progress) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(file)
	if e != nil || !bytes.Equal(got, b) {
		t.Fatal("wrong content", e)
	}
	before := requests.Load()
	if _, e = Fetch(context.Background(), dir, s, func(Progress) error { return nil }); e != nil || requests.Load() != before {
		t.Fatal("downloaded cached file again", e)
	}
	s.Identity = "changed"
	if _, e = Fetch(context.Background(), dir, s, func(Progress) error { return nil }); e == nil {
		t.Fatal("mixed source versions")
	}
	if e = Remove(dir); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(dir); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("cache not removed", e)
	}
}

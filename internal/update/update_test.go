package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func archive(t *testing.T, headers []*tar.Header, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if e := tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if h.Size > 0 {
			if _, e := tw.Write(content[:h.Size]); e != nil {
				t.Fatal(e)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}
func elfBytes() []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], 2)
	binary.LittleEndian.PutUint16(b[18:], 62)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}
func releaseFixture(t *testing.T, corrupt bool) (*Client, string) {
	t.Helper()
	body := archive(t, []*tar.Header{{Name: "vault", Mode: 0755, Size: 64, Typeflag: tar.TypeReg}}, elfBytes())
	digest := sha256.Sum256(body)
	sum := hex.EncodeToString(digest[:])
	if corrupt {
		sum = strings.Repeat("0", 64)
	}
	repo := "owner/vault"
	name := "pikpak-vault-0.3.0-linux-amd64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			fmt.Fprint(w, `{"status":"ok","version":"0.2.0","pid":"1","ready":true}`)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			fmt.Fprint(w, sum+"  "+name+"\n")
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			w.Write(body)
		default:
			json.NewEncoder(w).Encode(Release{Tag: "v0.3.0", Assets: []Asset{{Name: name, Size: int64(len(body)), URL: "https://github.com/" + repo + "/releases/download/v0.3.0/" + name}, {Name: "SHA256SUMS", URL: "https://github.com/" + repo + "/releases/download/v0.3.0/SHA256SUMS"}}})
		}
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	c := &Client{API: server.URL, Repository: repo, Arch: "amd64", HTTP: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		cp := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = base.Scheme
		u.Host = base.Host
		cp.URL = &u
		return http.DefaultTransport.RoundTrip(cp)
	})}}
	return c, server.URL + "/healthz"
}
func TestReleaseDownloadAndChecksum(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		c, _ := releaseFixture(t, corrupt)
		r, e := c.Release(context.Background(), "")
		if e != nil {
			t.Fatal(e)
		}
		target := filepath.Join(t.TempDir(), "vault")
		e = c.Download(context.Background(), r, target)
		if (e != nil) != corrupt {
			t.Fatalf("checksum corrupt=%v: %v", corrupt, e)
		}
	}
	c, _ := releaseFixture(t, false)
	r, e := c.Release(context.Background(), "")
	if e != nil {
		t.Fatal(e)
	}
	c.Arch = "arm64"
	if e = c.Download(context.Background(), r, filepath.Join(t.TempDir(), "vault")); e == nil {
		t.Fatal("missing arch accepted")
	}
	r.Assets[0].URL = "https://evil.test/vault"
	if _, e = c.asset(r, r.Assets[0].Name); e == nil {
		t.Fatal("untrusted origin accepted")
	}
}
func TestRejectUnsafeArchives(t *testing.T) {
	for _, variant := range []string{"traversal", "symlink", "duplicate", "missing"} {
		t.Run(variant, func(t *testing.T) {
			headers := []*tar.Header{{Name: "vault", Size: 64, Mode: 0755, Typeflag: tar.TypeReg}}
			switch variant {
			case "traversal":
				headers = append(headers, &tar.Header{Name: "../escape", Typeflag: tar.TypeReg})
			case "symlink":
				headers = append(headers, &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
			case "duplicate":
				headers = append(headers, headers[0])
			case "missing":
				headers = nil
			}
			if e := ExtractBinary(bytes.NewReader(archive(t, headers, elfBytes())), filepath.Join(t.TempDir(), "vault")); e == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
func TestSemver(t *testing.T) {
	for _, v := range []struct {
		tag, current string
		want         bool
	}{{"v0.3.0", "0.2.0", true}, {"v1.10.0", "1.9.9", true}, {"v0.2.0", "0.2.0", false}, {"v0.1.9", "0.2.0", false}, {"v1.2.0-beta", "0.2.0", false}, {"v01.2.0", "0.2.0", false}, {"v1.0.0", "dev", false}} {
		if Newer(v.tag, v.current) != v.want {
			t.Fatal(v)
		}
	}
}

func workerFixture(t *testing.T, corrupt bool) *Worker {
	t.Helper()
	dir := t.TempDir()
	data := t.TempDir()
	binaryPath := filepath.Join(t.TempDir(), "vault")
	helper := filepath.Join(t.TempDir(), "maintenance")
	os.WriteFile(binaryPath, []byte("old-binary"), 0755)
	os.WriteFile(helper, []byte("old-binary"), 0755)
	os.WriteFile(filepath.Join(data, "db"), []byte("original-auth-and-paths"), 0600)
	c, health := releaseFixture(t, corrupt)
	w := &Worker{Dir: dir, Data: data, Executable: binaryPath, HelperPath: helper, Client: c, Health: health}
	w.Backup = func(data, backup string) error {
		b, e := os.ReadFile(filepath.Join(data, "db"))
		if e != nil {
			return e
		}
		return os.WriteFile(backup, b, 0600)
	}
	w.Restore = func(backup, data string) error {
		b, e := os.ReadFile(backup)
		if e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(data, "db"), b, 0600)
	}
	w.Control = func(context.Context, string) error { return nil }
	w.Ready = func(context.Context, string, string, bool) error { return nil }
	req, _ := json.Marshal(Request{Tag: "v0.3.0", Token: Nonce()})
	os.WriteFile(filepath.Join(data, RequestName), req, 0600)
	return w
}
func TestWorkerSuccessRollbackAndCrash(t *testing.T) {
	for _, scenario := range []string{"success", "checksum", "readiness", "crash"} {
		t.Run(scenario, func(t *testing.T) {
			w := workerFixture(t, scenario == "checksum")
			stops := 0
			starts := 0
			w.Control = func(ctx context.Context, action string) error {
				if action == "stop" {
					stops++
				}
				if action == "start" {
					starts++
					if starts == 1 {
						os.WriteFile(filepath.Join(w.Data, "db"), []byte("candidate-migration"), 0600)
						if scenario == "crash" {
							panic("simulated process death")
						}
					}
				}
				return nil
			}
			w.Ready = func(ctx context.Context, v, token string, candidate bool) error {
				if scenario == "readiness" && v == "0.3.0" {
					return fmt.Errorf("candidate failed readiness")
				}
				return nil
			}
			func() {
				defer func() {
					if v := recover(); v != nil && scenario != "crash" {
						panic(v)
					}
				}()
				if e := w.Run(context.Background()); e != nil {
					t.Fatal(e)
				}
			}()
			if scenario == "crash" {
				if e := w.Run(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			s := ReadStatus(w.Dir)
			db, _ := os.ReadFile(filepath.Join(w.Data, "db"))
			exe, _ := os.ReadFile(w.Executable)
			switch scenario {
			case "success":
				if s.Phase != "completed" || !s.Accepted || string(db) != "candidate-migration" || string(exe) == "old-binary" {
					t.Fatalf("success %+v", s)
				}
			case "checksum":
				if stops != 0 || s.Phase != "failed" || string(db) != "original-auth-and-paths" {
					t.Fatal("failed download changed live data")
				}
			default:
				if s.Phase != "rolled_back" || string(db) != "original-auth-and-paths" || string(exe) != "old-binary" {
					t.Fatalf("rollback failed: %+v db=%s", s, db)
				}
			}
			if _, e := os.Stat(filepath.Join(w.Dir, "startup.env")); !os.IsNotExist(e) {
				t.Fatal("startup gate retained")
			}
		})
	}
}

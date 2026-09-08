//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pikpakvault/internal/update"
	"pikpakvault/internal/vault"
)

type fixtureTransport struct{ base *url.URL }

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = f.base.Scheme
	u.Host = f.base.Host
	c.URL = &u
	return http.DefaultTransport.RoundTrip(c)
}

// The subprocess runs in the real updater's independent systemd cgroup. Only
// this test binary replaces GitHub with a local HTTP fixture.
func TestSystemdHelper(t *testing.T) {
	if os.Getenv("VAULT_SYSTEMD_HELPER") != "1" {
		t.Skip("systemd child only")
	}
	w := maintenanceWorker()
	base, _ := url.Parse(os.Getenv("VAULT_FIXTURE_URL"))
	w.Client.API = base.String()
	w.Client.Repository = "fixture/vault"
	w.Client.HTTP = &http.Client{Transport: fixtureTransport{base}, Timeout: time.Minute}
	w.Health = "http://127.0.0.1:5675/healthz"
	if os.Getenv("VAULT_FIXTURE_CRASH") == "1" {
		original := w.Control
		w.Control = func(ctx context.Context, action string) error {
			e := original(ctx, action)
			if e == nil && action == "start" && update.ReadStatus(update.StateDir).Phase == "verifying" {
				marker := filepath.Join(update.StateDir, "crashed-once")
				if _, err := os.Stat(marker); os.IsNotExist(err) {
					os.WriteFile(marker, []byte("1"), 0600)
					s, err := vault.Open(update.DataDir)
					if err != nil {
						return err
					}
					s.Set("fixture-record", "candidate-mutated")
					s.DB.Close()
					os.Exit(90)
				}
			}
			return e
		}
	}
	if e := w.Run(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestSystemdLifecycle(t *testing.T) {
	if os.Getenv("VAULT_SYSTEMD_TEST") != "1" {
		t.Skip("requires disposable root systemd runner")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root")
	}
	for _, p := range []string{update.Binary, filepath.Join(update.DataDir, "vault.db")} {
		if _, e := os.Stat(p); !os.IsNotExist(e) {
			t.Fatal("refusing to use existing installation", p)
		}
	}
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	tmp := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command(args[0], args[1:]...)
		c.Dir = repo
		if out, e := c.CombinedOutput(); e != nil {
			t.Fatalf("%v: %v\n%s", args, e, out)
		}
	}
	t.Cleanup(func() {
		exec.Command("systemctl", "stop", "pikpak-vault-update.path", "pikpak-vault-update.service", "pikpak-vault.service").Run()
	})
	candidate := filepath.Join(tmp, "candidate")
	run("go", "build", "-o", candidate, "-ldflags=-X pikpakvault/internal/vault.Version=0.3.0", "./cmd/vault")
	bundle := filepath.Join(tmp, "bundle")
	os.MkdirAll(bundle, 0755)
	run("go", "build", "-o", filepath.Join(bundle, "vault"), "-ldflags=-X pikpakvault/internal/vault.Version=0.2.0", "./cmd/vault")
	run("cp", "-R", filepath.Join(repo, "deploy"), bundle)
	// Seed fake credentials and paths only; tests never access a PikPak account.
	os.MkdirAll(update.DataDir, 0700)
	s, e := vault.Open(update.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	s.Set("password", base64.RawStdEncoding.EncodeToString(make([]byte, 16))+"."+base64.RawStdEncoding.EncodeToString(make([]byte, 32)))
	s.Set("scan_minutes", "0")
	s.Set("fixture-record", "original")
	secret, _ := s.Seal(map[string]string{"refresh_token": "fixture-only-token"})
	_, e = s.DB.Exec(`INSERT INTO accounts(id,name,secret,created) VALUES('fixture','fixture',?,1)`, secret)
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Close()
	run("useradd", "--system", "--home-dir", update.DataDir, "--shell", "/usr/sbin/nologin", "pikpak-vault")
	run("chown", "-R", "pikpak-vault:pikpak-vault", update.DataDir)
	run("bash", filepath.Join(bundle, "deploy/install.sh"))
	run("systemctl", "restart", update.Unit)
	current := func() string {
		r, e := http.Get("http://127.0.0.1:5675/healthz")
		if e != nil {
			return ""
		}
		defer r.Body.Close()
		var h struct {
			Version string `json:"version"`
			Ready   bool   `json:"ready"`
		}
		json.NewDecoder(r.Body).Decode(&h)
		if !h.Ready {
			return ""
		}
		return h.Version
	}
	wait := func(check func() bool) {
		t.Helper()
		limit := time.Now().Add(100 * time.Second)
		for time.Now().Before(limit) {
			if check() {
				return
			}
			time.Sleep(time.Second)
		}
		out, _ := exec.Command("journalctl", "-u", "pikpak-vault-update", "-u", "pikpak-vault", "-n", "80", "--no-pager").CombinedOutput()
		t.Fatalf("timeout; status=%+v\n%s", update.ReadStatus(update.StateDir), out)
	}
	wait(func() bool { return current() == "0.2.0" })
	testBinary, _ := os.Executable()
	for _, scenario := range []string{"crash", "readiness", "success"} {
		t.Run(scenario, func(t *testing.T) {
			payload, e := os.ReadFile(candidate)
			if e != nil {
				t.Fatal(e)
			}
			if scenario == "readiness" {
				payload, e = os.ReadFile("/bin/false")
				if e != nil {
					t.Fatal(e)
				}
			}
			var pack bytes.Buffer
			gz := gzip.NewWriter(&pack)
			tw := tar.NewWriter(gz)
			tw.WriteHeader(&tar.Header{Name: "vault", Mode: 0755, Size: int64(len(payload)), Typeflag: tar.TypeReg})
			tw.Write(payload)
			tw.Close()
			gz.Close()
			body := pack.Bytes()
			sum := sha256.Sum256(body)
			name := "pikpak-vault-0.3.0-linux-amd64.tar.gz"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
					fmt.Fprint(w, hex.EncodeToString(sum[:])+"  "+name+"\n")
				} else if strings.HasSuffix(r.URL.Path, ".tar.gz") {
					w.Write(body)
				} else {
					json.NewEncoder(w).Encode(update.Release{Tag: "v0.3.0", Assets: []update.Asset{{Name: name, Size: int64(len(body)), URL: "https://github.com/fixture/vault/releases/download/v0.3.0/" + name}, {Name: "SHA256SUMS", URL: "https://github.com/fixture/vault/releases/download/v0.3.0/SHA256SUMS"}}})
				}
			}))
			defer server.Close()
			run("systemctl", "stop", "pikpak-vault-update.path", "pikpak-vault-update.service")
			override := "/etc/systemd/system/pikpak-vault-update.service.d"
			os.MkdirAll(override, 0755)
			crash := "0"
			if scenario == "crash" {
				crash = "1"
			}
			config := "[Service]\nExecStart=\nExecStart=" + testBinary + " -test.run=^TestSystemdHelper$ -test.v\nEnvironment=VAULT_SYSTEMD_HELPER=1 VAULT_FIXTURE_URL=" + server.URL + " VAULT_FIXTURE_CRASH=" + crash + "\nRestartSec=1\nProtectHome=false\nProtectSystem=false\nPrivateTmp=false\n"
			os.WriteFile(filepath.Join(override, "fixture.conf"), []byte(config), 0644)
			run("systemctl", "daemon-reload")
			run("systemctl", "reset-failed", "pikpak-vault-update.service", update.Unit)
			run("systemctl", "start", "pikpak-vault-update.path")
			if e = update.Submit(update.DataDir, "v0.3.0"); e != nil {
				t.Fatal(e)
			}
			wait(func() bool {
				st := update.ReadStatus(update.StateDir)
				return st.Tag == "v0.3.0" && !st.Busy() && st.Updated > 0 && func() bool {
					_, e := os.Stat(filepath.Join(update.DataDir, update.RequestName))
					return os.IsNotExist(e)
				}()
			})
			status := update.ReadStatus(update.StateDir)
			expected := "rolled_back"
			version := "0.2.0"
			if scenario == "success" {
				expected = "completed"
				version = "0.3.0"
			}
			if status.Phase != expected {
				t.Fatalf("unexpected phase %+v", status)
			}
			wait(func() bool { return current() == version })
			db, e := vault.Open(update.DataDir)
			if e != nil {
				t.Fatal(e)
			}
			defer db.DB.Close()
			if db.Get("fixture-record") != "original" {
				t.Fatal("rollback lost data")
			}
			ac, e := db.Account("fixture")
			var credentials map[string]string
			if e != nil {
				t.Fatal(e)
			}
			if e = db.Unseal(ac.Secret, &credentials); e != nil || credentials["refresh_token"] != "fixture-only-token" {
				t.Fatal("rollback lost credentials")
			}
			t.Logf("real systemd %s: phase=%s version=%s; data and credentials preserved", scenario, status.Phase, version)
		})
	}
}

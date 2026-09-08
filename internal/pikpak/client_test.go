package pikpak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAccountRateLimitCoolsAllEndpointsWithoutRequests(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"too_many_requests"}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "token", CaptchaToken: "captcha"})
	c.DriveURL = server.URL
	_, err := c.Offline(context.Background(), "magnet:test", "root")
	var up *APIError
	if !errors.As(err, &up) || up.RetryAfter < 85 {
		t.Fatal(err)
	}
	_, err = c.Get(context.Background(), "file")
	if !errors.As(err, &up) || up.Code != "account_cooldown" || calls != 1 {
		t.Fatal(err, calls)
	}
}
func TestWritePacingAndCancellation(t *testing.T) {
	var stamps []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stamps = append(stamps, time.Now())
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "token", CaptchaToken: "captcha"})
	c.DriveURL = server.URL
	c.RequestInterval = 0
	c.WriteInterval = 60 * time.Millisecond
	if err := c.Move(context.Background(), "one", "parent"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename(context.Background(), "one", "name"); err != nil {
		t.Fatal(err)
	}
	if len(stamps) != 2 || stamps[1].Sub(stamps[0]) < 50*time.Millisecond {
		t.Fatal("writes were not paced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Trash(ctx, []string{"one"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(stamps) != 2 {
		t.Fatal("sent cancelled write")
	}
}

func TestRecycledGetReturnsTombstone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"file_in_recycle_bin","error_description":"File or folder is already in recycler"}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "token", CaptchaToken: "captcha"})
	c.DriveURL = server.URL
	f, err := c.Get(context.Background(), "deleted-id")
	if err != nil || !f.Trashed || f.ID != "deleted-id" {
		t.Fatal(f, err)
	}
}

func TestRefreshPersistsRotatedTokensAndRejectsIdentityChange(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotation", true: "identity-mismatch"}[changed], func(t *testing.T) {
			requests := 0
			persisted := Credentials{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/user/me":
					requests++
					if requests == 1 {
						w.WriteHeader(401)
						w.Write([]byte(`{"error":"unauthenticated"}`))
						return
					}
					if r.Header.Get("Authorization") != "Bearer new-access" {
						t.Error("rotated access token not used")
					}
					w.Write([]byte(`{"sub":"user-a"}`))
				case "/v1/auth/token":
					var b map[string]string
					json.NewDecoder(r.Body).Decode(&b)
					if b["refresh_token"] != "old-refresh" {
						t.Error("refresh token not sent")
					}
					sub := "user-a"
					if changed {
						sub = "user-b"
					}
					json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "sub": sub, "expires_in": 3600})
				default:
					t.Error("unexpected endpoint", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			c := New(Credentials{UserID: "user-a", AccessToken: "old-access", RefreshToken: "old-refresh", CaptchaToken: "captcha"})
			c.UserURL = server.URL
			c.Save = func(v Credentials) error { persisted = v; return nil }
			_, err := c.Me(context.Background())
			if changed {
				if err == nil || !strings.Contains(err.Error(), "identity_changed") {
					t.Fatal(err)
				}
				if persisted.AccessToken != "" {
					t.Fatal("persisted mismatched credentials")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if persisted.RefreshToken != "new-refresh" || requests != 2 {
					t.Fatal("rotation did not persist")
				}
			}
		})
	}
}
func TestWriteTransportFailureIsNotRetried(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"busy","error_description":"temporary"}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access", CaptchaToken: "captcha"})
	c.DriveURL = server.URL
	_, err := c.Offline(context.Background(), "magnet:?xt=urn:btih:"+strings.Repeat("a", 40), "root")
	if err == nil || calls != 1 {
		t.Fatalf("mutation retried: %d, %v", calls, err)
	}
}
func TestMoveToAccountTopIncludesEmptyDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var destination map[string]string
		if err := json.Unmarshal(body["to"], &destination); err != nil {
			t.Error("destination missing", err)
		}
		if parent, ok := destination["parent_id"]; !ok || parent != "" {
			t.Error("account top destination was omitted")
		}
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access", CaptchaToken: "captcha"})
	c.DriveURL = server.URL
	if err := c.Move(context.Background(), "root-id", ""); err != nil {
		t.Fatal(err)
	}
}
func TestSharePaginationAndExtractionCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path == "/drive/v1/share" {
			if q.Get("pass_code") != "private-code" {
				t.Error("missing extraction code")
			}
			w.Write([]byte(`{"share_status":"OK","pass_code_token":"short-lived","next_page_token":"next","files":[{"id":"one","size":"123"}]}`))
			return
		}
		if q.Get("pass_code_token") != "short-lived" || q.Get("page_token") != "next" || q.Has("pass_code") {
			t.Error("incorrect pagination auth")
		}
		w.Write([]byte(`{"share_status":"OK","files":[{"id":"two","size":456}]}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access", CaptchaToken: "captcha"})
	c.DriveURL = server.URL
	p, e := c.Share(context.Background(), "id", "private-code", "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	second, e := c.Share(context.Background(), "id", "", p.PassCodeToken, "", p.Next)
	if e != nil {
		t.Fatal(e)
	}
	if p.Files[0].Size != 123 || second.Files[0].Size != 456 {
		t.Fatal("numeric size parsing failed")
	}
}
func TestCaptchaChallengeIsVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"captcha_token":"challenge-token","expires_in":300,"url":"https://mypikpak.com/verify/challenge"}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access"})
	c.UserURL = server.URL
	_, err := c.Me(context.Background())
	e, ok := err.(*APIError)
	if !ok || e.Code != "verification_required" || e.VerificationURL == "" {
		t.Fatalf("challenge hidden: %v", err)
	}
}

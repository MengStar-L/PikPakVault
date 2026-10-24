package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pikpakvault/internal/pikpak"
)

func TestAccountVerifyDeduplicatedAndPendingVisible(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	h := a.Handler(nil)
	var first Job
	for i := 0; i < 3; i++ {
		w := request(t, h, "POST", "/api/v1/accounts/a/verify", map[string]any{}, "test-csrf")
		var j Job
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &j) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		if i == 0 {
			first = j
		} else if j.ID != first.ID {
			t.Fatal("duplicate verification")
		}
	}
	var listing struct {
		Accounts []Account `json:"accounts"`
	}
	w := request(t, h, "GET", "/api/v1/accounts", nil, "")
	if json.Unmarshal(w.Body.Bytes(), &listing) != nil || len(listing.Accounts) != 1 || !listing.Accounts[0].VerificationPending {
		t.Fatal(w.Body.String())
	}
	older, _ := a.Store.NewJob("a", "verify", "older", JobData{})
	_, _ = a.Store.DB.Exec(`UPDATE jobs SET state='failed' WHERE id=?`, older.ID)
	w = request(t, h, "POST", "/api/v1/jobs/"+older.ID+"/retry", map[string]any{}, "test-csrf")
	if w.Code != 409 {
		t.Fatal("retry created parallel verification", w.Code)
	}
	requireComplete(t, execute(t, a, &first))
	w = request(t, h, "GET", "/api/v1/accounts", nil, "")
	if json.Unmarshal(w.Body.Bytes(), &listing) != nil || listing.Accounts[0].VerificationPending {
		t.Fatal(w.Body.String())
	}
}

func TestAccountFaultGuardsDoNotEnqueueOrLoseTasks(t *testing.T) {
	for _, state := range []string{"blocked", "auth_required", "quota_exceeded", "verification_required", "error"} {
		t.Run(state, func(t *testing.T) {
			a, _ := testApp(t)
			sessionForTest(t, a)
			j, _ := a.Store.NewJob("a", "import", "pending", JobData{ParentID: "root"})
			_, _ = a.Store.DB.Exec(`UPDATE jobs SET state='paused',attempts=3 WHERE id=?`, j.ID)
			_, _ = a.Store.DB.Exec(`UPDATE accounts SET status=?,error=? WHERE id='a'`, state, "账号原因明确可见")
			h := a.Handler(nil)
			for _, operation := range []struct {
				path string
				body any
			}{
				{"/api/v1/imports", map[string]any{"items": []map[string]string{{"link": "magnet:?xt=urn:btih:0123456789012345678901234567890123456789", "parent_id": "root"}}}},
				{"/api/v1/recovery", map[string]any{"confirm": true, "account_id": "a", "ids": []string{}}},
				{"/api/v1/jobs/" + j.ID + "/retry", map[string]any{}},
			} {
				w := request(t, h, "POST", operation.path, operation.body, "test-csrf")
				if w.Code != 409 || !strings.Contains(w.Body.String(), "账号原因明确可见") {
					t.Fatalf("%s: %d %s", operation.path, w.Code, w.Body.String())
				}
			}
			retained, _ := a.Store.Job(j.ID)
			jobs, _ := a.Store.Jobs()
			if len(jobs) != 1 || retained.State != "paused" || retained.Attempts != 3 || string(retained.Data) != string(j.Data) {
				t.Fatal("task state changed", retained)
			}
			w := request(t, h, "POST", "/api/v1/scans", map[string]any{}, "test-csrf")
			want := 409
			if state == "quota_exceeded" {
				want = 202
			}
			if w.Code != want {
				t.Fatal("scan guard", w.Code, w.Body.String())
			}
		})
	}
}

func TestSynchronousMediaFailureUpdatesOriginalAccount(t *testing.T) {
	a, _ := testApp(t)
	a.Factory = nil
	sessionForTest(t, a)
	addAccount(t, a, "b", "user-b")
	secret, _ := a.Store.Seal(pikpak.Credentials{AccessToken: "current-token", CaptchaToken: "captcha", Expiry: time.Now().Add(time.Hour).Unix(), CaptchaExpiry: time.Now().Add(time.Hour).Unix()})
	_, _ = a.Store.DB.Exec(`UPDATE accounts SET secret=? WHERE id='a'`, secret)
	_ = a.Store.InsertNode(Node{ID: "movie", ParentID: "root", Kind: "file", Name: "movie.mp4"})
	_ = a.Store.Bind("a", "movie", "remote-movie", "present", "movie.mp4", "remote-root", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = a.Store.Set("active_account", "b")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"user_blocked","error_description":"user blocked current-token"}`))
	}))
	defer server.Close()
	provider, err := a.client("a")
	if err != nil {
		t.Fatal(err)
	}
	c := provider.(*pikpak.Client)
	c.DriveURL = server.URL
	c.RequestInterval = 0
	w := request(t, a.Handler(nil), "GET", "/api/v1/files/movie/media", nil, "")
	if w.Code != 502 || !strings.Contains(w.Body.String(), "冻结") || strings.Contains(w.Body.String(), "current-token") {
		t.Fatal(w.Code, w.Body.String())
	}
	ac, _ := a.Store.Account("a")
	other, _ := a.Store.Account("b")
	if ac.Status != "blocked" || other.Status != "ready" {
		t.Fatal(ac.Status, other.Status)
	}
	// This stale cached client cannot damage credentials replaced while the request ran.
	_, _ = a.Store.DB.Exec(`UPDATE accounts SET secret='replacement',status='ready',error='' WHERE id='a'`)
	_, _ = c.Get(context.Background(), "remote-movie")
	ac, _ = a.Store.Account("a")
	if ac.Status != "ready" {
		t.Fatal("stale callback overwrote updated credentials")
	}
}

func TestTaskStatusAccountErrorsAreConservative(t *testing.T) {
	for _, code := range []string{"not_enough_space", "quota_exceeded", "user_blocked", `{"error":"quota_exceeded"}`} {
		err := taskAccountError(pikpak.Task{Params: pikpak.TaskParams{ErrorDetail: code}})
		if err == nil {
			t.Fatal("missed explicit task error", code)
		}
	}
	for _, message := range []string{"share expired", "file not found", "some title contains user_blocked", "403 Forbidden", "network_error"} {
		if taskAccountError(pikpak.Task{Message: message}) != nil {
			t.Fatal("source error disabled account", message)
		}
	}
}

func TestUnhealthyAccountThumbnailDoesNotRetryLogin(t *testing.T) {
	a, f := testApp(t)
	sessionForTest(t, a)
	_ = a.Store.InsertNode(Node{ID: "thumb", ParentID: "root", Kind: "file", Name: "thumb.mp4"})
	_ = a.Store.Bind("a", "thumb", "remote-thumb", "present", "thumb.mp4", "remote-root", "")
	_, _ = a.Store.DB.Exec(`UPDATE accounts SET status='blocked' WHERE id='a'`)
	for i := 0; i < 3; i++ {
		w := request(t, a.Handler(nil), "GET", "/api/v1/files/thumb/thumbnail", nil, "")
		if w.Code != 409 || !strings.Contains(w.Body.String(), "冻结") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if f.calls["get"] != 0 {
		t.Fatal("thumbnail retried unavailable account")
	}
}

func TestCaptchaOnlyCredentialUpdateRetainsDeviceAndChecksIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name, device, identity string
	}{
		{"new-device", "verified-device", "user-a"},
		{"existing-device", "", "user-a"},
		{"wrong-account", "verified-device", "user-b"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			a, _ := testApp(t)
			sessionForTest(t, a)
			previous := pikpak.Credentials{Username: "original-user", Password: "original-password", AccessToken: "original-access", RefreshToken: "original-refresh", UserID: "user-a", DeviceID: "original-device", CaptchaToken: "expired-captcha", Expiry: now() + 3600}
			secret, err := a.Store.Seal(previous)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.Store.DB.Exec(`UPDATE accounts SET secret=?,status='verification_required',verification_url='https://mypikpak.com/verify/old' WHERE id='a'`, secret); err != nil {
				t.Fatal(err)
			}
			wantDevice := scenario.device
			if wantDevice == "" {
				wantDevice = previous.DeviceID
			}
			requests := 0
			transport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Header.Get("X-Device-ID") != wantDevice || r.Header.Get("X-Captcha-Token") != "verified-captcha" || r.Header.Get("Authorization") != "Bearer original-access" {
					t.Error("credential verification did not use supplied captcha/device with original access token")
				}
				w := httptest.NewRecorder()
				switch r.URL.Path {
				case "/v1/user/me":
					_ = json.NewEncoder(w).Encode(map[string]string{"sub": scenario.identity})
				case "/drive/v1/about":
					_, _ = w.Write([]byte(`{"quota":{"limit":"1000","usage":"10"}}`))
				default:
					t.Error("unexpected credential request", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
				return w.Result(), nil
			})
			t.Cleanup(func() { http.DefaultTransport = transport })
			w := request(t, a.Handler(nil), "PATCH", "/api/v1/accounts/a", map[string]string{"captcha_token": "verified-captcha", "device_id": scenario.device}, "test-csrf")
			account, err := a.Store.Account("a")
			if err != nil {
				t.Fatal(err)
			}
			if scenario.identity != "user-a" {
				if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "identity_changed") || requests != 1 || account.Secret != secret || account.Status != "verification_required" || account.VerificationURL == "" {
					t.Fatalf("mismatched credentials were accepted or previous state lost: %d %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusOK || requests != 2 || account.Status != "ready" || account.VerificationURL != "" {
				t.Fatalf("credential verification failed: %d %s", w.Code, w.Body.String())
			}
			var saved pikpak.Credentials
			if err := a.Store.Unseal(account.Secret, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.DeviceID != wantDevice || saved.CaptchaToken != "verified-captcha" || saved.CaptchaExpiry <= now() || saved.UserID != previous.UserID || saved.Username != previous.Username || saved.Password != previous.Password || saved.AccessToken != previous.AccessToken || saved.RefreshToken != previous.RefreshToken {
				t.Fatal("verified credentials were not retained correctly")
			}
		})
	}
}

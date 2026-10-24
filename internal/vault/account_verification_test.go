package vault

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"pikpakvault/internal/pikpak"
)

func TestVerificationURLBoundaries(t *testing.T) {
	r := httptest.NewRequest("POST", "http://vault.example/api/v1/accounts/a/verification", nil)
	r.Header.Set("Origin", "https://vault.example")
	if _, err := verificationCallback(r, "https://vault.example/account-verification"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"https://evil.example/account-verification", "http://vault.example/account-verification", "https://vault.example/other", "https://vault.example/account-verification?next=evil", "https://vault.example/account-verification#token", "https://user@vault.example/account-verification", "https://vault.example/%61ccount-verification"} {
		if _, err := verificationCallback(r, value); err == nil {
			t.Fatal("accepted unsafe callback", value)
		}
	}
	for _, value := range []string{"http://user.mypikpak.com/captcha", "https://user.mypikpak.com.evil.example/captcha", "https://untrusted.mypikpak.com/captcha", "https://user.mypikpak.com:8443/captcha", "https://user@user.mypikpak.com/captcha", "https://user.mypikpak.com/captcha#value"} {
		if _, err := officialVerificationURL(value, "https://vault.example/account-verification", "state"); err == nil {
			t.Fatal("accepted unsafe upstream", value)
		}
	}
	for _, host := range []string{"mypikpak.com", "user.mypikpak.com", "mypikpak.net", "user.mypikpak.net"} {
		link, err := officialVerificationURL("https://"+host+"/captcha?redirect_uri=old&state=old", "https://vault.example/account-verification", "new-state")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(link)
		if u.Query().Get("state") != "new-state" || u.Query().Get("redirect_uri") != "https://vault.example/account-verification" {
			t.Fatal(link)
		}
	}
}

func TestAccountVerificationRequiresAuthenticationAndCSRF(t *testing.T) {
	a, _ := testApp(t)
	sessionForTest(t, a)
	for _, path := range []string{"/api/v1/accounts/a/verification", "/api/v1/accounts/verification/complete"} {
		w := httptest.NewRecorder()
		a.Handler(nil).ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
		if w.Code != 401 {
			t.Fatal(path, w.Code)
		}
		if w := request(t, a.Handler(nil), "POST", path, map[string]any{}, ""); w.Code != 403 {
			t.Fatal(path, w.Code)
		}
	}
}

func TestAccountVerificationRoundTrip(t *testing.T) {
	for _, scenario := range []string{"success", "expired", "stale", "wrong-identity", "empty-identity", "wrong-session", "invalid-token", "quota", "missing-root", "missing-root-running", "changed-during-validation", "signin"} {
		t.Run(scenario, func(t *testing.T) {
			a, _ := testApp(t)
			a.Factory = nil
			sessionForTest(t, a)
			creds := pikpak.Credentials{AccessToken: "access", RefreshToken: "refresh", Username: "user", Password: "password", DeviceID: "device", UserID: "user-a", CaptchaToken: "old", Expiry: now() + 3600}
			if scenario == "signin" {
				creds.AccessToken = ""
				creds.RefreshToken = ""
				creds.Expiry = 0
			}
			sealed, err := a.Store.Seal(creds)
			if err != nil {
				t.Fatal(err)
			}
			root := "remote-root"
			if strings.HasPrefix(scenario, "missing-root") {
				root = ""
			}
			if _, err := a.Store.DB.Exec(`UPDATE accounts SET secret=?,root_id=?,status='verification_required' WHERE id='a'`, sealed, root); err != nil {
				t.Fatal(err)
			}
			job, _ := a.Store.NewJob("a", "recover", "original task", JobData{})
			_, _ = a.Store.DB.Exec(`UPDATE jobs SET state='paused',attempts=2 WHERE id=?`, job.ID)
			if scenario == "missing-root-running" {
				verify, _ := a.Store.NewJob("a", "verify", "old verification", JobData{})
				_, _ = a.Store.DB.Exec(`UPDATE jobs SET state='running' WHERE id=?`, verify.ID)
			}
			captchaCalls, meCalls, quotaCalls, signinCalls := 0, 0, 0, 0
			originalTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				switch r.URL.Path {
				case "/v1/shield/captcha/init":
					captchaCalls++
					_, _ = w.Write([]byte(`{"captcha_token":"pending-captcha","expires_in":300,"url":"https://user.mypikpak.com/captcha?client_id=client"}`))
				case "/v1/auth/signin":
					signinCalls++
					if r.Header.Get("X-Captcha-Token") != "completed-captcha" {
						t.Error("completed signin token was overwritten")
					}
					_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","sub":"user-a","expires_in":3600}`))
				case "/v1/user/me":
					meCalls++
					if r.Header.Get("X-Captcha-Token") != "completed-captcha" || r.Header.Get("X-Device-ID") != "device" {
						t.Error("completed token/device not preserved")
					}
					if scenario == "invalid-token" {
						w.WriteHeader(403)
						_, _ = w.Write([]byte(`{"error":"captcha_invalid_token"}`))
						break
					}
					identity := "user-a"
					if scenario == "wrong-identity" {
						identity = "user-b"
					}
					if scenario == "empty-identity" {
						identity = ""
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"sub": identity})
				case "/drive/v1/about":
					quotaCalls++
					used := 10
					if scenario == "quota" {
						used = 100
					}
					if scenario == "changed-during-validation" {
						_, _ = a.Store.DB.Exec(`UPDATE accounts SET secret='newer-credentials' WHERE id='a'`)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"quota": map[string]int{"limit": 100, "usage": used}})
				default:
					t.Error("unexpected endpoint", r.URL.Path)
					w.WriteHeader(404)
				}
				return w.Result(), nil
			})
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			h := a.Handler(nil)
			w := request(t, h, "POST", "/api/v1/accounts/a/verification", map[string]string{"callback_url": "http://example.com/account-verification"}, "test-csrf")
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var prepared struct {
				URL     string `json:"url"`
				Expires int64  `json:"expires_at"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &prepared); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(prepared.URL)
			stateValue := u.Query().Get("state")
			if len(stateValue) < 32 || u.Query().Get("redirect_uri") != "http://example.com/account-verification" || prepared.Expires <= now() || prepared.Expires > now()+600 {
				t.Fatal("invalid challenge response")
			}
			account, _ := a.Store.Account("a")
			if account.Status != "verification_required" || captchaCalls != 1 {
				t.Fatal("preparing challenge changed health")
			}
			secretBefore := account.Secret
			if again := request(t, h, "POST", "/api/v1/accounts/a/verification", map[string]string{"callback_url": "http://example.com/account-verification"}, "test-csrf"); again.Code != 429 || captchaCalls != 1 {
				t.Fatal("verification requests were not cooled down", again.Code)
			}
			key := verificationStatePrefix + tokenHash(stateValue)
			stored := a.Store.Get(key)
			if stored == "" || strings.Contains(stored, stateValue) || strings.Contains(stored, "device") {
				t.Fatal("state missing or not encrypted")
			}
			if scenario == "expired" || scenario == "wrong-session" {
				var state accountVerificationState
				_ = a.Store.Unseal(stored, &state)
				if scenario == "expired" {
					state.Expires = now() - 1
				} else {
					state.SessionID = "different-session"
				}
				sealed, _ := a.Store.Seal(state)
				_ = a.Store.Set(key, sealed)
			}
			if scenario == "stale" {
				_, _ = a.Store.DB.Exec(`UPDATE accounts SET secret='newer-credentials' WHERE id='a'`)
			}
			body := map[string]any{"state": stateValue, "captcha_token": "completed-captcha", "expires_in": 300}
			w = request(t, h, "POST", "/api/v1/accounts/verification/complete", body, "test-csrf")
			account, _ = a.Store.Account("a")
			want := 200
			switch scenario {
			case "expired", "stale", "changed-during-validation":
				want = 409
			case "wrong-identity", "empty-identity", "invalid-token":
				want = 502
			case "wrong-session":
				want = 403
			}
			if w.Code != want {
				t.Fatal(scenario, w.Code, w.Body.String())
			}
			if want == 200 {
				status := "ready"
				if scenario == "quota" {
					status = "quota_exceeded"
				}
				if strings.HasPrefix(scenario, "missing-root") {
					status = "unverified"
				}
				if account.Status != status || account.VerificationURL != "" || account.Identity != "user-a" || meCalls != 1 || quotaCalls != 1 {
					t.Fatal("account not verified correctly", account.Status)
				}
				var saved pikpak.Credentials
				_ = a.Store.Unseal(account.Secret, &saved)
				if saved.CaptchaToken != "completed-captcha" || !saved.CaptchaVerified || saved.DeviceID != "device" {
					t.Fatal("verified token/device not persisted")
				}
				if scenario == "signin" && (signinCalls != 1 || captchaCalls != 1 || saved.AccessToken != "new-access") {
					t.Fatal("signin replaced completed token")
				}
				if strings.HasPrefix(scenario, "missing-root") {
					var count int
					_ = a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='verify' AND state='queued'`).Scan(&count)
					if count != 1 {
						t.Fatal("root initialization missing")
					}
				}
			} else {
				if account.Status != "verification_required" {
					t.Fatal("failed validation made account healthy")
				}
				if scenario == "stale" || scenario == "changed-during-validation" {
					if account.Secret != "newer-credentials" {
						t.Fatal("new credentials overwritten")
					}
				} else if account.Secret != secretBefore {
					t.Fatal("failed verification overwrote existing credentials")
				}
				if (scenario == "expired" || scenario == "stale" || scenario == "wrong-session") && meCalls != 0 {
					t.Fatal("expired or stale callback reached upstream")
				}
			}
			retained, _ := a.Store.Job(job.ID)
			if retained.State != "paused" || retained.Attempts != 2 {
				t.Fatal("verification automatically resumed existing task")
			}
			replayStatus := 409
			if scenario == "wrong-session" {
				replayStatus = 403
			}
			if replay := request(t, h, "POST", "/api/v1/accounts/verification/complete", body, "test-csrf"); replay.Code != replayStatus {
				t.Fatal("state replay accepted", replay.Code)
			}
		})
	}
}

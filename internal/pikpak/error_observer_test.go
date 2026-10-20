package pikpak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueuedRequestRechecksHealthBeforeLogin(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	requests := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"user_blocked"}`))
	}))
	defer server.Close()
	c := New(Credentials{RefreshToken: "refresh-secret"})
	c.UserURL, c.DriveURL = server.URL, server.URL
	c.RequestInterval, c.WriteInterval = 0, 0
	blocked := atomic.Bool{}
	guardError := errors.New("account paused")
	c.BeforeRequest = func(ctx context.Context) error {
		if blocked.Load() {
			return guardError
		}
		return nil
	}
	c.OnError = func(err error) {
		var up *APIError
		if errors.As(err, &up) && up.Code == "user_blocked" {
			blocked.Store(true)
		}
	}
	firstResult, secondResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), "one")
		firstResult <- err
	}()
	<-firstEntered
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		_, err := c.Get(context.Background(), "two")
		secondResult <- err
	}()
	<-secondStarted
	close(releaseFirst)
	select {
	case err := <-firstResult:
		var up *APIError
		if !errors.As(err, &up) || up.Code != "user_blocked" {
			t.Fatalf("first request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request deadlocked")
	}
	select {
	case err := <-secondResult:
		if !errors.Is(err, guardError) {
			t.Fatalf("queued request bypassed updated health: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request deadlocked")
	}
	if requests.Load() != 1 {
		t.Fatalf("queued request repeated login after account failure: %d requests", requests.Load())
	}
}

func TestBeforeRequestReceivesExplicitProbeContext(t *testing.T) {
	type probeKey struct{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"quota":{"limit":"1024"}}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access-secret", CaptchaToken: "captcha-secret"})
	c.DriveURL = server.URL
	c.BeforeRequest = func(ctx context.Context) error {
		if ctx.Value(probeKey{}) != true {
			return errors.New("account paused")
		}
		return nil
	}
	if _, err := c.Quota(context.Background()); err == nil {
		t.Fatal("normal request bypassed health guard")
	}
	if _, err := c.Quota(context.WithValue(context.Background(), probeKey{}, true)); err != nil {
		t.Fatalf("explicit probe context was lost: %v", err)
	}
}

func TestBlockedRefreshDoesNotRetryPasswordAndReportsOnce(t *testing.T) {
	for _, code := range []string{"user_blocked", "account_frozen", "user_disabled", "account_suspended", "account_banned"} {
		t.Run(code, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/v1/auth/token" {
					t.Errorf("blocked refresh attempted another endpoint: %s", r.URL.Path)
				}
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "account unavailable"})
			}))
			defer server.Close()
			c := New(Credentials{Username: "someone@example.test", Password: "private-password", RefreshToken: "refresh-secret"})
			c.UserURL, c.DriveURL = server.URL, server.URL
			c.RequestInterval, c.WriteInterval = 0, 0
			var reported []error
			c.OnError = func(err error) { reported = append(reported, err) }
			_, err := c.Quota(context.Background())
			var up *APIError
			if !errors.As(err, &up) || up.Code != code || requests != 1 || len(reported) != 1 || reported[0] != err {
				t.Fatalf("unexpected final account error: %v; requests=%d; callbacks=%d", err, requests, len(reported))
			}
		})
	}
}

func TestObserverOnlyReportsFinalAuthenticationFailure(t *testing.T) {
	for _, failAgain := range []bool{false, true} {
		name := "refresh-success"
		if failAgain {
			name = "refresh-then-final-auth-failure"
		}
		t.Run(name, func(t *testing.T) {
			requests, attempts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch r.URL.Path {
				case "/v1/auth/token":
					_, _ = w.Write([]byte(`{"access_token":"new-access","expires_in":3600}`))
				case "/drive/v1/about":
					attempts++
					if attempts == 1 || failAgain {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = w.Write([]byte(`{"error":"unauthenticated"}`))
						return
					}
					_, _ = w.Write([]byte(`{"quota":{"limit":"1024","usage":"512"}}`))
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			c := New(Credentials{AccessToken: "old-access", RefreshToken: "refresh-secret", CaptchaToken: "captcha-secret"})
			c.UserURL, c.DriveURL = server.URL, server.URL
			c.RequestInterval, c.WriteInterval = 0, 0
			var reported []error
			c.OnError = func(err error) { reported = append(reported, err) }
			quota, err := c.Quota(context.Background())
			if requests != 3 || attempts != 2 {
				t.Fatalf("unexpected retry count: requests=%d attempts=%d", requests, attempts)
			}
			if failAgain {
				if err == nil || len(reported) != 1 || reported[0] != err {
					t.Fatalf("final error not reported exactly once: %v (%d callbacks)", err, len(reported))
				}
			} else if err != nil || quota.Limit != 1024 || len(reported) != 0 {
				t.Fatalf("recovered authentication reported as failure: %v (%d callbacks)", err, len(reported))
			}
		})
	}
}

func TestObserverDoesNotReportRecoveredCaptcha(t *testing.T) {
	attempts, reported := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/shield/captcha/init" {
			_, _ = w.Write([]byte(`{"captcha_token":"new-captcha","expires_in":300}`))
			return
		}
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"captcha_token_expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"quota":{"limit":"1024","usage":"512"}}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access-secret", CaptchaToken: "old-captcha"})
	c.UserURL, c.DriveURL = server.URL, server.URL
	c.RequestInterval, c.WriteInterval = 0, 0
	c.OnError = func(error) { reported++ }
	if _, err := c.Quota(context.Background()); err != nil || attempts != 2 || reported != 0 {
		t.Fatalf("captcha retry reported as failure: %v (%d attempts, %d callbacks)", err, attempts, reported)
	}
}

func TestFinalSignInErrorIsReportedOnce(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/v1/shield/captcha/init":
			_, _ = w.Write([]byte(`{"captcha_token":"captcha-secret","expires_in":300}`))
		case "/v1/auth/signin":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_credentials","error_description":"credentials rejected"}`))
		default:
			t.Errorf("unexpected endpoint after sign-in failure: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c := New(Credentials{Username: "someone@example.test", Password: "private-password"})
	c.UserURL, c.DriveURL = server.URL, server.URL
	c.RequestInterval, c.WriteInterval = 0, 0
	var reported []error
	c.OnError = func(err error) { reported = append(reported, err) }
	_, err := c.Quota(context.Background())
	var up *APIError
	if !errors.As(err, &up) || up.Code != "invalid_credentials" || requests != 2 || len(reported) != 1 || reported[0] != err {
		t.Fatalf("sign-in error not reported once: %v (%d requests, %d callbacks)", err, requests, len(reported))
	}
}

func TestBlockedRequestDoesNotTriggerTokenRefresh(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/drive/v1/about" {
			t.Errorf("account restriction triggered authentication request: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"user_blocked"}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access-secret", RefreshToken: "refresh-secret", CaptchaToken: "captcha-secret"})
	c.UserURL, c.DriveURL = server.URL, server.URL
	c.RequestInterval, c.WriteInterval = 0, 0
	reported := 0
	c.OnError = func(error) { reported++ }
	if _, err := c.Quota(context.Background()); err == nil || requests != 1 || reported != 1 {
		t.Fatalf("blocked account retried: %v (%d requests, %d callbacks)", err, requests, reported)
	}
}

func TestRefreshRejectionCanStillFallBackToPassword(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/v1/auth/token":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		case "/v1/shield/captcha/init":
			_, _ = w.Write([]byte(`{"captcha_token":"captcha-secret","expires_in":300}`))
		case "/v1/auth/signin":
			_, _ = w.Write([]byte(`{"access_token":"new-access","expires_in":3600}`))
		case "/drive/v1/about":
			_, _ = w.Write([]byte(`{"quota":{"limit":"1024"}}`))
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c := New(Credentials{Username: "someone@example.test", Password: "private-password", RefreshToken: "old-refresh"})
	c.UserURL, c.DriveURL = server.URL, server.URL
	c.RequestInterval, c.WriteInterval = 0, 0
	c.OnError = func(err error) { t.Errorf("successful fallback reported: %v", err) }
	if _, err := c.Quota(context.Background()); err != nil || requests != 4 {
		t.Fatalf("password fallback failed: %v (%d requests)", err, requests)
	}
}

func TestObserverIncludesUploadTicketAndShareReadErrors(t *testing.T) {
	for _, operation := range []string{"upload", "share"} {
		t.Run(operation, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"storage_quota_exceeded"}`))
			}))
			defer server.Close()
			c := New(Credentials{AccessToken: "access-secret", CaptchaToken: "captcha-secret"})
			c.DriveURL = server.URL
			var reported []error
			c.OnError = func(err error) { reported = append(reported, err) }
			var err error
			if operation == "upload" {
				_, err = c.BeginUpload(context.Background(), "target", File{Name: "movie.mp4", Size: 123})
			} else {
				_, err = c.Share(context.Background(), "share-id", "", "", "", "")
			}
			if err == nil || len(reported) != 1 || reported[0] != err {
				t.Fatalf("failure not observed: %v (%d callbacks)", err, len(reported))
			}
		})
	}
}

func TestErrorCodeAndMessageAreRedactedBeforeObservation(t *testing.T) {
	for _, operation := range []string{"share-query", "share-body"} {
		t.Run(operation, func(t *testing.T) {
			const echo = "private-password access-secret refresh-secret captcha-secret private-extraction-code"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": echo, "error_description": echo})
			}))
			defer server.Close()
			c := New(Credentials{Password: "private-password", AccessToken: "access-secret", RefreshToken: "refresh-secret", CaptchaToken: "captcha-secret"})
			c.DriveURL = server.URL
			var observed error
			c.OnError = func(err error) { observed = err }
			if operation == "share-query" {
				_, _ = c.Share(context.Background(), "share-id", "private-extraction-code", "", "", "")
			} else {
				_, _ = c.RestoreShare(context.Background(), "share-id", "private-extraction-code", []string{"one"}, "parent")
			}
			if observed == nil {
				t.Fatal("missing error")
			}
			for _, secret := range strings.Fields(echo) {
				if strings.Contains(observed.Error(), secret) {
					t.Fatalf("error observer exposed %q", secret)
				}
			}
		})
	}
}

func TestDecodeErrorRetainsExplicitCodesWithoutGuessing(t *testing.T) {
	for _, tc := range []struct {
		body string
		code string
	}{
		{`{"error":"user_blocked","error_code":4001}`, "user_blocked"},
		{`{"error_code":"account_frozen"}`, "account_frozen"},
		{`{"error_code":4001}`, "4001"},
		{`{"error":4002}`, "4002"},
		{`{"error_code":0}`, "upstream_error"},
		{`{"error_code":null}`, "upstream_error"},
		{`{"error":{"detail":"not-a-machine-code"}}`, "upstream_error"},
	} {
		if got := DecodeError([]byte(tc.body), 400, "/test"); got.Code != tc.code {
			t.Errorf("%s decoded as %q, want %q", tc.body, got.Code, tc.code)
		}
	}
}

func TestShareErrorsRedactBeforeTruncatingOverlappingSecrets(t *testing.T) {
	const extractionCode = "private-extraction-code"
	echo := strings.Repeat("x", 390) + extractionCode
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": echo, "error_description": echo})
	}))
	defer server.Close()
	// A credential is deliberately a prefix of the longer extraction code.
	c := New(Credentials{AccessToken: "access-secret", CaptchaToken: "captcha-secret", Password: "private-"})
	c.DriveURL = server.URL
	_, err := c.Share(context.Background(), "share-id", extractionCode, "", "", "")
	if err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "extraction") {
		t.Fatalf("partially redacted secret: %v", err)
	}
}

func TestNonzeroErrorCodeInSuccessfulHTTPResponseIsObserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error_code":"account_frozen","error_description":"restricted"}`))
	}))
	defer server.Close()
	c := New(Credentials{AccessToken: "access-secret", CaptchaToken: "captcha-secret"})
	c.DriveURL = server.URL
	reported := 0
	c.OnError = func(error) { reported++ }
	_, err := c.Quota(context.Background())
	var up *APIError
	if !errors.As(err, &up) || up.Code != "account_frozen" || reported != 1 {
		t.Fatalf("application-level error hidden: %v (%d callbacks)", err, reported)
	}
}

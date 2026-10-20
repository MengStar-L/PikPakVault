package vault

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"pikpakvault/internal/pikpak"
)

type healthDrive struct {
	*fakeDrive
	meError, quotaError error
	quota               *pikpak.Quota
	onMe                func()
}

func (f *healthDrive) Me(ctx context.Context) (pikpak.Identity, error) {
	if f.onMe != nil {
		f.onMe()
	}
	if f.meError != nil {
		return pikpak.Identity{}, f.meError
	}
	return f.fakeDrive.Me(ctx)
}

func (f *healthDrive) Quota(ctx context.Context) (pikpak.Quota, error) {
	if f.quotaError != nil {
		return pikpak.Quota{}, f.quotaError
	}
	if f.quota != nil {
		return *f.quota, nil
	}
	return f.fakeDrive.Quota(ctx)
}

func TestAccountHealthClassifiesOnlyAccountWideFailures(t *testing.T) {
	cases := []struct {
		name, want string
		err        error
	}{
		{"frozen", "blocked", &pikpak.APIError{Status: 400, Code: "user_blocked"}},
		{"wrapped frozen", "blocked", fmt.Errorf("wrapped: %w", &pikpak.APIError{Status: 400, Code: "user-blocked"})},
		{"login", "auth_required", &pikpak.APIError{Status: 401}},
		{"password", "auth_required", &pikpak.APIError{Status: 400, Code: "invalid_password"}},
		{"account password", "auth_required", &pikpak.APIError{Status: 400, Code: "invalid_account_or_password"}},
		{"unknown signin rejection", "auth_required", &pikpak.APIError{Status: 400, Endpoint: "/v1/auth/signin", Code: "new_error_code"}},
		{"unknown token rejection", "auth_required", &pikpak.APIError{Status: 403, Endpoint: "/v1/auth/token", Code: "new_error_code"}},
		{"network signin", "", &pikpak.APIError{Endpoint: "/v1/auth/signin", Code: "network_error"}},
		{"temporary signin", "", &pikpak.APIError{Status: 503, Endpoint: "/v1/auth/signin", Code: "temporarily_unavailable"}},
		{"quota", "quota_exceeded", &pikpak.APIError{Status: 403, Code: "storage_quota_exceeded"}},
		{"verification", "verification_required", &pikpak.APIError{Status: 403, Code: "verification_required"}},
		{"source forbidden", "", &pikpak.APIError{Status: 403, Endpoint: "/drive/v1/share", Code: "forbidden"}},
		{"missing", "", &pikpak.APIError{Status: 404, Code: "file_not_found"}},
		{"transient", "", &pikpak.APIError{Status: 503, Code: "temporarily_unavailable"}},
		{"network", "", &pikpak.APIError{Code: "network_error"}},
		{"rate limit", "", &pikpak.APIError{Status: 429}},
		{"upload credentials", "", &pikpak.APIError{Status: 401, Endpoint: "upload/storage", Code: "AccessDenied"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			status, message, _ := classifyAccountError(tt.err)
			if status != tt.want || status != "" && message == "" {
				t.Fatalf("status=%q message=%q", status, message)
			}
		})
	}
	_, message, verifyURL := classifyAccountError(&pikpak.APIError{Code: "verification_required", Message: "password=secret token=private", VerificationURL: "javascript:alert(1)"})
	if strings.Contains(message, "secret") || verifyURL != "" {
		t.Fatal("unsafe upstream detail was exposed")
	}
}

func TestAccountHealthVerificationFailuresPauseAndPreserve(t *testing.T) {
	for _, tc := range []struct {
		code, want string
		status     int
	}{
		{"user_blocked", "blocked", 400},
		{"invalid_password", "auth_required", 400},
		{"quota_exceeded", "quota_exceeded", 403},
		{"verification_required", "verification_required", 403},
		{"network_error", "error", 0},
	} {
		t.Run(tc.code, func(t *testing.T) {
			a, f := testApp(t)
			w := &healthDrive{fakeDrive: f, meError: &pikpak.APIError{Status: tc.status, Code: tc.code, Message: "token=do-not-expose"}}
			a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
			queued := createImport(t, a, "magnet")
			queued.State, queued.Attempts, queued.Progress = "retry", 4, 40
			d := JobData{Transfers: map[string]*TransferState{"saved": {TaskID: "remote-task", OutputIDs: []string{"remote-file"}, Phase: "submitted"}}}
			if err := a.Store.SaveJob(&queued, &d); err != nil {
				t.Fatal(err)
			}
			original := string(queued.Data)
			j, _ := a.Store.NewJob("a", "verify", "verify", JobData{})
			execute(t, a, &j)
			ac, _ := a.Store.Account("a")
			after, _ := a.Store.Job(queued.ID)
			if ac.Status != tc.want || ac.Error == "" || strings.Contains(ac.Error, "do-not-expose") {
				t.Fatalf("account: %+v", ac)
			}
			if after.State != "paused" || after.Attempts != 4 || after.Progress != 40 || string(after.Data) != original {
				t.Fatalf("task/session changed: %+v", after)
			}
			if j.Attempts != 0 || j.State != "paused" && j.State != "failed" {
				t.Fatalf("verification unexpectedly retries: %+v", j)
			}
			var events int
			a.Store.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='account'`).Scan(&events)
			if events == 0 {
				t.Fatal("account SSE not emitted")
			}
			// New scheduler ticks never generate repeated failed scans.
			a.Store.Set("scan_minutes", "30")
			a.scheduleScan()
			var scheduled int
			a.Store.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind='scan'`).Scan(&scheduled)
			if tc.want != "quota_exceeded" && scheduled != 0 {
				t.Fatal("scheduled scan on blocked account")
			}
		})
	}
}

func TestAccountHealthQuotaAllowsCleanupAndRequiresExplicitVerify(t *testing.T) {
	a, f := testApp(t)
	var jobs []Job
	for _, kind := range []string{"import", "recover", "teldrive_upload", "rss_scan", "scan", "trash", "sync", "cleanup"} {
		j, _ := a.Store.NewJob("a", kind, kind, JobData{})
		jobs = append(jobs, j)
	}
	a.recordAccountQuota("a", pikpak.Quota{Limit: 100, Usage: 100})
	for _, j := range jobs {
		saved, _ := a.Store.Job(j.ID)
		wantPaused := !accountStatusAllows("quota_exceeded", j.Kind)
		if (saved.State == "paused") != wantPaused {
			t.Fatalf("%s state=%s", j.Kind, saved.State)
		}
	}
	a.recordAccountQuota("a", pikpak.Quota{Limit: 100, Usage: 40})
	ac, _ := a.Store.Account("a")
	if ac.Status != "quota_exceeded" {
		t.Fatal("background quota response cleared health")
	}
	a.observeAccountError("a", &pikpak.APIError{Code: "user_blocked"})
	a.recordAccountQuota("a", pikpak.Quota{Limit: 100, Usage: 100})
	ac, _ = a.Store.Account("a")
	if ac.Status != "blocked" {
		t.Fatal("quota overwrote frozen status")
	}
	a.Factory = func(Account) (pikpak.Provider, error) { return f, nil }
	j, _ := a.Store.NewJob("a", "verify", "verify", JobData{})
	requireComplete(t, execute(t, a, &j))
	ac, _ = a.Store.Account("a")
	if ac.Status != "ready" || ac.Error != "" {
		t.Fatal("successful verify did not clear health")
	}
	for _, j := range jobs {
		saved, _ := a.Store.Job(j.ID)
		if saved.State != "paused" {
			t.Fatal("verification resumed paused task without user review", j.Kind)
		}
	}
}

func TestAccountHealthFullQuotaVerificationSkipsRootWrites(t *testing.T) {
	a, f := testApp(t)
	w := &healthDrive{fakeDrive: f, quota: &pikpak.Quota{Limit: 100, Usage: 101}}
	a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
	a.observeAccountError("a", &pikpak.APIError{Code: "user_blocked"})
	j, _ := a.Store.NewJob("a", "verify", "verify", JobData{})
	execute(t, a, &j)
	ac, _ := a.Store.Account("a")
	if ac.Status != "quota_exceeded" || ac.Used != 101 || ac.Error == "" || f.calls["mkdir"] != 0 {
		t.Fatalf("quota verification incorrect: %+v calls=%v", ac, f.calls)
	}
}

func TestAccountHealthLateFailureStaysBoundAndIgnoresOldCredentials(t *testing.T) {
	a, f := testApp(t)
	addAccount(t, a, "b", "user-b")
	old, _ := a.Store.Account("a")
	w := &healthDrive{fakeDrive: f, meError: &pikpak.APIError{Status: 400, Code: "user_blocked"}, onMe: func() {
		a.Store.Set("active_account", "b")
	}}
	a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
	j, _ := a.Store.NewJob("a", "verify", "verify", JobData{})
	execute(t, a, &j)
	ac, _ := a.Store.Account("a")
	b, _ := a.Store.Account("b")
	if ac.Status != "blocked" || b.Status != "ready" {
		t.Fatalf("late failure affected wrong account: %s %s", ac.Status, b.Status)
	}
	secret, _ := a.Store.Seal(pikpak.Credentials{AccessToken: "replacement"})
	a.Store.DB.Exec(`UPDATE accounts SET secret=?,status='ready',error='' WHERE id='a'`, secret)
	if a.observeAccountErrorIfCurrent("a", old.Secret, w.meError) {
		t.Fatal("old client fault was accepted")
	}
	ac, _ = a.Store.Account("a")
	if ac.Status != "ready" {
		t.Fatal("old response overwrote new credentials")
	}
	// Persisted fault state is authoritative after a process restart.
	a.observeAccountError("a", w.meError)
	restarted := NewApp(a.Store)
	if restarted.accountJobAllowed("a", "import") || !restarted.accountJobAllowed("a", "verify") {
		t.Fatal("restart bypassed health gate")
	}
}

func TestAccountHealthSourceFailureDoesNotDisableAccount(t *testing.T) {
	a, f := testApp(t)
	w := &alteredDrive{fakeDrive: f, offlineError: &pikpak.APIError{Status: 403, Code: "source_forbidden"}}
	a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
	j := createImport(t, a, "magnet")
	execute(t, a, &j)
	ac, _ := a.Store.Account("a")
	if ac.Status != "ready" || j.State != "failed" {
		t.Fatal(ac.Status, j.State, j.Message)
	}
}

func TestAccountHealthScanQuotaFailureIsNotSuccessful(t *testing.T) {
	a, f := testApp(t)
	if _, err := a.ensureRoot(context.Background(), f, "a"); err != nil {
		t.Fatal(err)
	}
	w := &healthDrive{fakeDrive: f, quotaError: &pikpak.APIError{Status: 400, Code: "user_blocked"}}
	a.Factory = func(Account) (pikpak.Provider, error) { return w, nil }
	j, _ := a.Store.NewJob("a", "scan", "scan", JobData{})
	execute(t, a, &j)
	ac, _ := a.Store.Account("a")
	if ac.Status != "blocked" || j.State != "paused" {
		t.Fatal(ac.Status, j.State, j.Message)
	}
}

func TestAccountHealthClientGateAndConcurrentCredentialRefresh(t *testing.T) {
	a, _ := testApp(t)
	a.Factory = nil
	provider, err := a.client("a")
	if err != nil {
		t.Fatal(err)
	}
	c := provider.(*pikpak.Client)
	a.observeAccountError("a", &pikpak.APIError{Code: "user_blocked"})
	if err = c.BeforeRequest(context.Background()); err == nil {
		t.Fatal("queued request bypassed frozen account")
	}
	if err = c.BeforeRequest(context.WithValue(context.Background(), accountProbeKey{}, "a")); err != nil {
		t.Fatal("explicit verification was blocked", err)
	}
	if err = c.BeforeRequest(context.WithValue(context.Background(), accountProbeKey{}, "b")); err == nil {
		t.Fatal("another account's verification bypassed the gate")
	}
	a.Store.DB.Exec(`UPDATE accounts SET status='ready',error='' WHERE id='a'`)
	var wg sync.WaitGroup
	errs := make(chan error, 80)
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := c.Save(pikpak.Credentials{AccessToken: fmt.Sprint("refreshed-", i)}); err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			unlock, err := c.BeforeWrite()
			if err != nil {
				errs <- err
			} else {
				unlock()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			c.OnError(&pikpak.APIError{Status: 403, Code: "source_forbidden"})
			if err := c.BeforeRequest(context.Background()); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error("credential refresh produced an inconsistent guard", err)
	}
}

package vault

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"pikpakvault/internal/pikpak"
)

type accountProbeKey struct{}

// Only explicit account-wide errors affect account health. A forbidden share,
// expired download URL or unavailable file must not disable the whole account.
func classifyAccountError(err error) (status, message, verificationURL string) {
	var up *pikpak.APIError
	if !errors.As(err, &up) {
		return "", "", ""
	}
	if up.Endpoint == "upload/storage" {
		return "", "", ""
	}
	code := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(up.Code)), "-", "_")
	switch code {
	case "user_blocked", "user_banned", "account_blocked", "account_banned", "account_frozen", "user_frozen", "account_disabled", "user_disabled", "account_suspended", "user_suspended":
		return "blocked", "PikPak 账号已被冻结或停用。请到 PikPak 官方客户端检查账号状态，解封后重新验证，或切换其他账号。", ""
	case "verification_required", "captcha_required", "captcha_invalid", "captcha_token_expired", "captcha_invalid_token":
		if u, err := url.Parse(up.VerificationURL); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
			verificationURL = u.String()
		}
		return "verification_required", "PikPak 账号需要安全验证。请完成验证后重新验证账号，期间已暂停相关任务。", verificationURL
	case "quota_exceeded", "storage_quota_exceeded", "space_not_enough", "not_enough_space", "insufficient_space", "insufficient_storage", "storage_not_enough", "storage_limit_exceeded", "not_enough_storage", "storage_full":
		return "quota_exceeded", "PikPak 账号空间不足，已暂停新增保存和恢复。请清理空间或切换账号，再重新验证并继续任务。", ""
	case "credentials_required", "invalid_credentials", "invalid_grant", "invalid_password", "password_error", "invalid_username_or_password", "invalid_account_or_password", "user_not_found", "unauthenticated", "unauthorized", "invalid_token", "token_expired", "access_token_expired", "refresh_token_expired", "identity_changed":
		return "auth_required", "PikPak 登录已失效或认证信息不正确。请更新账号凭据并重新验证，之后可继续原任务。", ""
	}
	if up.Status == 401 && !strings.Contains(up.Endpoint, "upload/storage") {
		return "auth_required", "PikPak 登录已失效。请更新账号凭据并重新验证，之后可继续原任务。", ""
	}
	if (up.Status == 400 || up.Status == 403) && (up.Endpoint == "/v1/auth/signin" || up.Endpoint == "/v1/auth/token") {
		return "auth_required", "PikPak 登录失败，请检查账号认证信息并重新验证。原任务和资源记录已保留。", ""
	}
	// PikPak also returns this stable code for quota failures.
	if code == "resource_exhausted" && strings.Contains(strings.ToLower(up.Message), "space") {
		return "quota_exceeded", "PikPak 账号空间不足，已暂停新增保存和恢复。请清理空间或切换账号，再重新验证并继续任务。", ""
	}
	return "", "", ""
}

func accountErrorMessage(ac Account) string {
	if ac.Error != "" {
		return ac.Error
	}
	switch ac.Status {
	case "blocked":
		return "PikPak 账号已被冻结或停用，请检查账号状态或切换账号"
	case "auth_required":
		return "PikPak 登录已失效，请更新账号凭据并重新验证"
	case "quota_exceeded":
		return "PikPak 账号空间不足，请清理空间或切换账号，再重新验证"
	case "verification_required":
		return "PikPak 账号需要完成验证，请前往账号管理"
	case "error":
		return "暂时无法确认 PikPak 账号状态，请检查网络并重新验证"
	default:
		return "请先连接并验证当前 PikPak 账号"
	}
}

func accountStatusAllows(status, kind string) bool {
	if kind == "verify" || status == "ready" {
		return true
	}
	return status == "quota_exceeded" && (kind == "scan" || kind == "adopt" || kind == "trash" || kind == "sync" || kind == "cleanup")
}

func (a *App) accountJobAllowed(id, kind string) bool {
	ac, err := a.Store.Account(id)
	return err == nil && accountStatusAllows(ac.Status, kind)
}

func (a *App) accountFailure(id string) error {
	ac, err := a.Store.Account(id)
	if err != nil {
		return fail(409, "请先连接并验证当前 PikPak 账号")
	}
	return fail(409, accountErrorMessage(ac))
}

func (a *App) observeAccountError(id string, err error) bool {
	return a.observeAccountErrorIfCurrent(id, "", err)
}

func (a *App) observeAccountErrorIfCurrent(id, expectedSecret string, err error) bool {
	status, message, verificationURL := classifyAccountError(err)
	if status == "" {
		return false
	}
	return a.setAccountFault(id, expectedSecret, status, message, verificationURL)
}

// No job mutex is taken here: provider callbacks may run inside an operation.
// The transaction protects new credentials and task/session state together.
func (a *App) setAccountFault(id, expectedSecret, status, message, verificationURL string) bool {
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	ac, err := accountScan(tx.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id=?`, id))
	if err != nil || expectedSecret != "" && ac.Secret != expectedSecret {
		return false
	}
	// A quota refresh or transient verification error cannot erase a known
	// authentication/freeze fault. Only successful explicit verification clears it.
	if (status == "quota_exceeded" || status == "error") && ac.Status != "ready" && ac.Status != "unverified" && ac.Status != "error" && ac.Status != "quota_exceeded" {
		status, message, verificationURL = ac.Status, accountErrorMessage(ac), ac.VerificationURL
	}
	changed := ac.Status != status || ac.Error != message || ac.VerificationURL != verificationURL
	if _, err = tx.Exec(`UPDATE accounts SET status=?,error=?,verification_url=? WHERE id=?`, status, message, verificationURL, id); err != nil {
		return false
	}
	filter := ""
	if status == "quota_exceeded" {
		filter = " AND kind NOT IN ('scan','adopt','trash','sync','cleanup')"
	}
	result, err := tx.Exec(`UPDATE jobs SET state='paused',message=?,updated=? WHERE account_id=? AND kind<>'verify' AND state IN ('queued','waiting','retry')`+filter,
		message+" 原任务和已提交结果已保留，处理后可手动继续。", now(), id)
	if err != nil || tx.Commit() != nil {
		return false
	}
	paused, _ := result.RowsAffected()
	if changed || paused > 0 {
		a.Store.Event("account", id)
	}
	return true
}

func (a *App) observeVerificationFailure(id, expectedSecret string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, errPaused) {
		return
	}
	if a.observeAccountErrorIfCurrent(id, expectedSecret, err) {
		return
	}
	a.setAccountFault(id, expectedSecret, "error", "暂时无法确认 PikPak 账号状态。请检查网络和账号连接后重新验证；原任务和资源记录已保留。", "")
}

func (a *App) recordAccountQuota(id string, q pikpak.Quota) {
	a.recordAccountQuotaIfCurrent(id, "", q)
}

func (a *App) recordAccountQuotaIfCurrent(id, expectedSecret string, q pikpak.Quota) {
	result, err := a.Store.DB.Exec(`UPDATE accounts SET quota_limit=?,quota_used=? WHERE id=? AND (?='' OR secret=?)`, int64(q.Limit), int64(q.Usage), id, expectedSecret, expectedSecret)
	if err != nil {
		return
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return
	}
	if !q.Unlimited && q.Limit > 0 && q.Usage >= q.Limit {
		a.observeAccountErrorIfCurrent(id, expectedSecret, &pikpak.APIError{Code: "quota_exceeded"})
	}
}

// The cached client is evicted atomically with credential replacement. A stale
// response must never make newer credentials healthy or unhealthy.
func (a *App) currentClientAccount(id string, c pikpak.Provider) (Account, error) {
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	if a.Factory == nil && a.clients[id] != c {
		return Account{}, errPaused
	}
	return a.Store.Account(id)
}

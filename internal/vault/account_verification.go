package vault

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"pikpakvault/internal/pikpak"
)

const verificationStatePrefix = "account_verification:"

type accountVerificationState struct {
	AccountID  string `json:"account_id"`
	DeviceID   string `json:"device_id"`
	SecretHash string `json:"secret_hash"`
	Action     string `json:"action"`
	SessionID  string `json:"session_id"`
	Expires    int64  `json:"expires"`
}

func verificationCallback(r *http.Request, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || !strings.EqualFold(u.Host, r.Host) || u.Path != "/account-verification" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fail(400, "验证返回地址必须是本站的验证页面")
	}
	origin := r.Header.Get("Origin")
	if origin != "" {
		o, err := url.Parse(origin)
		if err != nil || o.User != nil || o.Scheme != u.Scheme || !strings.EqualFold(o.Host, u.Host) || o.Path != "" || o.RawQuery != "" || o.Fragment != "" {
			return "", fail(400, "验证返回地址与当前页面不一致")
		}
	} else if (r.TLS != nil) != (u.Scheme == "https") {
		return "", fail(400, "验证返回地址与当前连接不一致")
	}
	return u.String(), nil
}

func officialVerificationURL(raw, callback, state string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return "", fail(502, "PikPak 返回了无法使用的验证地址，请重新获取")
	}
	host := strings.ToLower(u.Hostname())
	if host != "mypikpak.com" && host != "user.mypikpak.com" && host != "mypikpak.net" && host != "user.mypikpak.net" {
		return "", fail(502, "PikPak 返回了非官方验证地址")
	}
	q := u.Query()
	q.Set("redirect_uri", callback)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (a *App) accountVerificationPrepare(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := decode(r, &v); err != nil {
		return err
	}
	callback, err := verificationCallback(r, v.CallbackURL)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	provider, err := a.client(id)
	if err != nil {
		return err
	}
	c, ok := provider.(*pikpak.Client)
	if !ok {
		return fail(409, "当前账号无法发起网页验证")
	}
	challengeURL, snapshot, err := c.PrepareVerification(context.WithValue(r.Context(), accountProbeKey{}, id))
	if err != nil {
		var up *pikpak.APIError
		if errors.As(err, &up) && (up.Code == "verification_cooldown" || up.Code == "verification_not_required") {
			return fail(up.Status, up.Message)
		}
		return err
	}
	ac, err := a.currentClientAccount(id, c)
	if err != nil {
		return err
	}
	var current pikpak.Credentials
	if err := a.Store.Unseal(ac.Secret, &current); err != nil {
		return err
	}
	if current != snapshot {
		return fail(409, "账号凭据已经变化，请重新获取验证链接")
	}
	state := ID() + ID()
	link, err := officialVerificationURL(challengeURL, callback, state)
	if err != nil {
		return err
	}
	expires := now() + 600
	if snapshot.CaptchaExpiry > now() && snapshot.CaptchaExpiry < expires {
		expires = snapshot.CaptchaExpiry
	}
	s := r.Context().Value(sessionKey{}).(session)
	payload, err := a.Store.Seal(accountVerificationState{AccountID: id, DeviceID: snapshot.DeviceID, SecretHash: tokenHash(ac.Secret), Action: snapshot.CaptchaAction, SessionID: s.ID, Expires: expires})
	if err != nil {
		return err
	}
	if err := a.cleanVerificationStates(id); err != nil {
		return err
	}
	if err := a.Store.Set(verificationStatePrefix+tokenHash(state), payload); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"url": link, "expires_at": expires})
	return nil
}

func (a *App) cleanVerificationStates(accountID string) error {
	rows, err := a.Store.DB.Query(`SELECT key,value FROM settings WHERE key LIKE 'account_verification:%'`)
	if err != nil {
		return err
	}
	var remove []string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return err
		}
		var state accountVerificationState
		if a.Store.Unseal(value, &state) != nil || state.Expires <= now() || state.AccountID == accountID {
			remove = append(remove, key)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, key := range remove {
		if _, err := a.Store.DB.Exec(`DELETE FROM settings WHERE key=?`, key); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) accountVerificationComplete(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		State        string `json:"state"`
		CaptchaToken string `json:"captcha_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := decode(r, &v); err != nil {
		return err
	}
	if len(v.State) < 32 || len(v.State) > 256 || len(v.CaptchaToken) == 0 || len(v.CaptchaToken) > 16384 || v.ExpiresIn <= 0 || strings.ContainsAny(v.CaptchaToken, "\r\n") {
		return fail(400, "验证信息不完整或已过期，请重新获取验证链接")
	}
	key := verificationStatePrefix + tokenHash(v.State)
	var sealed string
	if err := a.Store.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&sealed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fail(409, "验证链接已失效或已经使用，请重新获取")
		}
		return err
	}
	var state accountVerificationState
	if err := a.Store.Unseal(sealed, &state); err != nil {
		return err
	}
	s := r.Context().Value(sessionKey{}).(session)
	if state.SessionID != s.ID {
		return fail(403, "请在发起验证的登录会话中完成验证")
	}
	// Claim once before touching the upstream. Concurrent/replayed callbacks must
	// never exchange or overwrite credentials more than once.
	result, err := a.Store.DB.Exec(`DELETE FROM settings WHERE key=? AND value=?`, key, sealed)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fail(409, "该验证链接已经使用，请重新获取")
	}
	if state.Expires <= now() {
		return fail(409, "验证链接已过期，请重新获取")
	}
	ac, err := a.Store.Account(state.AccountID)
	if err != nil {
		return err
	}
	if tokenHash(ac.Secret) != state.SecretHash {
		return fail(409, "账号凭据已经变化，请重新获取验证链接")
	}
	var credentials pikpak.Credentials
	if err := a.Store.Unseal(ac.Secret, &credentials); err != nil {
		return err
	}
	if credentials.DeviceID != state.DeviceID {
		return fail(409, "验证设备已经变化，请重新获取验证链接")
	}
	credentials.CaptchaToken = v.CaptchaToken
	credentials.CaptchaExpiry = now() + min(v.ExpiresIn, 600)
	credentials.CaptchaAction = state.Action
	credentials.CaptchaVerified = true
	credentials.UserID = ac.Identity
	c := pikpak.New(credentials)
	me, err := c.Me(r.Context())
	if err != nil {
		return err
	}
	quota, err := c.Quota(r.Context())
	if err != nil {
		return err
	}
	secret, err := a.Store.Seal(c.Credentials)
	if err != nil {
		return err
	}
	status, message := "ready", ""
	if !quota.Unlimited && quota.Limit > 0 && quota.Usage >= quota.Limit {
		status, message = "quota_exceeded", accountErrorMessage(Account{Status: "quota_exceeded"})
	}
	a.gate.Lock()
	defer a.gate.Unlock()
	a.clientsMu.Lock()
	defer a.clientsMu.Unlock()
	prepareRoot := status == "ready" && ac.RootID == "" && a.active() == ac.ID
	if prepareRoot {
		status = "unverified"
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err = tx.Exec(`UPDATE accounts SET secret=?,identity=?,status=?,error=?,verification_url='',quota_limit=?,quota_used=? WHERE id=? AND secret=?`, secret, me.Sub, status, message, int64(quota.Limit), int64(quota.Usage), ac.ID, ac.Secret)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fail(409, "账号凭据已经变化，请重新获取验证链接")
	}
	if prepareRoot {
		// A running verifier still owns the replaced client and will pause. Only
		// pending verifiers can pick up these newly committed credentials.
		_, err = tx.Exec(`INSERT INTO jobs(id,account_id,kind,title,state,data,created,updated) SELECT ?,?,'verify',?,'queued','{}',?,? WHERE NOT EXISTS(SELECT 1 FROM jobs WHERE account_id=? AND kind='verify' AND state IN ('queued','waiting','retry'))`, ID(), ac.ID, "检查账号 · "+ac.Name, now(), now(), ac.ID)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	delete(a.clients, ac.ID)
	a.Store.Event("account", ac.ID)
	if prepareRoot {
		a.notify()
	}
	writeJSON(w, 200, map[string]string{"account_id": ac.ID})
	return nil
}

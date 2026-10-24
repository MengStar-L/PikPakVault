// Run before the application and its assets: verification tokens must not stay
// in the address bar, browser history, storage, or subsequent request referrers.
(() => {
  if (window.location.pathname !== '/account-verification') return;
  const query = new URLSearchParams(window.location.search);
  const state = query.get('state');
  const token = query.get('captcha_token');
  const expiry = query.get('expires_in');
  const valid = query.getAll('state').length === 1 && query.getAll('captcha_token').length === 1 &&
    query.getAll('expires_in').length <= 1 && state && state.length <= 256 && token && token.length <= 16384 &&
    (expiry === null || (/^\d{1,6}$/.test(expiry) && Number(expiry) > 0 && Number(expiry) <= 86400));
  window.__vaultVerificationReturn = valid
    ? { state, captcha_token: token, expires_in: expiry === null ? 600 : Number(expiry) }
    : { error: '验证返回信息缺失或不完整，请回到账号管理重新开始安全验证。' };
  window.history.replaceState(window.history.state, '', '/account-verification');
})();

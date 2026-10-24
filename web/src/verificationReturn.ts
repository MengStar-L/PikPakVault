import { api, setCsrf } from './api'

type VerificationPayload = { state: string; captcha_token: string; expires_in: number }
type VerificationReturn = VerificationPayload | { error: string }
type Completion = { account_id: string }

declare global {
  interface Window { __vaultVerificationReturn?: VerificationReturn }
}

// This is intentionally memory only. A reload must start a new verification,
// and StrictMode/remounts must not submit a one-time return token twice.
let returned = window.__vaultVerificationReturn
delete window.__vaultVerificationReturn
let completion: Promise<Completion> | undefined

export function discardAccountVerificationReturn() {
  returned = undefined
}

export function completeAccountVerification(csrf: string): Promise<Completion> {
  if (completion) return completion
  if (!returned || 'error' in returned) {
    return Promise.reject(new Error(returned && 'error' in returned ? returned.error : '没有可用的验证返回信息，请回到账号管理重新开始安全验证。'))
  }
  const payload = returned
  setCsrf(csrf)
  completion = api<Completion>('/accounts/verification/complete', payload).then(result => {
    returned = undefined
    return result
  }).catch(error => {
    // State is bound to the session that started verification. A new login
    // must start a fresh challenge rather than replaying this response.
    returned = undefined
    throw error
  })
  return completion
}

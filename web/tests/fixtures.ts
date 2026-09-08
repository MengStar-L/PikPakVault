import { test as base } from '@playwright/test'

// Reuse one authenticated session per worker so the real sign-in rate limiter
// remains enabled while the browser suite grows beyond ten tests per minute.
export const test = base.extend<{}, { vaultState: Awaited<ReturnType<import('@playwright/test').APIRequestContext['storageState']>> }>({
  vaultState: [async ({ playwright }, use) => {
    const request = await playwright.request.newContext({ baseURL: 'http://127.0.0.1:8088' })
    const response = await request.post('/api/v1/auth/login', { data: { password: 'vault-e2e-password' } })
    if (!response.ok()) throw new Error(`Fixture authentication failed: ${response.status()}`)
    await use(await request.storageState()); await request.dispose()
  }, { scope: 'worker' }],
  storageState: async ({ vaultState }, use) => use(vaultState),
})
export { expect } from '@playwright/test'

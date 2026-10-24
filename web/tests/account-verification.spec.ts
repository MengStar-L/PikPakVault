import { test, expect } from './fixtures'
import type { Page } from '@playwright/test'

const account = {
  id: 'captcha-account', name: '当前需要安全验证的账号', identity: 'account-identity', root_id: 'remote-root',
  status: 'verification_required', error: 'PikPak 账号需要安全验证。',
  verification_url: 'https://user.mypikpak.com/captcha/v2/txCaptcha.html?captcha_token=old-unusable-token',
  quota_limit: 10 * 1024 ** 4, quota_used: 8.4 * 1024 ** 4, verification_pending: false,
}

async function accountPage(page: Page) {
  await page.addInitScript(() => {
    window.EventSource = class extends EventTarget { close() {} } as unknown as typeof EventSource
  })
  await page.route('**/api/v1/accounts', route => route.fulfill({ json: { accounts: [account], active_id: account.id } }))
}

test('manual verification gets a fresh link once, recovers from failure, and expires without retry loops', async ({ page }) => {
  await accountPage(page)
  await page.clock.install()
  let requests = 0
  let release: (() => void) | undefined
  await page.route('**/api/v1/accounts/captcha-account/verification', async route => {
    requests++
    expect(route.request().postDataJSON()).toEqual({ callback_url: 'http://127.0.0.1:8088/account-verification' })
    if (requests === 1) return route.fulfill({ status: 503, json: { error: '暂时无法获取验证链接，请稍后重试' } })
    await new Promise<void>(resolve => { release = resolve })
    return route.fulfill({ json: {
      url: 'https://user.mypikpak.com/captcha/v2/txCaptcha.html?state=fresh-state&redirect_uri=http%3A%2F%2F127.0.0.1%3A8088%2Faccount-verification',
      expires_at: Math.floor(await page.evaluate(() => Date.now()) / 1000) + 60,
    } })
  })
  await page.goto('/accounts')
  const panel = page.getByRole('region', { name: `安全验证 ${account.name}` })
  await expect(page.locator('a[href*="old-unusable-token"]')).toHaveCount(0)
  await panel.getByRole('button', { name: '开始安全验证' }).click()
  await expect(panel.getByRole('alert')).toHaveText('暂时无法获取验证链接，请稍后重试')
  await panel.getByRole('button', { name: '重新获取验证链接' }).click()
  await expect(panel.getByRole('button', { name: '正在准备验证…' })).toBeDisabled()
  await expect.poll(() => requests).toBe(2)
  release!()
  const link = panel.getByRole('link', { name: '打开 PikPak 验证' })
  await expect(link).toBeVisible()
  await expect(link).toHaveAttribute('rel', 'noopener noreferrer nofollow')
  await expect(panel).toContainText('完成后会自动返回核验账号')
  for (const width of [1440, 768, 360]) {
    await page.setViewportSize({ width, height: 900 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy()
    await page.screenshot({ path: `../artifacts/account-verification-${width}.png`, fullPage: true })
  }
  await page.clock.fastForward(61000)
  await expect(panel).toContainText('验证链接已过期')
  await expect(link).toHaveCount(0)
  await expect(panel.getByRole('button', { name: '更新凭据' })).toBeVisible()
  expect(requests).toBe(2)
})

test('verification refuses a non-PikPak destination', async ({ page }) => {
  await accountPage(page)
  await page.route('**/api/v1/accounts/captcha-account/verification', route => route.fulfill({ json: {
    url: 'https://user.mypikpak.com.evil.example/captcha', expires_at: Math.floor(Date.now() / 1000) + 300,
  } }))
  await page.goto('/accounts')
  const panel = page.getByRole('region', { name: `安全验证 ${account.name}` })
  await panel.getByRole('button', { name: '开始安全验证' }).click()
  await expect(panel.getByRole('alert')).toContainText('未取得有效的 PikPak 验证链接')
  await expect(panel.getByRole('link')).toHaveCount(0)
})

test('return removes sensitive URL fields before authentication and completes only once with CSRF', async ({ page }) => {
  await accountPage(page)
  const auth = await (await page.request.get('/api/v1/auth/status')).json()
  let submissions = 0
  await page.route('**/api/v1/auth/status', async route => {
    // The early bootstrap runs before App's first authenticated request.
    expect(new URL(page.url()).search).toBe('')
    await route.continue()
  })
  await page.route('**/api/v1/accounts/verification/complete', route => {
    submissions++
    expect(route.request().headers()['x-csrf-token']).toBe(auth.csrf)
    expect(route.request().headers()['referer'] || '').not.toContain('return-test-token')
    expect(route.request().postDataJSON()).toEqual({ state: 'return-test-state', captcha_token: 'return-test-token', expires_in: 300 })
    return route.fulfill({ json: { account_id: account.id } })
  })
  await page.goto('/account-verification?state=return-test-state&captcha_token=return-test-token&expires_in=300')
  await expect(page).toHaveURL('http://127.0.0.1:8088/account-verification')
  await expect(page.getByRole('heading', { name: '安全验证已完成' })).toBeVisible()
  expect(submissions).toBe(1)
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).not.toMatch(/return-test-token|return-test-state/)
  await expect(page.getByText('账号连接已核验。已暂停的任务仍保留，可在传输任务中继续或重试。')).toBeVisible()
  await page.reload()
  await expect(page.getByRole('heading', { name: '安全验证尚未完成' })).toBeVisible()
  expect(submissions).toBe(1)
})

test('failed or malformed verification return does not mark the account ready or submit again', async ({ page }) => {
  await accountPage(page)
  let submissions = 0
  await page.route('**/api/v1/accounts/verification/complete', route => {
    submissions++
    return route.fulfill({ status: 409, json: { error: '验证已过期或账号凭据已变化，请重新开始' } })
  })
  await page.goto('/account-verification?state=failed-state&captcha_token=failed-token&expires_in=300')
  await expect(page.getByRole('heading', { name: '安全验证尚未完成' })).toBeVisible()
  await expect(page.getByText('验证已过期或账号凭据已变化，请重新开始')).toBeVisible()
  expect(submissions).toBe(1)
  await page.setViewportSize({ width: 360, height: 900 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy()
  await page.goto('/account-verification?state=a&state=b&captcha_token=duplicate-token')
  await expect(page).toHaveURL('http://127.0.0.1:8088/account-verification')
  await expect(page.getByText('验证返回信息缺失或不完整，请回到账号管理重新开始安全验证。')).toBeVisible()
  expect(submissions).toBe(1)
})

test('a signed-out verification return requires a fresh challenge after login', async ({ page }) => {
  await accountPage(page)
  let loggedIn = false
  let submissions = 0
  await page.route('**/api/v1/auth/status', route => route.fulfill({ json: {
    configured: true, authenticated: loggedIn, ...(loggedIn ? { csrf: 'new-login-csrf' } : {}),
  } }))
  await page.route('**/api/v1/auth/login', route => {
    loggedIn = true
    return route.fulfill({ json: { csrf: 'new-login-csrf' } })
  })
  await page.route('**/api/v1/accounts/verification/complete', route => {
    submissions++
    expect(route.request().headers()['x-csrf-token']).toBe('new-login-csrf')
    expect(route.request().postDataJSON().captcha_token).toBe('login-return-token')
    return route.fulfill({ json: { account_id: account.id } })
  })
  await page.goto('/account-verification?state=login-return-state&captcha_token=login-return-token&expires_in=300')
  await expect(page).toHaveURL('http://127.0.0.1:8088/account-verification')
  await expect(page.getByText('资源库登录已失效。请先登录，再在账号管理重新开始安全验证。')).toBeVisible()
  expect(submissions).toBe(0)
  await page.getByPlaceholder('输入你的管理员密码').fill('example-password')
  await page.getByRole('button', { name: '进入资源库' }).click()
  await expect(page).toHaveURL('http://127.0.0.1:8088/accounts')
  await expect(page.getByRole('button', { name: '开始安全验证' })).toBeVisible()
  expect(submissions).toBe(0)
})

import { test, expect } from './fixtures'
import type { Page } from '@playwright/test'

type HealthAccount = {
  id: string; name: string; identity: string; root_id: string; status: string; error: string
  verification_url: string; quota_limit: number; quota_used: number; verification_pending: boolean
}
const healthy: HealthAccount = {
  id: 'account-health-test', name: '我的主账号', identity: 'test-account',
  root_id: 'test-root', status: 'ready', error: '', verification_url: '',
  quota_limit: 10 * 1024 ** 4, quota_used: 708 * 1024 ** 3, verification_pending: false,
}

async function accountState(page: Page, account: Partial<HealthAccount> = {}) {
  let active = { ...healthy, ...account }
  let checks = 0
  await page.addInitScript(() => {
    const streams: EventTarget[] = []
    ;(window as unknown as { vaultHealthStreams: EventTarget[] }).vaultHealthStreams = streams
    class TestEvents extends EventTarget {
      constructor(_url: string) { super(); streams.push(this); setTimeout(() => this.dispatchEvent(new Event('open')), 0) }
      close() { streams.splice(streams.indexOf(this), 1) }
    }
    window.EventSource = TestEvents as unknown as typeof EventSource
  })
  await page.route('**/api/v1/accounts', route => route.fulfill({ json: { accounts: [active], active_id: active.id } }))
  await page.route('**/api/v1/accounts/*/verify', route => {
    checks++
    active = { ...active, verification_pending: true }
    return route.fulfill({ status: 202, json: { id: 'health-verification-job', kind: 'verify', state: 'queued' } })
  })
  return {
    update(value: Partial<HealthAccount>) { active = { ...active, ...value } },
    checks: () => checks,
    async signal(event = 'change') {
      await page.evaluate(eventName => {
        const streams = (window as unknown as { vaultHealthStreams: EventTarget[] }).vaultHealthStreams
        streams.forEach(stream => stream.dispatchEvent(new Event(eventName)))
      }, event)
    },
  }
}

async function noOverflow(page: Page) {
  const bad = await page.evaluate(() => [...document.querySelectorAll(
    '.account-health-alert,.account-health-alert *,.account-card,.account-card *,.file-account,.storage-card',
  )].filter(el => {
    const bounds = el.getBoundingClientRect()
    return bounds.width > 0 && bounds.height > 0 && getComputedStyle(el).visibility !== 'hidden' &&
      (bounds.right > innerWidth + 2 || bounds.left < -2)
  }).map(el => `${el.tagName}.${el.className}`))
  expect(bad).toEqual([])
}

test('blocked account stays visible while browsing and verification recovers through SSE reconnect', async ({ page }) => {
  const state = await accountState(page, { status: 'blocked', error: 'PikPak 已限制此账号，请先在官方客户端处理。' })
  await page.goto('/files')
  const alert = page.getByRole('alert', { name: '当前账号异常' })
  await expect(alert).toContainText('账号已被限制或冻结')
  await expect(page.locator('.file-card').first()).toBeVisible()
  await expect(page.locator('.account-switch')).toHaveClass(/has-issue/)
  await expect(page.locator('.storage-health')).toContainText('容量待核验')
  await page.locator('.heading-actions').getByRole('button', { name: '添加资源', exact: true }).click()
  await expect(page).toHaveURL(/\/accounts$/)
  await expect(page.getByRole('dialog')).toHaveCount(0)
  const card = page.locator('.account-card').first()
  await expect(card.locator('.status')).toHaveText('已限制 / 冻结')
  await expect(card).toContainText('上次读取的云端容量')
  await expect(card).toHaveCSS('opacity', '1')
  await page.screenshot({ path: '../artifacts/account-health-blocked.png', fullPage: true })
  await alert.getByRole('button', { name: '重新检查 我的主账号', exact: true }).click()
  await expect(alert.getByRole('button', { name: '正在检查 我的主账号', exact: true })).toBeDisabled()
  await expect(card.getByRole('button', { name: '正在检查 我的主账号', exact: true })).toBeDisabled()
  expect(state.checks()).toBe(1)
  state.update({ status: 'ready', error: '', verification_pending: false })
  await state.signal('open')
  await expect(alert).toHaveCount(0)
  await expect(card.locator('.status')).toHaveText('可用')
  await expect(card.getByRole('button', { name: '重新检查 我的主账号', exact: true })).toBeEnabled()
  await expect(page.locator('.account-switch')).not.toHaveClass(/has-issue/)
  await expect(page.locator('.storage-health')).toHaveCount(0)
})

test('quota and authentication failures remain truthful at desktop tablet and mobile sizes', async ({ page }) => {
  const state = await accountState(page, {
    status: 'quota_exceeded',
    name: '长期收藏使用的主账号与很长的名称-CloudCollection-2026',
    error: '云端剩余空间不足以完成本次保存，请释放空间或切换账号。现有目录和来源记录继续保留。',
  })
  await page.goto('/accounts')
  const alert = page.getByRole('alert', { name: '当前账号异常' })
  const card = page.locator('.account-card').first()
  await expect(alert).toContainText('云端空间不足')
  await expect(card.locator('.status')).toHaveText('空间不足')
  for (const width of [1440, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 960 })
    await expect(alert).toBeVisible()
    await expect(card).toHaveCSS('opacity', '1')
    await noOverflow(page)
    await page.screenshot({ path: `../artifacts/account-health-${width}.png`, fullPage: true })
  }
  state.update({ status: 'auth_required', error: 'PikPak 登录已失效，请更新密码或令牌。' })
  await state.signal()
  await expect(alert).toContainText('账号登录失效')
  await expect(card.locator('.status')).toHaveText('登录失效')
  await card.getByRole('button', { name: '更新凭据' }).click()
  await expect(page.getByRole('dialog', { name: '更新账号凭据' })).toBeVisible()
  await page.getByRole('button', { name: '取消', exact: true }).click()
  state.update({ status: 'verification_required', error: '请在 PikPak 完成安全验证。', verification_url: 'https://mypikpak.com/verification' })
  await state.signal()
  await expect(alert).toContainText('账号需要安全验证')
  await expect(card.getByRole('link', { name: '前往 PikPak 完成验证' })).toBeVisible()
  state.update({ status: 'error', error: '网络连接超时，账号状态尚未确认。', verification_url: '' })
  await state.signal()
  await expect(alert).toContainText('账号连接异常')
  await expect(card.locator('.status')).toHaveText('连接异常')
  await expect(card.getByRole('link', { name: '前往 PikPak 完成验证' })).toHaveCount(0)
  await noOverflow(page)
  await page.goto('/files')
  await expect(page.locator('.file-card').first()).toBeVisible()
  await expect(alert).toContainText('账号连接异常')
  await noOverflow(page)
  await page.screenshot({ path: '../artifacts/account-health-library-mobile.png', fullPage: true })
})

test('failed verification request leaves warning and allows retry without a stale loading state', async ({ page }) => {
  await accountState(page, { status: 'auth_required', error: '登录已失效，请更新凭据。' })
  await page.route('**/api/v1/accounts/*/verify', route => route.fulfill({
    status: 503, json: { error: '暂时无法检查，请稍后重试' },
  }))
  await page.goto('/files')
  const alert = page.getByRole('alert', { name: '当前账号异常' })
  await alert.getByRole('button', { name: '重新检查 我的主账号' }).click()
  await expect(page.getByText('暂时无法检查，请稍后重试', { exact: true })).toBeVisible()
  await expect(alert).toContainText('账号登录失效')
  await expect(alert.getByRole('button', { name: '重新检查 我的主账号' })).toBeEnabled()
  await expect(page.locator('.file-card').first()).toBeVisible()
})

test('an inactive account warning does not mark the healthy active account unavailable', async ({ page }) => {
  await accountState(page)
  await page.route('**/api/v1/accounts', route => route.fulfill({
    json: { active_id: healthy.id, accounts: [healthy, { ...healthy, id: 'old-account', name: '旧账号', status: 'blocked', error: '账号已冻结。' }] },
  }))
  await page.goto('/accounts')
  await expect(page.locator('.account-card')).toHaveCount(2)
  await expect(page.getByRole('alert', { name: '当前账号异常' })).toHaveCount(0)
  await expect(page.locator('.account-card').first().locator('.status')).toHaveText('可用')
  const old = page.locator('.account-card').nth(1)
  await expect(old.locator('.status')).toHaveText('已限制 / 冻结')
  await expect(old.getByRole('button', { name: '切换到此账号' })).toBeDisabled()
})

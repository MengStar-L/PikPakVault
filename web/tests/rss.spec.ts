import { test, expect } from './fixtures'
import type { RSSSubscription } from '../src/RSS'

function subscription(overrides: Partial<RSSSubscription> = {}): RSSSubscription {
  return { id: 'rss-one', name: '每周电影更新', url: 'https://feed.example.test/weekly.xml?token=private-feed-token', parent_id: 'root', target_path: '我的文件', interval_minutes: 30, enabled: true, initialized: true, import_existing: false, last_checked: 1789860000, next_check: 1789861800, last_error: '', last_job: null, counts: { total: 12, saved: 8, pending: 2, failed: 1, skipped: 1 }, ...overrides }
}

test('RSS creation selects a nested folder immediately and editing, pause, check and removal preserve resources', async ({ page }) => {
  let subscriptions: RSSSubscription[] = []
  let checks = 0
  let removed = 0
  let writes = 0
  await page.route('**/api/v1/rss', route => {
    if (route.request().method() === 'GET') return route.fulfill({ json: { subscriptions, active_account: 'a' } })
    const body = route.request().postDataJSON()
    expect(body).toMatchObject({ name: '每周电影更新', url: 'https://feed.example.test/weekly.xml', parent_id: 'rss-nested', interval_minutes: 30, enabled: true, import_existing: false })
    writes++
    subscriptions = [subscription({ ...body, initialized: false, target_path: '我的文件 / 订阅收藏 / 电影时光' })]
    return route.fulfill({ status: 201, json: subscriptions[0] })
  })
  await page.route('**/api/v1/files?**', route => {
    const query = new URL(route.request().url()).searchParams
    if (query.get('transfers') !== '0') return route.continue()
    const parent = query.get('parent')
    const files = parent === 'root' ? [{ id: 'rss-target', name: '订阅收藏', kind: 'folder' }] : parent === 'rss-target' ? [{ id: 'rss-nested', name: '电影时光', kind: 'folder' }] : []
    const breadcrumbs = parent === 'root' ? [] : [{ id: 'rss-target', name: '订阅收藏' }, ...(parent === 'rss-nested' ? [{ id: 'rss-nested', name: '电影时光' }] : [])]
    return route.fulfill({ json: { files, breadcrumbs, total: files.length, page: 0, limit: 100 } })
  })
  await page.route('**/api/v1/rss/rss-one', route => {
    if (route.request().method() === 'DELETE') { removed++; subscriptions = []; return route.fulfill({ json: { ok: true } }) }
    subscriptions[0] = { ...subscriptions[0], ...route.request().postDataJSON() }
    return route.fulfill({ json: subscriptions[0] })
  })
  await page.route('**/api/v1/rss/rss-one/check', route => {
    checks++; subscriptions[0] = { ...subscriptions[0], initialized: true, last_job: { id: 'rss-check', kind: 'rss_check', state: 'completed', message: '发现 2 个新条目，已加入保存任务', title: '检查订阅', progress: 100, attempts: 0, next_run: 0, created: 1789860000, account_id: 'a' } }
    return route.fulfill({ status: 202, json: subscriptions[0].last_job })
  })
  await page.goto('/rss')
  await expect(page.getByRole('heading', { name: 'RSS 订阅', exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: 'RSS 订阅', exact: true })).toHaveAttribute('aria-current', 'page')
  await page.getByRole('button', { name: '添加订阅', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByRole('button', { name: '保存订阅' })).toBeDisabled()
  await dialog.getByLabel('订阅名称', { exact: true }).fill('每周电影更新')
  await dialog.getByLabel('RSS / Atom 地址').fill('https://feed.example.test/weekly.xml')
  await expect(dialog.getByLabel('自动检查并保存新资源')).toBeChecked()
  await expect(dialog.getByLabel('同时保存订阅中现有的资源')).not.toBeChecked()
  await dialog.locator('.destination-row button').click()
  await dialog.locator('.picker-list').getByRole('button', { name: '订阅收藏', exact: true }).click()
  await dialog.locator('.picker-list').getByRole('button', { name: '电影时光', exact: true }).click()
  await expect(dialog.locator('.destination-path')).toHaveText('我的文件 / 订阅收藏 / 电影时光')
  expect(writes).toBe(0)
  await expect(dialog.getByRole('button', { name: '选择当前文件夹' })).toHaveCount(0)
  await dialog.getByRole('button', { name: '保存订阅', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect(writes).toBe(1)
  await expect(page.locator('.rss-target')).toHaveAttribute('href', '/files?folder=rss-nested')
  await expect(page.locator('.rss-first-check')).toContainText('首次成功检查建立记录')
  await page.getByRole('button', { name: '暂停', exact: true }).click()
  await expect(page.locator('.rss-mode')).toHaveText('已暂停')
  await page.getByRole('button', { name: '立即检查', exact: true }).click()
  await expect(page.locator('.rss-job-state')).toContainText('发现 2 个新条目')
  expect(checks).toBe(1)
  await expect(page).toHaveURL(/\/rss$/)
  await page.getByRole('button', { name: '编辑 每周电影更新', exact: true }).click()
  await expect(dialog.getByLabel('RSS / Atom 地址')).toHaveAttribute('readonly', '')
  await expect(dialog.getByLabel('同时保存订阅中现有的资源')).toHaveCount(0)
  await expect(dialog).toContainText('更换位置只影响之后发现的资源')
  await dialog.getByLabel('检查频率', { exact: true }).selectOption('60')
  await dialog.getByLabel('自动检查并保存新资源').check()
  await dialog.getByRole('button', { name: '保存订阅', exact: true }).click()
  await expect(page.locator('.rss-mode')).toHaveText('每 1 小时')
  await page.getByRole('button', { name: '移除 每周电影更新', exact: true }).click()
  await expect(dialog).toContainText('已经保存的文件、来源与传输任务都会保留')
  expect(removed).toBe(0)
  await dialog.getByRole('button', { name: '移除订阅', exact: true }).click()
  await expect(page.locator('.rss-card')).toHaveCount(0)
  expect(removed).toBe(1)
})

test('RSS populated desktop, tablet, mobile and entry records support long content, pagination and original job retry', async ({ page }) => {
  const title = '一份名称很长的每周收藏_'.repeat(12)
  const current = subscription({ name: title, target_path: '我的文件 / 很长的收藏路径 / '.repeat(5) + '最后的文件夹', last_error: '订阅暂时不可达：服务器返回 HTTP 503，请稍后重新检查。'.repeat(3) })
  let retries = 0
  let secondPage = 0
  const failedJob = { id: 'rss-import', account_id: 'a', kind: 'import', title: 'RSS 保存原任务', state: 'attention', progress: 10, message: '分享来源失效，请检查后重试原任务', attempts: 0, next_run: 0, created: 1789860000 }
  await page.route('**/api/v1/rss', r => r.fulfill({ json: { subscriptions: [current, subscription({ id: 'rss-two', name: '第二份订阅', enabled: false })], active_account: 'a' } }))
  await page.route('**/api/v1/rss/rss-one/entries?**', route => {
    const next = Number(new URL(route.request().url()).searchParams.get('page'))
    if (next) secondPage++
    return route.fulfill({ json: { entries: next ? [{ id: 'last-entry', title: '最后一条记录', discovered: 1789860000, published: 0, state: 'skipped', message: '首次检查已记录，不补回历史条目', job_id: '' }] : [{ id: 'long-entry', title: title + '.mkv', discovered: 1789860000, published: 1789850000, state: 'failed', message: failedJob.message, job_id: failedJob.id }, { id: 'success-entry', title: '已经保存的电影.mp4', discovered: 1789860000, published: 0, state: 'saved', message: '已保存到指定目录', job_id: '' }], total: 51, page: next, limit: 50 } })
  })
  await page.route('**/api/v1/jobs/rss-import', route => route.fulfill({ json: { job: { ...failedJob, ...(retries ? { state: 'completed', progress: 100, message: '保存完成' } : {}) }, problems: {}, transfers: {} } }))
  await page.route('**/api/v1/jobs/rss-import/retry', route => { retries++; return route.fulfill({ json: { ...failedJob, state: 'queued', message: '正在重试原任务' } }) })
  await page.goto('/rss')
  await expect(page.locator('.rss-card')).toHaveCount(2)
  await expect(page.locator('.rss-card').first()).not.toContainText('private-feed-token')
  await page.setViewportSize({ width: 1440, height: 760 })
  await page.locator('.sidebar').getByRole('link', { name: '设置', exact: true }).scrollIntoViewIfNeeded()
  await expect(page.locator('.sidebar').getByRole('link', { name: '设置', exact: true })).toBeInViewport()
  await page.locator('.sidebar').evaluate(el => { el.scrollTop = 0 })
  for (const width of [1440, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 960 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    expect(await page.locator('.rss-card').first().evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true)
    await page.screenshot({ path: `../artifacts/rss-populated-${width}.png`, animations: 'disabled' })
  }
  await page.locator('.rss-card').first().getByRole('button', { name: '订阅记录', exact: true }).click()
  const records = page.getByRole('dialog', { name: '订阅记录', exact: true })
  await expect(records.locator('.rss-entry')).toHaveCount(2)
  for (const width of [1440, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 960 })
    expect(await records.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true)
  }
  await page.screenshot({ path: '../artifacts/rss-records-mobile.png', animations: 'disabled' })
  await records.getByRole('button', { name: '下一页', exact: true }).click()
  await expect(records).toContainText('最后一条记录')
  expect(secondPage).toBeGreaterThan(0)
  await records.getByRole('button', { name: '上一页', exact: true }).click()
  await records.getByRole('button', { name: '任务详情', exact: true }).click()
  const task = page.getByRole('dialog', { name: '任务详情', exact: true })
  await expect(task).toContainText('RSS 保存原任务')
  await task.locator('.modal-actions').getByRole('button', { name: '关闭', exact: true }).click()
  await records.getByRole('button', { name: '重试原任务', exact: true }).click()
  await expect(page.getByText('任务已完成', { exact: true })).toBeVisible()
  expect(retries).toBe(1)
})

test('RSS no-account, request failures and reduced motion remain actionable', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' })
  let active = ''
  await page.route('**/api/v1/rss', route => route.request().method() === 'GET' ? route.fulfill({ json: { subscriptions: [subscription()], active_account: active } }) : route.fulfill({ status: 400, json: { error: '订阅地址不是有效的 RSS / Atom 文档' } }))
  await page.route('**/api/v1/rss/rss-one/check', route => route.fulfill({ status: 503, json: { error: '订阅服务器暂时不可达，请稍后重试' } }))
  await page.route('**/api/v1/rss/rss-one/entries?**', route => route.fulfill({ status: 503, json: { error: '无法读取订阅记录' } }))
  await page.goto('/rss')
  await expect(page.getByRole('link', { name: '连接账号', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '立即检查' })).toBeDisabled()
  active = 'a'
  await page.reload()
  await page.getByRole('button', { name: '立即检查' }).click()
  await expect(page.getByText('订阅服务器暂时不可达，请稍后重试', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: '添加订阅', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('订阅名称', { exact: true }).fill('测试订阅')
  await dialog.getByLabel('RSS / Atom 地址').fill('https://example.test/not-a-feed')
  await dialog.getByLabel('同时保存订阅中现有的资源').check()
  await page.setViewportSize({ width: 360, height: 760 })
  await dialog.getByRole('button', { name: '保存订阅', exact: true }).click()
  await expect(dialog.getByRole('alert')).toContainText('订阅地址不是有效的 RSS / Atom 文档')
  await expect(dialog.getByRole('alert')).toBeInViewport()
  expect(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true)
  await expect(dialog.getByRole('button', { name: '保存订阅', exact: true })).toBeInViewport()
  await expect(page.getByText('订阅服务器暂时不可达，请稍后重试', { exact: true })).not.toBeVisible()
  await page.screenshot({ path: '../artifacts/rss-editor-error-mobile.png', animations: 'disabled' })
  await dialog.getByRole('button', { name: '取消', exact: true }).click()
  await page.getByRole('button', { name: '订阅记录', exact: true }).click()
  await expect(dialog.getByRole('alert')).toContainText('无法读取订阅记录')
  await expect(dialog.getByRole('button', { name: '重试', exact: true })).toBeVisible()
})

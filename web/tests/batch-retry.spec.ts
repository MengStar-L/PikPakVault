import { test, expect } from './fixtures'
import type { Job } from '../src/api'

function task(id: string, state: string, account = 'current'): Job {
  return { id, account_id: account, kind: 'teldrive_upload', lane: 'teldrive', title: `上传 · ${id}.mp4`, state, progress: 43, message: '下载连接中断，原进度已保留', attempts: 6, next_run: 0, created: 1792425600 }
}

test('batch retry uses the full current-account count, blocks repeat clicks and refreshes original tasks', async ({ page }) => {
  const jobs = [task('paused-file', 'paused'), task('failed-file', 'failed'), task('other-account', 'failed', 'other'), task('saved-file', 'completed')]
  let count = 427
  let requests = 0
  let release!: () => void
  const hold = new Promise<void>(resolve => { release = resolve })
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs, retry_summary: { account_id: 'current', count } } }))
  await page.route('**/api/v1/jobs/retry-all', async route => {
    requests++
    expect(route.request().postDataJSON()).toEqual({ account_id: 'current' })
    await hold
    jobs[0].state = jobs[1].state = 'queued'
    count = 0
    await route.fulfill({ json: { account_id: 'current', restarted: 427, skipped: 0, skipped_reasons: {}, message: '已将 427 项原任务重新排队，保留已有下载和上传进度。' } })
  })
  await page.goto('/tasks')
  await page.getByRole('button', { name: '需要处理', exact: true }).click()
  await expect(page.locator('.task-card.paused')).toContainText('paused-file.mp4')
  const all = page.getByRole('button', { name: '一键全部重新开始', exact: true })
  await expect(all).toContainText('427')
  await all.click()
  await expect(all).toBeDisabled()
  await expect(all).toContainText('正在重新排队')
  await expect(page.getByRole('button', { name: '重试 上传 · failed-file.mp4', exact: true })).toBeDisabled()
  await all.evaluate(button => (button as HTMLButtonElement).click())
  expect(requests).toBe(1)
  release()
  await expect(page.locator('.task-batch-result')).toContainText('已将 427 项原任务重新排队')
  await expect(page.locator('.task-card')).toHaveCount(1)
  await expect(page.locator('.task-card')).toContainText('other-account.mp4')
  await expect(all).toBeDisabled()
  await page.getByRole('button', { name: /^进行中/ }).click()
  await expect(page.locator('.task-card')).toHaveCount(2)
  expect(requests).toBe(1)
})

test('batch retry failure preserves rows and gives an actionable error before another attempt', async ({ page }) => {
  let requests = 0
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs: [task('original', 'failed')], retry_summary: { account_id: 'current', count: 1 } } }))
  await page.route('**/api/v1/jobs/retry-all', route => {
    requests++
    return route.fulfill({ status: 409, json: { error: '账号已切换，请刷新后再重试' } })
  })
  await page.goto('/tasks')
  const all = page.getByRole('button', { name: '一键全部重新开始', exact: true })
  await all.click()
  await expect(page.locator('.task-batch-result')).toHaveText('账号已切换，请刷新后再重试')
  await expect(all).toBeEnabled()
  await expect(page.locator('.task-card.failed')).toContainText('original.mp4')
  await expect(page.locator('.task-card.failed')).toContainText('原进度已保留')
  await all.click()
  await expect.poll(() => requests).toBe(2)
})

test('batch retry explains skipped tasks, polls queued results without SSE, and fits small screens', async ({ page }) => {
  let jobs = [task('retryable', 'failed'), task('blocked', 'paused')]
  let count = 2
  let submitted = false
  let readsAfterSubmit = 0
  await page.route('**/api/v1/events**', route => route.abort())
  await page.route('**/api/v1/jobs', route => {
    if (submitted) {
      readsAfterSubmit++
      if (readsAfterSubmit > 1) jobs[0] = { ...jobs[0], state: 'completed', progress: 100, message: '已核对原任务保存结果' }
    }
    return route.fulfill({ json: { jobs, retry_summary: { account_id: 'current', count } } })
  })
  await page.route('**/api/v1/jobs/retry-all', route => {
    submitted = true
    jobs = [{ ...jobs[0], state: 'queued' }, jobs[1]]
    count = 1
    return route.fulfill({ json: { account_id: 'current', restarted: 1, skipped: 1, skipped_reasons: { executing: 1 }, message: '已将 1 项原任务重新排队' } })
  })
  await page.goto('/tasks')
  await page.getByRole('button', { name: '一键全部重新开始', exact: true }).click()
  await expect(page.locator('.task-batch-result')).toContainText('跳过 1 项：1 项原任务仍在保存进度')
  await expect(page.locator('.task-card.completed')).toContainText('retryable.mp4')
  for (const width of [1440, 768, 360]) {
    await page.setViewportSize({ width, height: 960 })
    await expect(page.getByRole('button', { name: '一键全部重新开始', exact: true })).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await page.screenshot({ path: `../artifacts/batch-retry-${width}.png`, animations: 'disabled' })
  }
})

test('duplicate check history is explicitly explained instead of silently appearing stuck', async ({ page }) => {
  const jobs = [task('old-scan', 'failed')]
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs, retry_summary: { account_id: 'current', count: 29 } } }))
  await page.route('**/api/v1/jobs/retry-all', route => route.fulfill({ json: {
    account_id: 'current', restarted: 1, skipped: 29, skipped_reasons: { duplicate_check: 29 }, message: '已将 1 个任务重新加入队列，保留原有进度',
  } }))
  await page.goto('/tasks')
  await page.getByRole('button', { name: '一键全部重新开始', exact: true }).click()
  await expect(page.locator('.task-batch-result')).toContainText('跳过 29 项：29 项相同检查已排队，保留其余历史记录')
  await expect(page.locator('.task-card.failed')).toBeVisible()
})

test('a completed batch request stays associated with its original account after switching', async ({ page }) => {
  let active = 'current'
  let release!: () => void
  const hold = new Promise<void>(resolve => { release = resolve })
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs: [task('account-task', 'failed', active)], retry_summary: { account_id: active, count: 1 } } }))
  await page.route('**/api/v1/jobs/retry-all', async route => {
    expect(route.request().postDataJSON()).toEqual({ account_id: 'current' })
    await hold
    await route.fulfill({ json: { account_id: 'current', restarted: 1, skipped: 0, skipped_reasons: {}, message: '原任务已重新排队' } })
  })
  await page.goto('/tasks')
  await page.getByRole('button', { name: '一键全部重新开始', exact: true }).click()
  active = 'other'
  release()
  await expect(page.getByText('原账号的批量操作已提交，重新排队 1 项任务；当前账号未受影响', { exact: true })).toBeVisible()
  await expect(page.locator('.task-batch-result')).toHaveCount(0)
})

test('single-task retry disables bulk retry until its first execution result', async ({ page }) => {
  let job = task('single', 'failed')
  let release!: () => void
  const hold = new Promise<void>(resolve => { release = resolve })
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs: [job], retry_summary: { account_id: 'current', count: 1 } } }))
  await page.route('**/api/v1/jobs/single', route => route.fulfill({ json: { job } }))
  await page.route('**/api/v1/jobs/single/retry', async route => {
    await hold
    job = { ...job, state: 'waiting', message: '正在核对原上传会话' }
    await route.fulfill({ json: job })
  })
  await page.goto('/tasks')
  await page.getByRole('button', { name: '重试 上传 · single.mp4', exact: true }).click()
  await expect(page.getByRole('button', { name: '一键全部重新开始', exact: true })).toBeDisabled()
  release()
  await expect(page.locator('.task-card.waiting')).toContainText('正在核对原上传会话')
  await expect(page.getByRole('button', { name: '一键全部重新开始', exact: true })).toBeEnabled()
})

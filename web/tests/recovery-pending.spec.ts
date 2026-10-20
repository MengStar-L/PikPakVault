import { test, expect } from './fixtures'
import type { Job } from '../src/api'

for (const initialState of ['paused', 'partial', 'failed']) {
  test(`recovery preview exposes the original ${initialState} task and retries without a duplicate`, async ({ page }) => {
    let job: Job = {
      id: 'original-recovery', account_id: 'a', kind: 'recover', lane: 'teldrive',
      title: '恢复 · 很长的文件名称和整理后的收藏目录'.repeat(3),
      state: initialState, progress: 43, message: '来源连接中断，已有下载进度已保留。',
      attempts: 1, next_run: 0, created: 1792425600,
    }
    let retries = 0
    let created = 0
    await page.route('**/api/v1/recovery/preview', route => route.fulfill({ json: {
      account: { id: 'a', name: '我的主账号' }, items: [], pending_jobs: [job],
      skipped_pending: 1, bytes: 0, transfer_bytes: 0, available: 102400, capacity_warning: false,
    } }))
    await page.route('**/api/v1/recovery', route => { created++; return route.fulfill({ status: 500, json: { error: '不应创建重复恢复任务' } }) })
    await page.route('**/api/v1/jobs/original-recovery', route => route.fulfill({ json: { job, problems: {}, transfers: {} } }))
    await page.route('**/api/v1/jobs/original-recovery/retry', route => {
      retries++
      job = { ...job, state: 'waiting', message: '正在核对原上传会话' }
      return route.fulfill({ json: job })
    })
    await page.goto('/recovery')
    await page.getByRole('button', { name: '恢复全部缺失', exact: true }).click()
    const preview = page.getByRole('dialog', { name: '确认恢复资源', exact: true })
    await expect(preview).toContainText('仍由原任务处理')
    await expect(preview).not.toContainText('所选资源目前无需恢复')
    await expect(preview).toContainText('来源连接中断，已有下载进度已保留。')
    await expect(preview.getByRole('button', { name: '确认开始恢复', exact: true })).toBeDisabled()
    for (const width of [1440, 390, 360]) {
      await page.setViewportSize({ width, height: 900 })
      expect(await preview.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      if (initialState === 'paused' && width !== 390) {
        await page.screenshot({ path: `../artifacts/recovery-pending-${width}.png`, animations: 'disabled' })
      }
    }
    await preview.getByRole('button', { name: `查看任务 ${job.title}`, exact: true }).click()
    const detail = page.getByRole('dialog', { name: '任务详情', exact: true })
    await expect(detail).toContainText(job.title)
    await expect(detail).toContainText(job.message)
    await detail.locator('.modal-actions').getByRole('button', { name: '关闭', exact: true }).click()
    await expect(preview).toBeVisible()
    await preview.getByRole('button', { name: `${initialState === 'paused' ? '继续' : '重试'} ${job.title}`, exact: true }).click()
    await expect(preview.locator('.recovery-pending-job .status')).toHaveText('云端处理中')
    await expect(preview).toContainText('正在核对原上传会话')
    expect(retries).toBe(1)
    expect(created).toBe(0)
  })
}

test('running recovery can be paused and completed resources refresh without a false pending task', async ({ page }) => {
  let state = 'running'
  let pauses = 0
  const job = { id: 'running-recovery', title: '云端恢复 · 1 项', kind: 'recover', lane: 'cloud', progress: 60, message: '正在核对云端结果' }
  await page.route('**/api/v1/recovery/preview', route => route.fulfill({ json: {
    account: { id: 'a', name: '我的主账号' }, items: [],
    pending_jobs: state === 'completed' ? [] : [{ ...job, state }],
    skipped_pending: state === 'completed' ? 0 : 1,
    bytes: 0, transfer_bytes: 0, available: 102400, capacity_warning: false,
  } }))
  await page.route('**/api/v1/jobs/running-recovery/pause', route => {
    pauses++; state = 'paused'
    return route.fulfill({ json: { ...job, state } })
  })
  await page.goto('/recovery')
  await page.getByRole('button', { name: '恢复全部缺失', exact: true }).click()
  const preview = page.getByRole('dialog', { name: '确认恢复资源', exact: true })
  await expect(preview.getByRole('button', { name: '暂停 云端恢复 · 1 项', exact: true })).toBeVisible()
  await preview.getByRole('button', { name: '暂停 云端恢复 · 1 项', exact: true }).click()
  await expect(preview.locator('.recovery-pending-job .status')).toHaveText('已暂停')
  expect(pauses).toBe(1)
  state = 'completed'
  await expect(preview.locator('.recovery-pending-job')).toHaveCount(0)
  await expect(preview).toContainText('所选资源目前无需恢复。')
})

test('recovery list shows an existing task state and opens its original recovery controls', async ({ page }) => {
  const job = { id: 'existing-partial', title: 'TelDrive 恢复 · 1 项', kind: 'recover', lane: 'teldrive', state: 'partial', progress: 43, message: '上传中断，请重试原任务' }
  await page.route('**/api/v1/files?**', route => {
    if (new URL(route.request().url()).searchParams.get('view') !== 'missing') return route.continue()
    return route.fulfill({ json: {
      files: [{ id: 'owned-missing', name: '待恢复的影片.mp4', kind: 'file', mime: 'video/mp4', size: 1234, state: 'missing', source_id: 'source', transfer: { job_id: job.id, state: job.state, progress: job.progress, message: job.message } }],
      breadcrumbs: [], page: 0, limit: 100, total: 1,
    } })
  })
  await page.route('**/api/v1/recovery/preview', route => {
    expect(route.request().postDataJSON()).toEqual({ ids: ['owned-missing'] })
    return route.fulfill({ json: {
      account: { id: 'a', name: '我的主账号' }, items: [], pending_jobs: [job],
      skipped_pending: 1, bytes: 0, transfer_bytes: 0, available: 102400, capacity_warning: false,
    } })
  })
  await page.goto('/recovery')
  const row = page.locator('.recovery-item').filter({ hasText: '待恢复的影片.mp4' })
  await expect(row.locator('.status')).toHaveText('部分完成')
  await expect(row).toContainText('已有原任务')
  await expect(row.getByRole('button', { name: '核对恢复' })).toHaveCount(0)
  await row.getByRole('button', { name: '处理原任务', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '确认恢复资源', exact: true })
  await expect(dialog).toContainText('上传中断，请重试原任务')
  await expect(dialog.getByRole('button', { name: `重试 ${job.title}`, exact: true })).toBeEnabled()
})

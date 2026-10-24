import { test, expect } from './fixtures'
import type { FileNode, Job } from '../src/api'

function waitingTask(): Job {
  return {
    id: 'space-task', account_id: 'current', kind: 'teldrive_upload', lane: 'teldrive',
    title: '上传 · 等待空间的影片.mp4', state: 'waiting', wait_reason: 'disk_space',
    progress: 12, attempts: 0, created: 1792840000, next_run: Math.floor(Date.now() / 1000) + 60,
    message: '等待磁盘空间：还需 3602 MiB，当前可用 1788 MiB；空间释放后自动继续，已有缓存和上传进度已保留。',
  }
}

test('disk-space waiting stays active with pause controls and automatically reflects later progress', async ({ page }) => {
  let job = waitingTask()
  const manual: Job = { ...waitingTask(), id: 'manual-pause', title: '上传 · 手动暂停.mp4', state: 'paused', wait_reason: undefined, next_run: 0, message: '已由管理员暂停' }
  await page.route('**/api/v1/events**', route => route.abort())
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs: [job, manual], retry_summary: { account_id: 'current', count: 1 } } }))
  await page.route('**/api/v1/jobs/space-task', route => route.fulfill({ json: { job, problems: {}, transfers: {} } }))
  await page.goto('/tasks')
  await page.getByRole('button', { name: /^进行中/ }).click()
  const card = page.locator('.task-card')
  await expect(card).toHaveCount(1)
  await expect(card.locator('.status')).toHaveText('等待磁盘空间')
  await expect(card).toContainText('下次检查')
  await expect(card).toContainText('空间释放后自动继续')
  await expect(card.getByRole('button', { name: `暂停 ${job.title}`, exact: true })).toBeVisible()
  await expect(card.getByRole('button', { name: `重试 ${job.title}`, exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: `查看 ${job.title} 的详情`, exact: true }).click()
  const detail = page.getByRole('dialog')
  await expect(detail.locator('.status')).toHaveText('等待磁盘空间')
  await expect(detail.getByRole('button', { name: '重试原任务' })).toHaveCount(0)
  await detail.getByRole('button', { name: '关闭', exact: true }).last().click()
  await expect(detail).not.toBeVisible()

  for (const width of [1440, 360]) {
    await page.setViewportSize({ width, height: 960 })
    await expect(card.locator('.status')).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await page.screenshot({ path: `../artifacts/disk-space-wait-${width}.png`, animations: 'disabled' })
  }

  // No refresh or retry click: the fallback poll must show the same task
  // advancing. A stale reason must not disguise queued/running/completed state.
  job = { ...job, state: 'queued', message: '空间已释放，等待继续原任务' }
  await expect(card.locator('.status')).toHaveText('排队中')
  job = { ...job, state: 'running', progress: 34, message: '正在继续原上传会话' }
  await expect(card.locator('.status')).toHaveText('进行中')
  job = { ...job, state: 'completed', progress: 100, message: '保存核验通过，缓存已释放' }
  await expect(card).toHaveCount(0)
  await page.getByRole('button', { name: '已完成', exact: true }).click()
  await expect(card.locator('.status')).toHaveText('已完成')
  await page.getByRole('button', { name: '需要处理', exact: true }).click()
  await expect(card).toHaveCount(1)
  await expect(card).toContainText('手动暂停.mp4')
  await expect(card.locator('.status')).toHaveText('已暂停')
})

test('pausing a disk-space wait keeps the original job available for manual continuation', async ({ page }) => {
  let job = waitingTask()
  let retries = 0
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs: [job], retry_summary: { account_id: 'current', count: job.state === 'paused' ? 1 : 0 } } }))
  await page.route('**/api/v1/jobs/space-task/pause', route => {
    job = { ...job, state: 'paused', wait_reason: undefined, message: '已由管理员暂停', next_run: 0 }
    return route.fulfill({ json: job })
  })
  await page.route('**/api/v1/jobs/space-task/retry', route => { retries++; return route.fulfill({ json: job }) })
  await page.goto('/tasks')
  await page.getByRole('button', { name: `暂停 ${job.title}`, exact: true }).click()
  await page.getByRole('button', { name: '需要处理', exact: true }).click()
  await expect(page.locator('.task-card .status')).toHaveText('已暂停')
  await expect(page.getByRole('button', { name: `重试 ${job.title}`, exact: true })).toBeVisible()
  await expect(page.locator('.task-card')).toContainText('12%')
  expect(retries).toBe(0)
})

test('a transferring file can show its disk-space reason without claiming cloud activity', async ({ page }) => {
  const job = waitingTask()
  const file: FileNode = {
    id: 'space-file', parent_id: 'root', name: '等待空间的影片.mp4', kind: 'file', size: 3 << 28,
    hash: '', mime: 'video/mp4', source_id: 'teldrive-source', source_path: '', source_key: '',
    favorite: false, trashed: false, created: 1792840000, modified: 1792840000, opened: 0,
    position: 0, revision: 1, state: 'pending', checked: 0,
    transfer: { job_id: job.id, state: job.state, wait_reason: job.wait_reason, progress: job.progress, message: job.message },
  }
  await page.route('**/api/v1/files?*', route => route.fulfill({ json: { files: [file], total: 1, page: 0, limit: 100, transferring: 1, breadcrumbs: [] } }))
  await page.goto('/files')
  const card = page.getByTestId('transfer-card')
  await expect(card).toHaveAttribute('aria-label', '等待空间的影片.mp4，等待磁盘空间')
  await expect(card.getByRole('status')).toContainText('等待磁盘空间')
  await expect(card.locator('.transfer-note')).toHaveText('空间释放后自动继续')
  await expect(card.locator('.transfer-label .spin')).toHaveCount(0)
})

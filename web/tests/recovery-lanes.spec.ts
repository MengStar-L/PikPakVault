import { test, expect } from './fixtures'
import type { Job } from '../src/api'

const makeJob = (id: string, title: string, lane: Job['lane']): Job => ({
  id, title, lane, account_id: 'lane-account', kind: 'recover', state: 'running',
  progress: 20, message: '', attempts: 0, next_run: 0, created: 1792425600,
})

test('mixed recovery shows two lanes with independent controls and compact responsive labels', async ({ page }) => {
  const jobs = [
    makeJob('cloud-lane', '恢复 · 云端来源', 'cloud'),
    makeJob('teldrive-lane', '恢复 · TelDrive 来源', 'teldrive'),
  ]
  const actions: string[] = []
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs } }))
  await page.route(/\/api\/v1\/jobs\/(cloud-lane|teldrive-lane)(?:\/(pause|retry))?$/, route => {
    const [, id, action] = new URL(route.request().url()).pathname.match(/\/jobs\/([^/]+)(?:\/([^/]+))?$/)!
    const job = jobs.find(item => item.id === id)!
    if (action) {
      actions.push(`${id}/${action}`)
      job.state = action === 'pause' ? 'paused' : 'waiting'
      return route.fulfill({ json: job })
    }
    return route.fulfill({ json: { job, problems: {}, transfers: {} } })
  })
  await page.route('**/api/v1/recovery/preview', route => route.fulfill({ json: {
    account: { id: 'lane-account', name: '测试账号' }, skipped_pending: 0,
    items: [
      { id: 'cloud-file', name: '磁链视频.mp4', path: '/电影/磁链视频.mp4', state: 'missing', size: 1024, method: 'source', has_source: true, conflict: false },
      { id: 'td-file', name: 'TelDrive 视频.mp4', path: '/收藏/TelDrive 视频.mp4', state: 'missing', size: 1024, method: 'source', has_source: true, conflict: false },
    ], bytes: 2048, transfer_bytes: 2048, available: 102400, capacity_warning: false,
  } }))
  await page.route('**/api/v1/recovery', route => {
    expect(route.request().postDataJSON()).toMatchObject({ ids: ['cloud-file', 'td-file'], account_id: 'lane-account', confirm: true })
    return route.fulfill({ status: 202, json: { ...jobs[0], jobs } })
  })
  await page.goto('/recovery')
  await page.getByRole('button', { name: '恢复全部缺失', exact: true }).click()
  await expect(page.locator('.recovery-lanes-note')).toContainText('两路并行')
  await expect(page.locator('.recovery-lanes-note')).toContainText('磁链仍可能需要等待 PikPak 下载')
  await page.getByRole('button', { name: '确认开始恢复', exact: true }).click()
  await expect(page).toHaveURL(/\/tasks$/)
  const cloud = page.locator('.task-card').filter({ hasText: '恢复 · 云端来源' })
  const teldrive = page.locator('.task-card').filter({ hasText: '恢复 · TelDrive 来源' })
  await expect(cloud.locator('.job-lane')).toHaveText('云端转存')
  await expect(teldrive.locator('.job-lane')).toHaveText('TelDrive 下载上传')
  for (const width of [1440, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 900 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    for (const card of [cloud, teldrive]) {
      expect(await card.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true)
    }
    if (width === 1440 || width === 390) {
      await page.screenshot({ path: `../artifacts/recovery-lanes-${width}.png`, animations: 'disabled' })
    }
  }
  await teldrive.getByRole('button', { name: '暂停 恢复 · TelDrive 来源', exact: true }).click()
  await expect(teldrive.locator('.status')).toHaveText('已暂停')
  await expect(cloud.locator('.status')).toHaveText('进行中')
  expect(actions).toEqual(['teldrive-lane/pause'])
  await teldrive.getByRole('button', { name: '查看 恢复 · TelDrive 来源 的详情', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.locator('.job-lane')).toHaveText('TelDrive 下载上传')
  await dialog.getByRole('button', { name: '重试原任务', exact: true }).click()
  await expect(dialog.locator('.status')).toHaveText('云端处理中')
  await expect(cloud.locator('.status')).toHaveText('进行中')
  expect(actions).toEqual(['teldrive-lane/pause', 'teldrive-lane/retry'])
})

test('old TelDrive uploads retain lane label without labelling unrelated checks', async ({ page }) => {
  const upload = { ...makeJob('old-upload', '上传 · 原有任务', undefined), kind: 'teldrive_upload', state: 'paused' }
  const scan = { ...makeJob('scan', '完整检查', undefined), kind: 'scan', state: 'completed' }
  await page.route('**/api/v1/jobs', route => route.fulfill({ json: { jobs: [upload, scan] } }))
  await page.goto('/tasks')
  await expect(page.locator('.task-card').filter({ hasText: '上传 · 原有任务' }).locator('.job-lane')).toHaveText('TelDrive 下载上传')
  await expect(page.locator('.task-card').filter({ hasText: '完整检查' }).locator('.job-lane')).toHaveCount(0)
})

import { test, expect } from './fixtures'
import { readFile } from 'node:fs/promises'

test('update check, installation confirmation and reconnect status are responsive', async ({ page }) => {
  await page.goto('/settings'); await expect(page.getByRole('heading', { name: '设置', exact: true })).toBeVisible()
  const info = { current: '0.2.0', repository: 'MengStar-L/PikPakVault', checked: 1, available: true, can_install: true, reason: '', error: '', release: { tag_name: 'v0.3.0', html_url: 'https://github.com/MengStar-L/PikPakVault/releases/tag/v0.3.0', body: '改进更新与恢复流程。\n' + '长版本说明'.repeat(100), assets: [] }, status: { phase: '', message: '', tag: '' } }
  let submitted = 0
  await page.route('**/api/v1/updates', r => r.fulfill({ json: info }))
  await page.route('**/api/v1/updates/check', r => r.fulfill({ json: info }))
  await page.route('**/api/v1/updates/install', r => { submitted++; expect(r.request().postDataJSON()).toEqual({ tag: 'v0.3.0', confirm: true }); return r.fulfill({ status: 202, json: { ...info, status: { phase: 'verifying', message: '正在启动新版本并检查数据库', tag: 'v0.3.0' } } }) })
  await page.reload(); await page.getByRole('button', { name: '检查更新', exact: true }).click()
  for (const width of [1440, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 900 }); await page.locator('.update-section').scrollIntoViewIfNeeded()
    await expect(page.getByText('发现新版本 v0.3.0')).toBeVisible()
    await page.getByRole('button', { name: '安装更新', exact: true }).click()
    expect(submitted).toBe(0)
    await expect(page.getByRole('dialog')).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    const bounds = await page.getByRole('dialog').boundingBox(); expect(bounds!.x).toBeGreaterThanOrEqual(0); expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width + 1)
    await page.getByRole('button', { name: '取消', exact: true }).click()
  }
  await page.getByRole('button', { name: '安装更新', exact: true }).click(); await page.getByRole('button', { name: '确认更新并重启' }).click()
  await expect(page.getByText('正在启动新版本并检查数据库')).toBeVisible(); expect(submitted).toBe(1)
})

test('full export imports into initialization wizard and requires imported login', async ({ page, browser }) => {
  await page.goto('/settings'); await expect(page.getByRole('heading', { name: '设置', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '下载完整备份' })).toBeVisible()
  const downloadPromise = page.waitForEvent('download'); await page.getByRole('button', { name: '下载完整备份' }).click(); const download = await downloadPromise
  const buffer = await readFile((await download.path())!)
  const fresh = await browser.newContext(); const target = await fresh.newPage()
  await target.goto('http://127.0.0.1:8089'); await target.getByRole('button', { name: '从完整备份导入' }).click()
  await target.setViewportSize({ width: 390, height: 900 })
  await target.getByRole('dialog').getByLabel('初始化代码').fill('vault-e2e-setup')
  await target.getByLabel('完整备份文件').setInputFiles({ name: 'pikpak-vault-backup.zip', mimeType: 'application/zip', buffer })
  await target.getByRole('button', { name: '校验并预览备份' }).click()
  await expect(target.locator('.backup-preview')).toBeVisible()
  await expect(target.getByRole('button', { name: '确认导入全部数据' })).toBeDisabled()
  expect(await target.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await target.getByRole('checkbox').check(); await target.getByRole('button', { name: '确认导入全部数据' }).click()
  await target.getByPlaceholder('输入你的管理员密码').fill('vault-e2e-password'); await target.getByRole('button', { name: '进入资源库' }).click()
  await expect(target.locator('.import-review')).toBeVisible(); await expect(target.locator('.file-card').first()).toBeVisible()
  await target.goto('http://127.0.0.1:8089/settings')
  await target.getByRole('button', { name: '导入完整备份', exact: true }).click()
  await target.getByLabel('完整备份文件').setInputFiles({ name: 'bad.zip', mimeType: 'application/zip', buffer: Buffer.from('invalid') })
  await target.getByRole('button', { name: '校验并预览备份' }).click(); await expect(target.getByRole('dialog').getByRole('alert')).toContainText('ZIP')
  await target.getByRole('button', { name: '取消', exact: true }).click()
  await target.getByRole('button', { name: '已核对，恢复后台检查' }).click(); await expect(target.locator('.import-review')).toHaveCount(0)
  await fresh.close()
})

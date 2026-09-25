import { test, expect } from './fixtures'
import type { Page, Route } from '@playwright/test'

type Folder = { id: string; name: string; parent_id: string }
const folders: Folder[] = [
  { id: 'picker-movies', name: '电影收藏', parent_id: 'root' },
  { id: 'picker-series', name: '剧集收藏', parent_id: 'root' },
  { id: 'picker-season', name: '夏季精选', parent_id: 'picker-movies' },
  { id: 'picker-empty', name: '空目录_保存时无需再次确认_很长的目录名称'.repeat(3), parent_id: 'picker-season' },
]
const byID = (id: string) => folders.find(folder => folder.id === id)!
const fullPath = (id: string): string => id === 'root' ? '我的文件' : `${fullPath(byID(id).parent_id)} / ${byID(id).name}`
const magnet = 'magnet:?xt=urn:btih:' + 'c'.repeat(40) + '&dn=Picker-interaction'

function listing(id: string) {
  const breadcrumbs: { id: string; name: string }[] = []
  for (let current = id; current !== 'root'; current = byID(current).parent_id) {
    const folder = byID(current)
    breadcrumbs.unshift({ id: folder.id, name: folder.name })
  }
  const files = folders.filter(folder => folder.parent_id === id).map(folder => ({ ...folder, kind: 'folder', state: 'present', mime: '', size: 0, created: 0 }))
  return { files, breadcrumbs, total: files.length, page: 0, limit: 100 }
}

async function mockPicker(page: Page, intercept?: (route: Route, parent: string) => Promise<boolean>) {
  await page.route('**/api/v1/files?**', async route => {
    const params = new URL(route.request().url()).searchParams
    if (params.get('transfers') !== '0') return route.continue()
    const parent = params.get('parent') || 'root'
    if (intercept && await intercept(route, parent)) return
    await route.fulfill({ json: listing(parent) })
  })
}

async function openImport(page: Page) {
  await page.goto('/files')
  await page.locator('.heading-actions').getByRole('button', { name: '添加资源', exact: true }).click()
  await page.getByLabel('资源链接').fill(magnet)
  await page.locator('.destination-row').getByRole('button').click()
  await expect(page.locator('.folder-picker')).toBeVisible()
}

async function choose(page: Page, id: string) {
  await page.locator('.picker-list').getByRole('button', { name: byID(id).name, exact: true }).click()
}

async function expectDestination(page: Page, id: string) {
  // Compare the entire trail while allowing the visual separator to change.
  const names = fullPath(id).split(' / ')
  for (const name of names) await expect(page.locator('.destination-row')).toContainText(name)
  await expect(page.locator('.picker-crumb').getByRole('button', { name: id === 'root' ? '我的文件' : byID(id).name, exact: true })).toBeVisible()
}

async function captureImports(page: Page) {
  const submitted: { items: { parent_id: string; link: string }[] }[] = []
  await page.route('**/api/v1/imports', route => {
    submitted.push(route.request().postDataJSON())
    return route.fulfill({ status: 202, json: { jobs: [] } })
  })
  return submitted
}

test('picker selects on navigation and only explicit save submits the chosen destination', async ({ page }) => {
  await mockPicker(page)
  const submitted = await captureImports(page)
  await openImport(page)
  await expect(page.locator('.folder-picker').getByRole('button', { name: '选择当前文件夹' })).toHaveCount(0)
  await choose(page, 'picker-movies')
  await choose(page, 'picker-season')
  await choose(page, 'picker-empty')
  await expectDestination(page, 'picker-empty')
  await expect(page.locator('.picker-list').getByRole('button')).toHaveCount(0)
  expect(submitted).toHaveLength(0)

  await page.getByRole('button', { name: '返回上级', exact: true }).click()
  await expectDestination(page, 'picker-season')
  await page.locator('.picker-crumb').getByRole('button', { name: '电影收藏', exact: true }).click()
  await expectDestination(page, 'picker-movies')
  await page.locator('.picker-crumb').getByRole('button', { name: '我的文件', exact: true }).click()
  await expectDestination(page, 'root')
  expect(submitted).toHaveLength(0)

  // Enter navigates/selects without submitting the containing resource form.
  const series = page.locator('.picker-list').getByRole('button', { name: '剧集收藏', exact: true })
  await series.focus()
  await series.press('Enter')
  await expectDestination(page, 'picker-series')
  expect(submitted).toHaveLength(0)
  const inertDuringClose = await page.evaluate(() => new Promise<boolean>(resolve => {
    const panel = document.querySelector('.destination-picker-panel')!
    const observer = new MutationObserver(() => {
      if (panel.hasAttribute('inert')) { observer.disconnect(); resolve(true) }
    })
    observer.observe(panel, { attributes: true, attributeFilter: ['inert'] })
    ;(document.querySelector('.destination-row button') as HTMLButtonElement).click()
    setTimeout(() => { observer.disconnect(); resolve(false) }, 1000)
  }))
  expect(inertDuringClose).toBe(true)
  await expect(page.locator('.folder-picker')).toHaveCount(0)
  await page.locator('.destination-row').getByRole('button').click()
  await expectDestination(page, 'picker-series')
  await page.getByRole('button', { name: '开始保存', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(submitted).toEqual([{ items: [{ link: magnet, parent_id: 'picker-series', pass_code: '', selected: [], preview: [] }] }])
  await expect(page).toHaveURL(/\/files$/)
})

test('late folder responses cannot overwrite a newer destination', async ({ page }) => {
  let release!: () => void
  const pending = new Promise<void>(resolve => { release = resolve })
  let requested = false
  await mockPicker(page, async (route, parent) => {
    if (parent !== 'picker-movies') return false
    requested = true
    await pending
    await route.fulfill({ json: listing(parent) })
    return true
  })
  const submitted = await captureImports(page)
  await openImport(page)
  await choose(page, 'picker-movies')
  await expect.poll(() => requested).toBe(true)
  // Destination feedback and ancestors are available before children finish loading.
  await expectDestination(page, 'picker-movies')
  await page.locator('.picker-crumb').getByRole('button', { name: '我的文件', exact: true }).click()
  await choose(page, 'picker-series')
  await expectDestination(page, 'picker-series')
  const loaded = page.waitForResponse(response => response.url().includes('parent=picker-movies') && response.status() === 200)
  release()
  await loaded
  await expectDestination(page, 'picker-series')
  await expect(page.locator('.picker-list')).not.toContainText('夏季精选')
  expect(submitted).toHaveLength(0)
  await page.getByRole('button', { name: '开始保存', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(submitted[0].items[0].parent_id).toBe('picker-series')
})

test('failed directory loading keeps the selected path and retries without saving resources', async ({ page }) => {
  let fail = true
  await mockPicker(page, async (route, parent) => {
    if (parent !== 'picker-movies' || !fail) return false
    await route.fulfill({ status: 503, json: { error: '目录读取暂时失败，请重试' } })
    return true
  })
  const submitted = await captureImports(page)
  await openImport(page)
  await choose(page, 'picker-movies')
  await expect(page.locator('.folder-picker').getByRole('alert')).toContainText('目录读取暂时失败')
  await expectDestination(page, 'picker-movies')
  expect(submitted).toHaveLength(0)
  fail = false
  await page.locator('.folder-picker').getByRole('button', { name: '重试', exact: true }).click()
  await choose(page, 'picker-season')
  await expectDestination(page, 'picker-season')
  expect(submitted).toHaveLength(0)
  await page.getByRole('button', { name: '开始保存', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(submitted[0].items[0].parent_id).toBe('picker-season')
})

test('opening a deep destination keeps its ancestors during pagination failure and retry', async ({ page }) => {
  let fail = true
  let pageTwoRequested = false
  let release!: () => void
  const pending = new Promise<void>(resolve => { release = resolve })
  await page.route('**/api/v1/files?**', async route => {
    const params = new URL(route.request().url()).searchParams
    const parent = params.get('parent') || 'picker-season'
    if (parent !== 'picker-season') return route.fulfill({ json: listing(parent) })
    if (params.get('page') === '1') {
      pageTwoRequested = true
      if (fail) {
        await pending
        return route.fulfill({ status: 503, json: { error: '第二页读取失败' } })
      }
      return route.fulfill({ json: { ...listing(parent), page: 1, total: 101 } })
    }
    return route.fulfill({ json: { ...listing(parent), total: 101 } })
  })
  const submitted = await captureImports(page)
  await page.goto('/files?folder=picker-season')
  await page.locator('.heading-actions').getByRole('button', { name: '添加资源', exact: true }).click()
  await page.getByLabel('资源链接').fill(magnet)
  await page.locator('.destination-row').getByRole('button').click()
  await expectDestination(page, 'picker-season')
  await page.locator('.folder-picker').getByRole('button', { name: '下一页', exact: true }).click()
  await expect.poll(() => pageTwoRequested).toBe(true)
  await expectDestination(page, 'picker-season')
  await expect(page.getByRole('button', { name: '返回上级', exact: true })).toBeEnabled()
  release()
  await expect(page.locator('.folder-picker').getByRole('alert')).toContainText('第二页读取失败')
  await expectDestination(page, 'picker-season')
  fail = false
  await page.locator('.folder-picker').getByRole('button', { name: '重试', exact: true }).click()
  await expect(page.locator('.folder-picker .pagination')).toContainText('2 / 2')
  await choose(page, 'picker-empty')
  await expectDestination(page, 'picker-empty')
  expect(submitted).toHaveLength(0)
  await page.getByRole('button', { name: '开始保存', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(submitted[0].items[0].parent_id).toBe('picker-empty')
})

test('deep paths stay usable on desktop and mobile with reduced motion', async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', error => errors.push(error.message))
  await mockPicker(page)
  const submitted = await captureImports(page)
  await openImport(page)
  await choose(page, 'picker-movies')
  await choose(page, 'picker-season')
  await choose(page, 'picker-empty')
  for (const width of [1440, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 844 })
    await expectDestination(page, 'picker-empty')
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    for (const selector of ['[role=dialog]', '.destination-row', '.folder-picker']) {
      expect(await page.locator(selector).evaluate(element => element.scrollWidth <= element.clientWidth + 1)).toBe(true)
    }
    const save = page.getByRole('button', { name: '开始保存', exact: true })
    await expect(save).toBeVisible()
    // Read geometry without scrolling the button into view: the footer must stay on screen.
    const saveBounds = await save.boundingBox()
    expect(saveBounds).not.toBeNull()
    expect(saveBounds!.y).toBeGreaterThanOrEqual(0)
    expect(saveBounds!.y + saveBounds!.height).toBeLessThanOrEqual(845)
    await page.screenshot({ path: `../artifacts/folder-picker-${width}.png`, animations: 'disabled' })
  }
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await page.getByRole('button', { name: '返回上级', exact: true }).click()
  await expectDestination(page, 'picker-season')
  await choose(page, 'picker-empty')
  await expectDestination(page, 'picker-empty')
  await page.locator('.destination-row').getByRole('button').click()
  await expect(page.locator('.folder-picker')).toHaveCount(0)
  await page.locator('.destination-row').getByRole('button').click()
  await expectDestination(page, 'picker-empty')
  expect(submitted).toHaveLength(0)
  await page.getByRole('button', { name: '开始保存', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(submitted[0].items[0].parent_id).toBe('picker-empty')
  expect(errors).toEqual([])
})

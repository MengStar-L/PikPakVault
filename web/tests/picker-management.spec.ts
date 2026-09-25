import { test, expect } from './fixtures'
import type { Page } from '@playwright/test'

const magnet = 'magnet:?xt=urn:btih:' + 'd'.repeat(40) + '&dn=Picker-management'

async function openImport(page: Page) {
  await page.goto('/files')
  await page.locator('.heading-actions').getByRole('button', { name: '添加资源', exact: true }).click()
  await page.getByLabel('资源链接').fill(magnet)
  await page.locator('.destination-row').getByRole('button').click()
  await expect(page.locator('.folder-picker')).toBeVisible()
}

async function startCreate(page: Page, name: string) {
  await page.locator('.folder-picker').getByRole('button', { name: '新建文件夹', exact: true }).click()
  await page.locator('.picker-editor').getByRole('textbox', { name: '文件夹名称', exact: true }).fill(name)
}

async function createFolder(page: Page, name: string): Promise<string> {
  await startCreate(page, name)
  const response = page.waitForResponse(r => new URL(r.url()).pathname === '/api/v1/files' && r.request().method() === 'POST')
  await page.locator('.picker-editor').getByRole('button', { name: '创建并进入', exact: true }).click()
  const result = await response
  expect(result.ok()).toBe(true)
  const created = await result.json()
  await expect(page.locator('.picker-crumb')).toContainText(name)
  await expect(page.locator('.picker-editor')).toHaveCount(0)
  return created.id
}

async function folderMenu(page: Page, name?: string) {
  await page.locator('.folder-picker').getByRole('button', { name: name ? `${name} 的文件夹操作` : '当前文件夹操作', exact: true }).click()
}

test('picker manages real folders inline and recycling the current folder returns to its parent', async ({ page }) => {
  const prefix = `E2E路径管理_${Date.now()}`
  const submitted: unknown[] = []
  const touched: string[] = []
  let creates = 0
  page.on('request', request => { if (new URL(request.url()).pathname === '/api/v1/files' && request.method() === 'POST') creates++ })
  await page.route('**/api/v1/imports', route => { submitted.push(route.request().postDataJSON()); return route.fulfill({ status: 202, json: { jobs: [] } }) })
  const { csrf } = await (await page.request.get('/api/v1/auth/status')).json()
  const file = async (id: string) => (await (await page.request.get(`/api/v1/files/${id}`)).json()).file
  try {
    await openImport(page)
    await startCreate(page, prefix + '_不应创建')
    await page.locator('.picker-editor').getByRole('button', { name: '取消', exact: true }).press('Enter')
    await expect(page.locator('.picker-editor')).toHaveCount(0)
    expect(creates).toBe(0)
    await startCreate(page, prefix + '_Escape取消')
    await page.locator('.picker-editor').getByRole('textbox', { name: '文件夹名称' }).press('Escape')
    await expect(page.locator('.picker-editor')).toHaveCount(0)
    await expect(page.getByRole('dialog')).toHaveCount(1)
    expect(creates).toBe(0)
    const parent = await createFolder(page, prefix)
    touched.push(parent)
    expect((await file(parent)).parent_id).toBe('root')
    expect(submitted).toHaveLength(0)

    await folderMenu(page)
    await page.getByRole('menuitem', { name: '重命名', exact: true }).click()
    await page.locator('.picker-editor').getByRole('textbox', { name: '文件夹名称' }).fill(prefix + '_已整理')
    // Enter belongs to the inline editor and must never submit the surrounding import form.
    await page.locator('.picker-editor').getByRole('textbox', { name: '文件夹名称' }).press('Enter')
    await expect.poll(async () => (await file(parent)).name).toBe(prefix + '_已整理')
    await expect(page.locator('.destination-path')).toContainText(prefix + '_已整理')
    expect(submitted).toHaveLength(0)
    await folderMenu(page)
    await page.getByRole('menuitem', { name: '重命名', exact: true }).click()
    await page.locator('.picker-editor').getByRole('textbox', { name: '文件夹名称' }).fill('取消不应该保存这个名称')
    await page.locator('.picker-editor').getByRole('button', { name: '取消', exact: true }).press('Enter')
    await expect(page.locator('.picker-editor')).toHaveCount(0)
    expect((await file(parent)).name).toBe(prefix + '_已整理')

    const child = await createFolder(page, '子目录')
    touched.push(child)
    const grandchild = await createFolder(page, '保留备份记录的后代')
    touched.push(grandchild)
    await page.locator('.folder-picker').getByRole('button', { name: '返回上级', exact: true }).click()
    await page.locator('.folder-picker').getByRole('button', { name: '返回上级', exact: true }).click()
    await folderMenu(page, '子目录')
    await page.getByRole('menuitem', { name: '重命名', exact: true }).click()
    await page.locator('.picker-editor').getByRole('textbox', { name: '文件夹名称' }).fill('新的子目录名称')
    await page.locator('.picker-editor').getByRole('button', { name: '保存名称', exact: true }).click()
    await expect(page.locator('.picker-list').getByRole('button', { name: '新的子目录名称', exact: true })).toBeVisible()
    await expect(page.locator('.destination-path')).not.toContainText('新的子目录名称')
    await page.locator('.picker-list').getByRole('button', { name: '新的子目录名称', exact: true }).click()
    await folderMenu(page)
    await page.getByRole('menuitem', { name: '移到回收站', exact: true }).click()
    const editor = page.locator('.picker-editor')
    await expect(editor).toContainText('回收站')
    await expect(editor).toContainText(/子|内容/)
    expect((await file(child)).trashed).toBe(false)
    await editor.getByRole('button', { name: '取消', exact: true }).press('Enter')
    expect((await file(child)).trashed).toBe(false)
    await folderMenu(page)
    await page.getByRole('menuitem', { name: '移到回收站', exact: true }).click()
    await page.locator('.picker-editor').getByRole('button', { name: '移到回收站', exact: true }).press('Enter')
    await expect.poll(async () => (await file(child)).trashed).toBe(true)
    expect((await file(grandchild)).trashed).toBe(true)
    await expect(page.locator('.destination-path')).toHaveText(`我的文件 / ${prefix}_已整理`)
    await expect(page.locator('.picker-list').getByRole('button', { name: '新的子目录名称', exact: true })).toHaveCount(0)
    expect(submitted).toHaveLength(0)
    await expect(page.getByRole('dialog')).toHaveCount(1)
    await page.getByRole('button', { name: '开始保存', exact: true }).click()
    await expect(page.getByRole('dialog')).toHaveCount(0)
    expect(submitted).toHaveLength(1)
    expect(submitted[0]).toMatchObject({ items: [{ parent_id: parent }] })
  } finally {
    // The shared fixture's other tests assert an empty recycle bin before their own flow.
    // Restore only this test's temporary records; never purge or touch fixture-owned files.
    if (touched.length) {
      const restore = await page.request.post('/api/v1/files/action', { data: { action: 'restore', ids: touched }, headers: { 'X-CSRF-Token': csrf } })
      expect(restore.ok()).toBe(true)
    }
  }
})

test('picker failures preserve input and in-flight writes block navigation and parent-form submission', async ({ page }) => {
  let created = false
  let createAttempts = 0
  let renameAttempts = 0
  let deletes = 0
  let submitted = 0
  let release!: () => void
  const delayed = new Promise<void>(resolve => { release = resolve })
  await page.route('**/api/v1/imports', route => { submitted++; return route.fulfill({ status: 202, json: { jobs: [] } }) })
  await page.route('**/api/v1/files?**', route => {
    const query = new URL(route.request().url()).searchParams
    if (query.get('transfers') !== '0') return route.continue()
    const isRoot = query.get('parent') === 'root'
    const files = isRoot ? [{ id: 'sibling', name: '另一个目录', kind: 'folder' }, ...(created ? [{ id: 'new-folder', name: '暂存内容', kind: 'folder' }] : [])] : []
    return route.fulfill({ json: { files, total: files.length, page: 0, limit: 100, breadcrumbs: isRoot ? [] : [{ id: 'new-folder', name: '暂存内容' }] } })
  })
  await page.route('**/api/v1/files', async route => {
    createAttempts++
    if (createAttempts === 1) return route.fulfill({ status: 409, json: { error: '同名目录已经存在，请修改名称' } })
    await delayed
    created = true
    return route.fulfill({ status: 201, json: { id: 'new-folder' } })
  })
  await page.route('**/api/v1/files/action', route => {
    const body = route.request().postDataJSON()
    if (body.action === 'rename') { renameAttempts++; return route.fulfill({ status: 409, json: { error: '目标名称已被使用' } }) }
    expect(body.action).toBe('trash'); deletes++
    return route.fulfill({ status: 503, json: { error: '暂时无法移到回收站，请稍后重试' } })
  })
  await openImport(page)
  await startCreate(page, '暂存内容')
  const editor = page.locator('.picker-editor')
  await editor.getByRole('button', { name: '创建并进入', exact: true }).click()
  await expect(editor.getByRole('alert')).toContainText('同名目录已经存在')
  await expect(editor.getByRole('textbox', { name: '文件夹名称' })).toHaveValue('暂存内容')
  expect(submitted).toBe(0)
  await editor.getByRole('button', { name: '创建并进入', exact: true }).click()
  await expect.poll(() => createAttempts).toBe(2)
  await expect(page.locator('.picker-crumb').getByRole('button', { name: '我的文件', exact: true })).toBeDisabled()
  await expect(page.locator('.picker-list').getByRole('button', { name: '另一个目录', exact: true })).toBeDisabled()
  await expect(editor.getByRole('button', { name: '创建并进入', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: '开始保存', exact: true }).evaluate((element: HTMLButtonElement) => element.click())
  expect(submitted).toBe(0)
  release()
  await expect(page.locator('.destination-path')).toHaveText('我的文件 / 暂存内容')
  await expect(page.locator('.picker-editor')).toHaveCount(0)
  expect(createAttempts).toBe(2)
  await folderMenu(page)
  await page.getByRole('menuitem', { name: '重命名', exact: true }).click()
  await editor.getByRole('textbox', { name: '文件夹名称' }).fill('与现有文件重名')
  await editor.getByRole('button', { name: '保存名称', exact: true }).click()
  await expect(editor.getByRole('alert')).toContainText('目标名称已被使用')
  await expect(editor.getByRole('textbox', { name: '文件夹名称' })).toHaveValue('与现有文件重名')
  expect(renameAttempts).toBe(1)
  await editor.getByRole('button', { name: '取消', exact: true }).click()
  await folderMenu(page)
  await page.getByRole('menuitem', { name: '移到回收站', exact: true }).click()
  await editor.getByRole('button', { name: '移到回收站', exact: true }).click()
  await expect(editor.getByRole('alert')).toContainText('暂时无法移到回收站')
  await expect(page.locator('.destination-path')).toHaveText('我的文件 / 暂存内容')
  expect(deletes).toBe(1)
  expect(submitted).toBe(0)
})

test('RSS picker inline management stays usable on mobile and fixes an empty final page after recycling', async ({ page }) => {
  const longName = '很长的订阅保存目录与中文名称_'.repeat(8)
  let recycled = false
  let saved = 0
  let total = 101
  let title = longName
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await page.route('**/api/v1/rss', route => {
    if (route.request().method() === 'GET') return route.fulfill({ json: { subscriptions: [], active_account: 'a' } })
    saved++; return route.fulfill({ status: 201, json: { id: 'management-rss' } })
  })
  await page.route('**/api/v1/files?**', route => {
    const query = new URL(route.request().url()).searchParams
    if (query.get('transfers') !== '0') return route.continue()
    const currentPage = Number(query.get('page'))
    const root = query.get('parent') === 'root'
    const files = !root ? [] : currentPage ? recycled ? [] : [{ id: 'last-folder', name: title, kind: 'folder' }] : [{ id: 'first-folder', name: '第一页文件夹', kind: 'folder' }]
    return route.fulfill({ json: { files, total: root ? total : 0, page: currentPage, limit: 100, breadcrumbs: root ? [] : [{ id: query.get('parent'), name: title }] } })
  })
  await page.route('**/api/v1/files/action', route => {
    const body = route.request().postDataJSON()
    if (body.action === 'rename') title = body.name
    else { expect(body.action).toBe('trash'); expect(body.ids).toEqual(['last-folder']); recycled = true; total = 100 }
    return route.fulfill({ json: { ok: true } })
  })
  await page.goto('/rss')
  await page.getByRole('button', { name: '添加订阅', exact: true }).click()
  await page.getByRole('textbox', { name: '订阅名称', exact: true }).fill('路径管理兼容')
  await page.getByLabel('RSS / Atom 地址').fill('https://rss.example.test/feed.xml')
  await page.locator('.destination-row button').click()
  const picker = page.locator('.folder-picker')
  await picker.getByRole('button', { name: '下一页', exact: true }).click()
  await folderMenu(page, title)
  await page.getByRole('menuitem', { name: '重命名', exact: true }).click()
  for (const width of [1440, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 844 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    for (const selector of ['[role=dialog]', '.folder-picker', '.picker-editor']) expect(await page.locator(selector).evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true)
    await expect(page.locator('.picker-editor').getByRole('button', { name: '保存名称', exact: true })).toBeInViewport()
    await page.screenshot({ path: `../artifacts/picker-management-${width}.png`, animations: 'disabled' })
  }
  await page.locator('.picker-editor').getByRole('textbox', { name: '文件夹名称' }).fill(longName + '改名')
  await page.locator('.picker-editor').getByRole('button', { name: '保存名称', exact: true }).click()
  await expect(picker.locator('.picker-list').getByRole('button', { name: longName + '改名', exact: true })).toBeVisible()
  expect(saved).toBe(0)
  await folderMenu(page, title)
  await page.getByRole('menuitem', { name: '移到回收站', exact: true }).click()
  await page.locator('.picker-editor').getByRole('button', { name: '移到回收站', exact: true }).click()
  await expect(picker.locator('.picker-list').getByRole('button', { name: '第一页文件夹', exact: true })).toBeVisible()
  await expect(picker.getByRole('button', { name: '下一页', exact: true })).toHaveCount(0)
  await expect(page.locator('.destination-path')).toHaveText('我的文件')
  expect(saved).toBe(0)
  await page.getByRole('button', { name: '保存订阅', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(saved).toBe(1)
})

test('move and TelDrive destination pickers can create folders without triggering the parent operation', async ({ page }) => {
  let created = 0
  let moves = 0
  let monitors = 0
  const source = { id: 'move-source', name: '待整理文件夹', parent_id: 'root', kind: 'folder', mime: '', size: 0, created: 1789860000, state: 'present', favorite: false }
  await page.route('**/api/v1/files?**', route => {
    const query = new URL(route.request().url()).searchParams
    if (query.get('transfers') !== '0') return route.fulfill({ json: { files: [source], breadcrumbs: [], total: 1, page: 0, limit: 100 } })
    const id = query.get('parent') || 'root'
    return route.fulfill({ json: { files: id === 'root' ? [source] : [], breadcrumbs: id === 'root' ? [] : [{ id, name: id === 'created-1' ? '新的移动目标' : 'TelDrive 新建保存位置' }], total: id === 'root' ? 1 : 0, page: 0, limit: 100 } })
  })
  await page.route('**/api/v1/files', route => {
    created++
    expect(route.request().postDataJSON().parent_id).toBe('root')
    return route.fulfill({ status: 201, json: { id: `created-${created}` } })
  })
  await page.route('**/api/v1/files/action', route => {
    expect(route.request().postDataJSON()).toMatchObject({ action: 'move', ids: ['move-source'], parent_id: 'created-1' })
    moves++
    return route.fulfill({ json: { ok: true } })
  })
  await page.goto('/files')
  await page.getByRole('button', { name: '待整理文件夹 的更多操作', exact: true }).click()
  await page.getByRole('menuitem', { name: '移动到…', exact: true }).click()
  await expect(page.locator('.picker-list').getByRole('button', { name: '待整理文件夹', exact: true })).toHaveCount(0)
  await createFolder(page, '新的移动目标')
  expect(moves).toBe(0)
  await page.getByRole('button', { name: '移动到这里', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(moves).toBe(1)

  await page.route('**/api/v1/teldrive', route => {
    if (route.request().method() === 'GET') return route.fulfill({ json: { monitors: [], active_account: 'a' } })
    monitors++
    expect(route.request().postDataJSON()).toMatchObject({ parent_id: 'created-2', folder_id: 'td-source' })
    return route.fulfill({ json: { id: 'new-monitor' } })
  })
  await page.route('**/api/v1/teldrive/browse', route => route.fulfill({ json: { items: route.request().postDataJSON().folder_id ? [] : [{ id: 'td-source', name: 'Telegram 来源', type: 'folder', size: 0 }], meta: { count: 1, currentPage: 1, totalPages: 1 } } }))
  await page.goto('/teldrive')
  await page.getByRole('button', { name: '添加监控', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('TelDrive 站点地址').fill('https://teldrive.example.test')
  await dialog.getByLabel('TelDrive access_token').fill('fixture-only-token')
  await dialog.getByRole('button', { name: '连接并选择文件夹', exact: true }).click()
  await dialog.getByRole('button', { name: 'Telegram 来源', exact: true }).click()
  await dialog.getByRole('button', { name: '选择当前文件夹', exact: true }).click()
  await dialog.locator('.td-destination button').click()
  await createFolder(page, 'TelDrive 新建保存位置')
  await expect(dialog.locator('.td-destination')).toContainText('TelDrive 新建保存位置')
  expect(monitors).toBe(0)
  await dialog.getByRole('button', { name: '保存监控', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect(monitors).toBe(1)
})

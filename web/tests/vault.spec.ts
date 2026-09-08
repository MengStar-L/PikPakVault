import { test, expect } from './fixtures'
import type { Page } from '@playwright/test'

async function login(page: Page) { await page.goto('/files'); await expect(page.getByPlaceholder('输入你的管理员密码').or(page.getByRole('heading',{name:'我的文件',exact:true}))).toBeVisible(); if (await page.getByPlaceholder('输入你的管理员密码').isVisible()) { await page.getByPlaceholder('输入你的管理员密码').fill('vault-e2e-password'); await page.getByRole('button', { name: '进入资源库' }).click() }; await expect(page.getByRole('heading', { name: '我的文件', exact: true })).toBeVisible() }
async function noOverflow(page: Page) { const bad = await page.evaluate(() => { const all = [...document.querySelectorAll('body *')]; return all.filter(el => { const r = el.getBoundingClientRect(); const style = getComputedStyle(el); return r.width > 0 && r.height > 0 && style.position !== 'fixed' && !el.closest('[data-radix-popper-content-wrapper],.virtual-files,[role=menu],[role=tooltip],.sr-only') && style.visibility !== 'hidden' && (r.right > window.innerWidth + 2 || r.left < -2) }).slice(0, 8).map(el => ({ tag: el.tagName, cls: el.className, right: el.getBoundingClientRect().right, left: el.getBoundingClientRect().left })) }); expect(bad).toEqual([]) }

test('responsive library, content wrapping and dialogs', async ({ page }) => {
  const errors: string[] = []; page.on('pageerror', error => errors.push(error.message));
  await login(page)
  for (const width of [1440, 1024, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 900 }); await page.waitForTimeout(200)
    await expect(page.locator('.file-card').first()).toBeVisible(); await noOverflow(page)
    await page.screenshot({ path: `../artifacts/library-${width}.png`, fullPage: true })
    await page.getByRole('button', { name: '列表视图', exact: true }).click(); await noOverflow(page)
    await page.getByRole('button', { name: '网格视图', exact: true }).click()
    await page.locator('.heading-actions').getByRole('button', { name: '添加资源', exact: true }).click()
    await expect(page.getByRole('dialog')).toBeVisible(); await noOverflow(page)
    await page.getByRole('button', { name: '关闭', exact: true }).click()
  }
  expect(errors).toEqual([])
})

test('file create, rename, favorite, move and recycle flow', async ({ page }) => {
  await login(page); const name = 'E2E 文件夹 ' + Date.now()
  await page.getByRole('button', { name: '新建文件夹', exact: true }).click(); await page.getByLabel('名称', { exact: true }).fill(name); await page.getByRole('button', { name: '创建文件夹', exact: true }).click()
  await page.getByRole('textbox', { name: '搜索文件' }).fill(name)
  await expect(page.locator('.item-count')).toContainText('1'); await expect(page.locator('.file-card').filter({ hasText: name })).toBeVisible()
  await page.getByRole('button', { name: `${name} 的更多操作` }).click(); await page.getByRole('menuitem', { name: '重命名', exact: true }).click(); await page.getByLabel('名称', { exact: true }).fill(name + ' 已整理'); await page.getByRole('button', { name: '保存名称' }).click()
  await expect(page.locator('.file-title')).toHaveCount(1); await expect(page.locator('.file-title')).toHaveText(name + ' 已整理')
  await page.getByRole('button', { name: `${name} 已整理 的更多操作` }).click(); await page.getByRole('menuitem', { name: '收藏', exact: true }).click(); await expect(page.locator('.favorite-star')).toBeVisible()
  await page.getByRole('button', { name: `${name} 已整理 的更多操作` }).click(); await page.getByRole('menuitem', { name: '移动到…', exact: true }).click()
  await page.locator('.picker-list').getByRole('button', { name: '电影时光' }).click(); await page.getByRole('button', { name: '选择当前文件夹' }).click(); await page.getByRole('button', { name: '移动到这里' }).click()
  await page.getByRole('button', { name: `${name} 已整理 的更多操作` }).click(); await page.getByRole('menuitem', { name: '详情与来源', exact: true }).click(); await expect(page.locator('.path-value')).toContainText('/电影时光/'); await page.getByRole('button', { name: '关闭', exact: true }).click()
  await page.getByRole('button', { name: `${name} 已整理 的更多操作` }).click(); await page.getByRole('menuitem', { name: '移到回收站', exact: true }).click(); await page.getByRole('button', { name: '移到回收站', exact: true }).click()
  await expect(page.getByText('没有找到相关内容')).toBeVisible()
  await page.goto('/trash'); await expect(page.locator('.item-count')).toContainText('1'); await expect(page.locator('.file-card').filter({ hasText: name })).toBeVisible()
  await page.getByRole('button', { name: `${name} 已整理 的更多操作` }).click(); await page.getByRole('menuitem', { name: '还原', exact: true }).click()
  await page.goto('/files'); await page.getByRole('textbox', { name: '搜索文件' }).fill(name); await expect(page.locator('.file-title')).toHaveCount(1); await expect(page.locator('.file-title')).toHaveText(name + ' 已整理')
})

test('source import progresses through real backend and recovers deletion', async ({ page }) => {
  await login(page); await page.locator('.heading-actions').getByRole('button', { name: '添加资源', exact: true }).click()
  await page.getByLabel('资源链接').fill('magnet:?xt=urn:btih:' + 'b'.repeat(40) + '&dn=E2E-Import')
  await page.getByRole('button', { name: '开始保存', exact: true }).click(); await expect(page.getByRole('dialog')).toHaveCount(0); await expect(page).toHaveURL(/\/files$/); await page.goto('/tasks'); await expect(page.getByRole('heading', { name: '传输任务', exact: true })).toBeVisible()
  const task = page.locator('.task-card').filter({ hasText: 'E2E-Import' }).first(); await expect(task.locator('.status')).toHaveText('已完成', { timeout: 20000 })
  await page.goto('/files'); await page.getByRole('textbox', { name: '搜索文件' }).fill('one.mp4'); const card = page.locator('.file-card').filter({ hasText: 'one.mp4' }).first(); await expect(card).toBeVisible()
  await card.getByRole('button', { name: /更多操作/ }).click(); await page.getByRole('menuitem', { name: '详情与来源' }).click(); await expect(page.locator('.source-link')).toContainText('urn:btih:'); await page.getByRole('button', { name: '关闭', exact: true }).click()
  const imported = await (await page.request.get('/api/v1/files?search=one.mp4')).json()
  await page.request.post('/__fixture/delete/' + imported.files[0].id)
  await page.goto('/recovery'); await page.getByRole('button', { name: '检查远端', exact: true }).click()
  await expect(page.locator('.recovery-item').filter({ hasText: 'one.mp4' })).toBeVisible({ timeout: 20000 })
  await page.locator('.recovery-item').filter({ hasText: 'one.mp4' }).getByRole('button', { name: '核对恢复' }).click()
  await expect(page.getByRole('heading', { name: '确认恢复资源' })).toBeVisible(); await expect(page.getByText('原始来源重建').or(page.getByText('核对 → 回收站 → 来源'))).toBeVisible()
  await noOverflow(page)
  await page.getByRole('button', { name: '确认开始恢复', exact: true }).click()
  await expect(page.locator('.task-card').filter({ hasText: 'Restore library' }).first().locator('.status')).toHaveText('已完成', { timeout: 20000 })
  const restored = await (await page.request.get('/api/v1/files/' + imported.files[0].id)).json()
  expect(restored.file.state).toBe('present'); expect(restored.file.name).toBe('one.mp4')
})

test('large folder uses pagination and bounded DOM', async ({ page }) => {
  await login(page); await page.goto('/files?folder=folder-3')
  await expect(page.locator('.item-count')).toContainText('1200')
  for (let i = 0; i < 12; i++) { await page.locator('[data-testid=file-scroll]').evaluate(el => { el.scrollTop += 1800 }); await page.waitForTimeout(160) }
  expect(await page.locator('.file-card').count()).toBeLessThan(60)
  await noOverflow(page)
  await page.getByRole('textbox', { name: '搜索文件' }).fill('课程 1199'); await expect(page.locator('.file-title')).toHaveCount(1); await expect(page.locator('.file-title')).toHaveText('课程 1199 — 长目录的流畅浏览.mp4')
})

test('media preview, authenticated proxy and settings backup', async ({ page }) => {
  await login(page); await page.getByRole('textbox', { name: '搜索文件' }).fill('阅读笔记')
  await page.locator('.file-card').filter({ hasText: '阅读笔记' }).dblclick(); await page.getByRole('button', { name: '服务器中转', exact: true }).click()
  await expect(page.locator('.text-preview')).toContainText('真正好的设计')
  await page.screenshot({ path: '../artifacts/text-preview.png' }); await page.getByRole('button', { name: '关闭', exact: true }).click()
  await page.getByRole('textbox', { name: '搜索文件' }).fill('Sunday Morning'); await page.locator('.file-card').filter({ hasText: 'Sunday Morning' }).dblclick(); await page.getByRole('button', { name: '服务器中转', exact: true }).click()
  await expect.poll(() => page.locator('audio').evaluate(el => (el as HTMLAudioElement).readyState)).toBeGreaterThanOrEqual(2)
  await page.locator('audio').evaluate(el => (el as HTMLAudioElement).play()); await expect.poll(() => page.locator('audio').evaluate(el => (el as HTMLAudioElement).currentTime)).toBeGreaterThan(0)
  await expect(page.getByRole('link', { name: '下载文件', exact: true })).toHaveAttribute('href', /[?&]proxy=1/)
  await page.getByRole('button', { name: '浏览器直连', exact: true }).click()
  await expect(page.getByRole('link', { name: '下载文件', exact: true })).toHaveAttribute('href', /[?&]proxy=0/)
  await page.getByRole('button', { name: '关闭', exact: true }).click()
  await page.goto('/settings'); const downloadPromise = page.waitForEvent('download'); await page.getByRole('button', { name: '下载完整备份', exact: true }).click(); const download = await downloadPromise; expect(download.suggestedFilename()).toBe('pikpak-vault-backup.zip')
  await page.setViewportSize({ width: 390, height: 844 }); await noOverflow(page); await page.screenshot({ path: '../artifacts/settings-mobile.png', fullPage: true })
  await page.goto('/accounts'); await noOverflow(page); await page.getByRole('button', { name: '添加账号', exact: true }).click(); await noOverflow(page)
})


test('custom cloud path migrates the root and persists on mobile', async ({ page }) => {
  await login(page)
  const original = await (await page.request.get('/api/v1/accounts')).json()
  const rootID = original.accounts.find((a: {id: string}) => a.id === original.active_id).root_id
  await page.goto('/settings')
  await expect(page.getByLabel('专用目录路径', { exact: true })).toHaveValue('My Pack/PikPakVault')
  const path = 'My Pack/日常资料/收藏'
  await page.getByLabel('专用目录路径', { exact: true }).fill(path)
  await page.getByRole('button', { name: '保存设置', exact: true }).click()
  await expect(page.locator('.root-path-state')).toContainText('当前账号已应用')
  await expect.poll(async () => (await (await page.request.get('/api/v1/settings')).json()).root_path_applied).toBe(path)
  await page.reload()
  await expect(page.getByLabel('专用目录路径', { exact: true })).toHaveValue(path)
  const updated = await (await page.request.get('/api/v1/accounts')).json()
  expect(updated.accounts.find((a: {id: string}) => a.id === original.active_id).root_id).toBe(rootID)
  for (const width of [1440, 768, 390, 360]) {
    await page.setViewportSize({ width, height: 900 }); await noOverflow(page)
    if (width === 390) await page.screenshot({ path: '../artifacts/root-path-settings-mobile.png', fullPage: true })
  }
  await page.getByLabel('专用目录路径', { exact: true }).fill('My Pack/../Other')
  await page.getByRole('button', { name: '保存设置', exact: true }).click()
  await expect(page.getByText('目录路径无效：请用 / 分隔文件夹，不能包含空层级、. 或 ..', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: '恢复默认路径', exact: true }).click()
  await page.getByRole('button', { name: '保存设置', exact: true }).click()
  await expect.poll(async () => (await (await page.request.get('/api/v1/settings')).json()).root_path_applied).toBe('My Pack/PikPakVault')
})

test('folder motion survives slow loading, browser back and refresh without replay', async ({ page }) => {
  await login(page)
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase', 'idle')
  await page.route('**/api/v1/files?*', async route => {
    const url = new URL(route.request().url())
    if (url.searchParams.get('parent') === 'folder-3') await new Promise(resolve => setTimeout(resolve, 550))
    await route.continue()
  })
  await page.evaluate(() => {
    const history: {phase:string; title:string; path:string; opacity:number; transform:string}[] = []
    ;(window as unknown as {motionHistory:typeof history}).motionHistory = history
    const end = performance.now() + 1600
    function frame() {
      const viewport = document.querySelector('.files-viewport')
      const content = document.querySelector('.file-results')
      if (content) { const style = getComputedStyle(content); history.push({phase:viewport?.getAttribute('data-motion-phase')||'',title:document.querySelector('h1')?.textContent||'',path:location.search,opacity:Number(style.opacity),transform:style.transform}) }
      if (performance.now() < end) requestAnimationFrame(frame)
    }
    requestAnimationFrame(frame)
  })
  const folder = page.locator('.file-card').filter({hasText:'学习与探索'}).first()
  await folder.focus(); await folder.press('Enter')
  await expect(page).toHaveURL(/folder=folder-3/)
  await expect(page.getByRole('heading', {name:'学习与探索',exact:true})).toBeVisible()
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase', 'loading')
  await expect(page.locator('.item-count')).toContainText('1200')
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase', 'idle')
  await expect(page.getByRole('heading', {name:'学习与探索',exact:true})).toBeFocused()
  const frames = await page.evaluate(() => (window as unknown as {motionHistory:{phase:string;title:string;path:string;opacity:number;transform:string}[]}).motionHistory)
  expect(frames.some(f => f.phase==='leaving' && f.opacity>0 && f.opacity<1)).toBe(true)
  expect(frames.some(f => f.phase==='entering' && f.opacity>0 && f.opacity<1 && f.transform!=='none')).toBe(true)
  expect(frames.filter(f=>f.path.includes('folder-3')&&f.phase!=='leaving').every(f=>f.title==='学习与探索')).toBe(true)
  await page.goBack()
  await expect(page.getByRole('heading',{name:'我的文件',exact:true})).toBeVisible()
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-direction', 'back')
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase', 'idle')
  await page.getByRole('button', {name:'刷新文件',exact:true}).click()
  await expect(page.locator('[data-testid=file-scroll]')).toHaveAttribute('aria-busy', 'false')
  expect(await page.locator('.file-results').evaluate(el=>el.getAnimations().length)).toBe(0)
  await page.getByRole('button', {name:'列表视图',exact:true}).click()
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase', 'idle')
  await noOverflow(page)
  await page.getByRole('button', {name:'网格视图',exact:true}).click()
})

test('interrupted navigation and delayed error never open an obsolete folder', async ({ page }) => {
  await login(page)
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle')
  await page.locator('.file-card').filter({hasText:'旅途与风景'}).first().focus()
  await page.keyboard.press('Enter')
  // The exit is deliberately short; dispatch a second user action in that window.
  await page.getByRole('button',{name:'我的文件根目录'}).dispatchEvent('click')
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle')
  await page.waitForTimeout(180)
  await expect(page).toHaveURL(/\/files$/)
  await page.route('**/api/v1/files?*',async route=>{
    if(new URL(route.request().url()).searchParams.get('parent')==='folder-2') {
      await new Promise(resolve=>setTimeout(resolve,220))
      await route.fulfill({status:503,contentType:'application/json',body:JSON.stringify({error:'模拟目录读取失败'})})
    } else await route.continue()
  })
  await page.locator('.file-card').filter({hasText:'设计灵感'}).first().dblclick()
  await expect(page.getByRole('heading',{name:'设计灵感',exact:true})).toBeVisible()
  await page.getByRole('button',{name:'我的文件根目录'}).click()
  await page.waitForTimeout(600)
  await expect(page).toHaveURL(/\/files$/)
  await expect(page.getByRole('heading',{name:'我的文件',exact:true})).toBeVisible()
  await expect(page.getByRole('alert').filter({hasText:'模拟目录读取失败'})).toHaveCount(0)
  await page.locator('.file-card').filter({hasText:'设计灵感'}).first().dblclick()
  await expect(page.getByRole('alert').filter({hasText:'模拟目录读取失败'})).toBeVisible()
  await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle')
  await page.getByRole('button',{name:'我的文件根目录'}).click()
  await expect(page).toHaveURL(/\/files$/)
})

test('mobile folder navigation and reduced motion remain usable', async ({ page, browser }) => {
  await login(page)
  const context=await browser.newContext({storageState:await page.context().storageState(),viewport:{width:390,height:844},hasTouch:true,baseURL:'http://127.0.0.1:8088'})
  try {
    const mobile=await context.newPage(); await mobile.goto('/files')
    await expect(mobile.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle')
    await mobile.locator('.file-card').filter({hasText:'旅途与风景'}).first().tap()
    await expect(mobile.getByRole('heading',{name:'旅途与风景',exact:true})).toBeVisible()
    await expect(mobile.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle')
    await expect(mobile.getByText('你的收藏，从这里开始',{exact:true})).toBeVisible()
    await noOverflow(mobile)
    await mobile.getByRole('button',{name:'我的文件根目录'}).tap()
    await expect(mobile.getByRole('heading',{name:'我的文件',exact:true})).toBeVisible()
    await mobile.emulateMedia({reducedMotion:'reduce'})
    await mobile.reload()
    await expect(mobile.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle')
    await mobile.locator('.file-card').filter({hasText:'旅途与风景'}).first().tap()
    await expect(mobile.getByRole('heading',{name:'旅途与风景',exact:true})).toBeVisible()
    await expect(mobile.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle')
    expect(await mobile.locator('.file-results').evaluate(el=>({animations:el.getAnimations().length,transform:getComputedStyle(el).transform}))).toEqual({animations:0,transform:'none'})
    await noOverflow(mobile)
  } finally { await context.close() }
})


test('imports remain in the chosen folder with durable live progress and no duplicate results', async ({page})=>{
  const errors:string[]=[];page.on('pageerror',e=>errors.push(e.message))
  await login(page)
  await page.locator('.file-card').filter({hasText:'设计灵感'}).first().dblclick()
  await expect(page.getByRole('heading',{name:'设计灵感',exact:true})).toBeVisible()
  // The sidebar action must inherit the open folder just like the toolbar.
  await page.locator('.sidebar').getByRole('button',{name:'添加资源',exact:true}).click()
  await page.getByLabel('资源链接').fill('magnet:?xt=urn:btih:'+'c'.repeat(40)+'&dn=E2E-InPlace-'+Date.now())
  const response=page.waitForResponse(r=>r.url().endsWith('/api/v1/imports')&&r.request().method()==='POST')
  await page.getByRole('button',{name:'开始保存',exact:true}).click()
  const {jobs}=await (await response).json();const job=jobs[0].id
  const card=page.locator(`[data-testid=transfer-card][data-job-id="${job}"]`)
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page).toHaveURL(/folder=folder-2/)
  await expect(card).toBeVisible();await expect(card).toContainText('传输中')
  await expect(card.getByRole('progressbar')).toHaveAttribute('aria-valuenow','43')
  await expect(card.locator('.file-title')).toHaveText('Collection')
  await expect(page.locator('.item-count')).toHaveText('全部内容 1')
  await expect(card.locator('.file-check,.cover-play')).toHaveCount(0)
  expect(await card.getAttribute('draggable')).not.toBe('true')
  await page.getByRole('button',{name:'选择本页全部文件',exact:true}).click()
  await expect(page.locator('.selected-count')).toHaveCount(0)
  await expect.poll(async()=> (await (await page.request.get('/api/v1/files?transfers=0&parent=folder-2')).json()).total).toBe(0)
  await page.reload();await expect(card).toContainText('43%')
  for(const width of [1440,768,390,360]){
    await page.setViewportSize({width,height:900})
    for(const mode of ['列表视图','网格视图']){
      await page.getByRole('button',{name:mode,exact:true}).click()
      await expect(card.locator('.transfer-label')).toBeVisible();await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle');await noOverflow(page)
      if(width===390)await page.screenshot({path:`../artifacts/transfer-${mode}-mobile.png`})
    }
  }
  await expect.poll(async()=> (await page.request.post(`/__fixture/transfer/${job}/fail`)).status()).toBe(200)
  await expect(card).toContainText('传输失败')
  await card.click();await expect(page.getByRole('dialog')).toContainText('模拟来源暂时不可用，请重试')
  await expect(page).toHaveURL(/folder=folder-2/)
  await page.getByLabel('关闭',{exact:true}).click()
  await card.getByRole('button',{name:'重试',exact:true}).click()
  await expect(card).toContainText('传输中')
  await expect.poll(async()=> (await page.request.post(`/__fixture/transfer/${job}/complete`)).status()).toBe(200)
  await expect(card).toHaveCount(0,{timeout:20000})
  await expect(page).toHaveURL(/folder=folder-2/)
  await expect(page.locator('.file-card')).toHaveCount(1)
  await expect(page.locator('.file-title')).toHaveText('Collection')
  await page.locator('.file-card').dblclick()
  await expect(page.getByRole('heading',{name:'Collection',exact:true})).toBeVisible()
  await expect(page.locator('.file-title').filter({hasText:'one.mp4'})).toBeVisible()
  expect(errors).toEqual([])
})

test('breadcrumbs fit available space, expand again and keep every ancestor navigable',async({page})=>{
  await login(page)
  const paths:Record<string,string[]>={short:['Hero','VR'],deep:['资料库','个人收藏','影像世界','全景影像','精选合集','旅行记录','2026年','VR'],long:['Hero','这是一个特别长的当前文件夹名称'.repeat(12)]}
  await page.route('**/api/v1/files?*',async route=>{
    const parent=new URL(route.request().url()).searchParams.get('parent')||''
    const match=/^crumb-(short|deep|long)-(\d+)$/.exec(parent)
    if(!match){await route.continue();return}
    const [,kind,index]=match
    await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify({files:[],total:0,page:0,limit:100,transferring:0,breadcrumbs:paths[kind].slice(0,Number(index)+1).map((name,i)=>({id:`crumb-${kind}-${i}`,name}))})})
  })
  await page.goto('/files?folder=crumb-short-1')
  const path=page.getByRole('navigation',{name:'文件路径'})
  await expect(path.getByRole('button',{name:'Hero',exact:true})).toBeVisible()
  await expect(path.getByRole('button',{name:'展开上级路径'})).toHaveCount(0)
  await path.getByRole('button',{name:'Hero',exact:true}).click()
  await expect(page.getByRole('heading',{name:'Hero',exact:true})).toBeVisible()
  await expect(page).toHaveURL(/folder=crumb-short-0/)
  await page.goBack()
  await expect(page.getByRole('heading',{name:'VR',exact:true})).toBeVisible()
  await page.screenshot({path:'../artifacts/breadcrumb-short-desktop.png'})
  await page.goto('/files?folder=crumb-deep-7')
  for(const width of [2200,1440,1024,768,390,360,2200]){
    await page.setViewportSize({width,height:900})
    await page.evaluate(()=>document.fonts.ready)
    await expect.poll(async()=>path.evaluate(el=>el.scrollWidth<=el.clientWidth+1)).toBe(true)
    if(width===2200){
      await expect(path.getByRole('button',{name:'展开上级路径'})).toHaveCount(0)
      await expect(path.locator('button.breadcrumb-ancestor')).toHaveCount(7)
    }else if(width<=390){await expect(path.getByRole('button',{name:'展开上级路径'})).toBeVisible()}
    await noOverflow(page)
    if(width===390||width===2200)await page.screenshot({path:`../artifacts/breadcrumb-deep-${width}.png`})
  }
  await page.setViewportSize({width:390,height:900})
  await path.getByRole('button',{name:'展开上级路径'}).click()
  await expect(page.getByRole('menuitem',{name:'资料库',exact:true})).toBeVisible()
  await page.getByRole('menuitem',{name:'资料库',exact:true}).click()
  await expect(page).toHaveURL(/folder=crumb-deep-0/)
  await expect(page.getByRole('heading',{name:'资料库',exact:true})).toBeVisible()
  await expect(path.getByRole('button',{name:'展开上级路径'})).toHaveCount(0)
  await page.goto('/files?folder=crumb-long-1')
  await expect(page.getByRole('heading',{name:paths.long[1],exact:true})).toBeVisible()
  await expect.poll(async()=>path.evaluate(el=>el.scrollWidth<=el.clientWidth+1)).toBe(true)
  await noOverflow(page)
  await expect(path.getByRole('button',{name:'展开上级路径'})).toBeVisible()
  const icon=await page.request.get('/favicon.svg');expect(icon.ok()).toBe(true);expect(icon.headers()['content-type']).toContain('image/svg+xml')
  const fallback=await page.request.get('/favicon.ico');expect(fallback.ok()).toBe(true);expect((await fallback.body()).subarray(0,4).toString('hex')).toBe('00000100')
})

test('folder video previews are optional, persistent and fall back cleanly',async({page})=>{
  await login(page)
  let previewRequests=0
  page.on('request',request=>{const url=new URL(request.url());if(url.pathname.endsWith('/thumbnail')&&url.searchParams.has('folder'))previewRequests++})
  await expect(page.locator('.folder-preview')).toHaveCount(0)
  await page.goto('/settings')
  const toggle=page.getByRole('switch',{name:'文件夹视频预览',exact:true})
  await expect(toggle).toHaveAttribute('aria-checked','false')
  await toggle.focus();await toggle.press('Space')
  await expect(toggle).toHaveAttribute('aria-checked','true')
  await page.getByRole('button',{name:'保存设置',exact:true}).click()
  await expect.poll(async()=> (await (await page.request.get('/api/v1/settings')).json()).folder_previews).toBe(true)
  await page.reload();await expect(toggle).toHaveAttribute('aria-checked','true')
  await page.setViewportSize({width:390,height:900});await toggle.scrollIntoViewIfNeeded();await noOverflow(page)
  await page.screenshot({path:'../artifacts/folder-preview-settings-mobile.png'})
  await page.goto('/files')
  const card=page.locator('.file-card').filter({hasText:'电影时光'}).first()
  await expect(card.locator('.folder-preview.is-ready')).toBeVisible()
  await expect.poll(async()=>card.locator('.folder-preview img').evaluateAll(images=>images.filter(img=>(img as HTMLImageElement).naturalWidth>0).length)).toBe(3)
  expect(previewRequests).toBeGreaterThan(0)
  await expect(page.locator('.file-card').filter({hasText:'旅途与风景'}).first().locator('.folder-preview')).toHaveCount(0)
  await expect(page.locator('.file-card').filter({hasText:'学习与探索'}).first().locator('.folder-preview-fallback .file-icon')).toBeVisible()
  for(const width of [1440,768,390]){
    await page.setViewportSize({width,height:900});await page.waitForTimeout(160);await expect(card.locator('.folder-preview.is-ready')).toBeVisible();await noOverflow(page)
    await page.screenshot({path:`../artifacts/folder-preview-${width}.png`})
  }
  await page.getByRole('button',{name:'列表视图',exact:true}).click();await expect(page.locator('.folder-preview')).toHaveCount(0);await noOverflow(page)
  await page.getByRole('button',{name:'网格视图',exact:true}).click()
  await page.route('**/api/v1/files/*/thumbnail?*',async route=>{
    if(new URL(route.request().url()).searchParams.has('folder'))await route.fulfill({status:503,body:'temporary image failure'})
    else await route.continue()
  })
  await page.reload();await expect(card.locator('.folder-preview img')).toHaveCount(0)
  await expect(card.locator('.folder-preview-fallback .file-icon')).toBeVisible()
  await expect(card.locator('.folder-preview.is-ready')).toHaveCount(0)
  await page.goto('/settings');await toggle.click();await page.getByRole('button',{name:'保存设置',exact:true}).click()
  await expect.poll(async()=> (await (await page.request.get('/api/v1/settings')).json()).folder_previews).toBe(false)
  const before=previewRequests
  await page.goto('/files');await expect(card).toBeVisible();await expect(page.locator('.folder-preview')).toHaveCount(0)
  expect(previewRequests).toBe(before)
})

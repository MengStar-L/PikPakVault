import { test, expect } from './fixtures'

test('TelDrive first upload stays in transfers and cannot create duplicate recovery', async ({page})=>{
  const seeded=await page.request.post('/__fixture/teldrive-pending')
  expect(seeded.ok()).toBe(true)
  const {id,job_id}=await seeded.json()
  await page.goto('/files')
  const folder=page.locator('.file-card').filter({hasText:'TelDrive 同步目录'})
  await expect(folder).toContainText('已暂停')
  await folder.dblclick()
  await expect(page.getByRole('heading',{name:'TelDrive 同步目录',exact:true})).toBeVisible()
  const card=page.locator(`[data-job-id="${job_id}"]`)
  await expect(card).toContainText('TelDrive 首次上传.mp4')
  await expect(card).toContainText('传输失败')
  await expect(card.getByRole('button',{name:'重试',exact:true})).toBeVisible()
  await card.click()
  await expect(page.getByRole('dialog')).toContainText('读取 TelDrive 文件时连接提前断开')
  await page.getByRole('dialog').locator('.modal-actions').getByRole('button',{name:'关闭',exact:true}).click()
  await page.goto('/recovery')
  await expect(page.getByRole('heading',{name:'恢复中心',exact:true})).toBeVisible()
  await expect(page.locator('.recovery-item').filter({hasText:'TelDrive 首次上传.mp4'})).toHaveCount(0)
  await page.getByRole('button',{name:'恢复全部缺失'}).click()
  await expect(page.getByRole('dialog').getByRole('status')).toContainText('已有传输或恢复任务')
  await expect(page.getByRole('dialog').locator('.recovery-preview-list')).not.toContainText('TelDrive 首次上传.mp4')
  for(const width of [1440,768,390]){
    await page.setViewportSize({width,height:900})
    expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
    expect(await page.getByRole('dialog').evaluate(el=>el.scrollWidth<=el.clientWidth+1)).toBe(true)
  }
  await page.getByRole('dialog').getByRole('button',{name:'取消',exact:true}).click()
  // Only explicit cancellation releases the node to the recovery centre.
  await page.goto('/tasks')
  await page.getByRole('button',{name:'取消 上传 · TelDrive 首次上传.mp4',exact:true}).click()
  await page.getByRole('dialog').getByRole('button',{name:'取消任务',exact:true}).click()
  await page.goto('/recovery')
  const row=page.locator('.recovery-item').filter({hasText:'TelDrive 首次上传.mp4'})
  await expect(row).toHaveCount(1)
  await row.getByRole('button',{name:'核对恢复'}).click()
  await expect(page.getByRole('dialog').locator('.recovery-preview-list')).toContainText('TelDrive 首次上传.mp4')
  await expect(page.getByRole('dialog').getByRole('button',{name:'确认开始恢复'})).toBeEnabled()
  const missing=await page.request.get(`/api/v1/files?view=missing&search=${encodeURIComponent('TelDrive 首次上传')}`)
  expect(missing.ok()).toBe(true)
  expect((await missing.json()).files.map((file:{id:string})=>file.id)).toEqual([id])
})

test('TelDrive folder selection, manual and automatic sync preserve the page', async ({page})=>{
  let monitors:Record<string,unknown>[]=[]
  let syncs=0
  const longName='电影与音乐收藏_很长的来源目录名称_'.repeat(6)
  await page.route('**/api/v1/teldrive',async route=>{
    if(route.request().method()==='GET')return route.fulfill({json:{monitors,active_account:'a'}})
    const body=route.request().postDataJSON()
    expect(body.auto_minutes).toBe(0);expect(body.folder_id).toBe('source');expect(body.parent_id).toBe('root');expect(body.token).toBe('fixture-only-token')
    monitors=[{...body,token:undefined,id:'monitor-1',target_path:'我的文件',last_run:0,last_job:null}]
    return route.fulfill({json:{id:'monitor-1'}})
  })
  await page.route('**/api/v1/teldrive/browse',route=>{
    const body=route.request().postDataJSON()
    return route.fulfill({json:{items:body.folder_id?[]:[{id:'source',name:longName,type:'folder',size:0},{id:'video',name:'video.mp4',type:'file',size:123456}],meta:{count:body.folder_id?0:2,currentPage:1,totalPages:1}}})
  })
  await page.route('**/api/v1/teldrive/monitor-1/sync',route=>{syncs++;monitors[0]={...monitors[0],last_job:{id:'scan',state:'completed',message:'已扫描 12 项，新增 2 个上传任务',title:'扫描',kind:'teldrive_scan'}};return route.fulfill({status:202,json:{id:'scan'}})})
  await page.route('**/api/v1/teldrive/monitor-1',route=>{
    const body=route.request().postDataJSON();expect(body.token).toBe('');expect(body.auto_minutes).toBe(15);monitors[0]={...monitors[0],auto_minutes:15};return route.fulfill({json:{id:'monitor-1'}})
  })
  await page.goto('/teldrive');await expect(page.getByRole('heading',{name:'TelDrive 同步',exact:true})).toBeVisible()
  await page.getByRole('button',{name:'添加监控'}).click()
  const dialog=page.getByRole('dialog')
  await expect(dialog.getByRole('button',{name:'保存监控'})).toBeDisabled()
  await dialog.getByLabel('TelDrive 站点地址').fill('https://td.example.test')
  await dialog.getByLabel('TelDrive access_token').fill('fixture-only-token')
  await dialog.getByRole('button',{name:'连接并选择文件夹'}).click()
  await dialog.getByRole('button',{name:longName,exact:true}).click()
  await dialog.getByRole('button',{name:'选择当前文件夹'}).click()
  await expect(dialog.getByLabel('同步方式')).toHaveValue('0')
  for(const width of [1440,768,390,360]){
    await page.setViewportSize({width,height:900})
    expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
    expect(await dialog.evaluate(el=>el.scrollWidth<=el.clientWidth+1)).toBe(true)
    const box=await dialog.boundingBox();expect(box!.x+box!.width).toBeLessThanOrEqual(width+1)
  }
  await page.screenshot({path:'../artifacts/teldrive-mobile-dialog.png'})
  await dialog.getByRole('button',{name:'保存监控'}).click();await expect(dialog).toHaveCount(0)
  await expect(page.locator('.td-card')).toHaveCount(1)
  for(const width of [1440,768,390,360]){
    await page.setViewportSize({width,height:900});expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
    expect(await page.locator('.td-card').evaluate(el=>el.scrollWidth<=el.clientWidth+1)).toBe(true)
  }
  await page.getByRole('button',{name:'立即同步'}).click();expect(syncs).toBe(1);await expect(page).toHaveURL(/\/teldrive$/)
  await expect(page.getByText('已扫描 12 项，新增 2 个上传任务')).toBeVisible()
  await page.getByRole('button',{name:'配置',exact:true}).click();await expect(dialog.getByLabel('TelDrive access_token')).toHaveValue('');await dialog.getByLabel('同步方式').selectOption('15');await dialog.getByRole('button',{name:'保存监控'}).click()
  await expect(page.getByText('每 15 分钟',{exact:true})).toBeVisible()
  await page.setViewportSize({width:1440,height:960});await page.screenshot({path:'../artifacts/teldrive-desktop.png'})
})

test('TelDrive connection errors and reduced motion stay usable',async({page})=>{
  await page.emulateMedia({reducedMotion:'reduce'})
  await page.route('**/api/v1/teldrive',r=>r.fulfill({json:{monitors:[],active_account:'a'}}))
  await page.route('**/api/v1/teldrive/browse',r=>r.fulfill({status:502,json:{error:'TelDrive GET /files：HTTP 401，认证失效，请更新 TelDrive access_token'}}))
  await page.goto('/teldrive');await page.getByRole('button',{name:'添加监控'}).click();await page.setViewportSize({width:390,height:844})
  await page.getByLabel('TelDrive 站点地址').fill('https://td.example.test');await page.getByLabel('TelDrive access_token').fill('fixture-only-token');await page.getByRole('button',{name:'连接并选择文件夹'}).click()
  await expect(page.getByRole('dialog').getByRole('alert')).toContainText('HTTP 401');await expect(page.getByRole('button',{name:'保存监控'})).toBeDisabled()
  expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
})

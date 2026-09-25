import { test, expect } from './fixtures'
import type { Locator, Page } from '@playwright/test'

const audioName='Sunday Morning — 慢生活歌单.wav'
const longName='这是一个需要优雅处理的很长文件名'.repeat(12)+'.mp4'

function cardFor(page:Page,name:string) {
  return page.locator('.file-card').filter({has:page.getByText(name,{exact:true})})
}

async function file(page:Page,id:string) {
  const response=await page.request.get(`/api/v1/files/${id}`)
  expect(response.ok()).toBe(true)
  return (await response.json()).file as {played_at?:number;favorite:boolean;position:number}
}

async function badgeFits(page:Page,card:Locator) {
  const badge=card.locator('.played-badge')
  await expect(badge).toHaveText('已播放')
  await expect(badge).toBeInViewport()
  const layout=await card.evaluate(element=>{
    const box=element.getBoundingClientRect()
    const badge=element.querySelector('.played-badge')!
    const mark=badge.getBoundingClientRect()
    return {
      documentFits:document.documentElement.scrollWidth<=innerWidth,
      cardFits:element.scrollWidth<=element.clientWidth+1,
      badgeFits:badge.scrollWidth<=badge.clientWidth+1,
      badgeInside:mark.left>=box.left&&mark.right<=box.right&&mark.top>=box.top&&mark.bottom<=box.bottom,
    }
  })
  expect(layout).toEqual({documentFits:true,cardFits:true,badgeFits:true,badgeInside:true})
}

test('preview alone leaves history unchanged; real web playback persists in cards and details',async({page})=>{
  let playedRequests=0
  page.on('request',request=>{
    if(request.method()==='POST'&&new URL(request.url()).pathname==='/api/v1/files/audio/played')playedRequests++
  })
  const original=(await file(page,'audio')).played_at
  await page.goto('/files')
  // Recovery uses a regular FilesResult cache beside the infinite file lists.
  await page.getByRole('link',{name:/恢复中心/}).click()
  await expect(page.getByRole('heading',{name:'恢复中心',exact:true})).toBeVisible()
  await expect(page.getByText('目前没有待处理的资源',{exact:true})).toBeVisible()
  await page.getByRole('navigation',{name:'主导航'}).getByRole('link',{name:/我的文件/}).click()
  await page.getByRole('textbox',{name:'搜索文件'}).fill('Sunday Morning')
  const card=cardFor(page,audioName)
  await card.dblclick()
  await page.getByRole('button',{name:'服务器中转',exact:true}).click()
  const audio=page.locator('audio')
  await expect.poll(()=>audio.evaluate(el=>(el as HTMLAudioElement).readyState)).toBeGreaterThanOrEqual(2)
  expect(await audio.evaluate(el=>(el as HTMLAudioElement).paused)).toBe(true)
  expect(playedRequests).toBe(0)
  expect((await file(page,'audio')).played_at).toBe(original)
  await page.getByRole('button',{name:'关闭',exact:true}).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(playedRequests).toBe(0)

  await card.dblclick()
  await page.getByRole('button',{name:'服务器中转',exact:true}).click()
  await expect.poll(()=>audio.evaluate(el=>(el as HTMLAudioElement).readyState)).toBeGreaterThanOrEqual(2)
  // Exercise native media playback, not a synthetic playing event.
  await audio.evaluate(async el=>{const media=el as HTMLAudioElement;media.muted=true;media.loop=true;await media.play()})
  await expect.poll(()=>audio.evaluate(el=>(el as HTMLAudioElement).currentTime)).toBeGreaterThan(0)
  await expect.poll(()=>playedRequests).toBe(1)
  await expect.poll(async()=>(await file(page,'audio')).played_at||0).toBeGreaterThan(0)
  await audio.evaluate(el=>(el as HTMLAudioElement).pause())
  await audio.evaluate(el=>(el as HTMLAudioElement).play())
  await expect.poll(()=>audio.evaluate(el=>(el as HTMLAudioElement).paused)).toBe(false)
  await page.getByRole('button',{name:'关闭',exact:true}).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(playedRequests).toBe(1)
  await expect(card.locator('.played-badge')).toHaveText('已播放')
  await expect(page.getByText('播放记录保存失败',{exact:true})).toHaveCount(0)

  await page.reload()
  await page.getByRole('textbox',{name:'搜索文件'}).fill('Sunday Morning')
  await expect(card.locator('.played-badge')).toHaveText('已播放')
  await card.getByRole('button',{name:`${audioName} 的更多操作`}).click()
  await page.getByRole('menuitem',{name:'详情与来源',exact:true}).click()
  const playedAt=(await file(page,'audio')).played_at!
  const expectedDate=await page.evaluate(value=>new Date(value*1000).toLocaleDateString('zh-CN',{month:'2-digit',day:'2-digit',year:'numeric'}),playedAt)
  await expect(page.getByRole('dialog').getByText(`已播放 · ${expectedDate}`,{exact:true})).toBeVisible()
})

test('played badge survives reload and fits long favorite names in desktop and phone grid/list',async({page})=>{
  // Intercept only external protocol activation; real history writes and reads
  // still use the fixture database, including the reload below.
  await page.addInitScript(()=>{
    document.addEventListener('click',event=>{
      const link=event.target instanceof Element?event.target.closest('a'):null
      if(link?.href.startsWith('potplayer://'))event.preventDefault()
    },true)
  })
  await page.route('**/api/v1/files/long/media',route=>route.fulfill({json:{file:{id:'long'},options:[{id:'',label:'原始文件',url:'https://media.example.test/long.mp4?token=played-layout'}],proxy_default:false}}))
  await page.goto('/files')
  await page.getByRole('textbox',{name:'搜索文件'}).fill('这是一个需要优雅处理')
  const card=cardFor(page,longName)
  if(!(await file(page,'long')).favorite){
    await card.getByRole('button',{name:`${longName} 的更多操作`}).click()
    await page.getByRole('menuitem',{name:'收藏',exact:true}).click()
  }
  await card.click({button:'right'})
  await page.getByRole('menuitem',{name:'使用 PotPlayer 播放',exact:true}).click()
  await expect.poll(async()=>(await file(page,'long')).played_at||0).toBeGreaterThan(0)
  await expect(card.locator('.played-badge')).toHaveText('已播放')
  await page.reload()
  await page.getByRole('textbox',{name:'搜索文件'}).fill('这是一个需要优雅处理')
  await expect(page.locator('.file-card')).toHaveCount(1)

  for(const width of [1440,360]){
    await page.setViewportSize({width,height:900})
    for(const mode of ['网格视图','列表视图']){
      await page.getByRole('button',{name:mode,exact:true}).click()
      await expect(card).toHaveClass(mode==='列表视图'?/\blist\b/:/\bgrid\b/)
      await expect(page.locator('.files-viewport')).toHaveAttribute('data-motion-phase','idle')
      await card.scrollIntoViewIfNeeded()
      await badgeFits(page,card)
      await expect(card.locator('.file-title')).toHaveAttribute('title',longName)
      await expect(card.locator('.favorite-star')).toHaveCount(1)
      if(mode==='列表视图')await expect(card.locator('.status.present')).toHaveText('已保存')
      if(width===1440)await expect(card.locator('.favorite-star')).toBeVisible()
      await page.screenshot({path:`../artifacts/played-${width}-${mode==='列表视图'?'list':'grid'}.png`,animations:'disabled'})
    }
  }
})

test('failed history write offers a retry without relaunching or losing the saved position',async({page})=>{
  const name='City Lights — 城市的另一面.mp4'
  const {csrf}=await (await page.request.get('/api/v1/auth/status')).json()
  const saved=await page.request.patch('/api/v1/files/night/position',{
    data:{position:42.75},headers:{'X-CSRF-Token':csrf},
  })
  expect(saved.ok()).toBe(true)
  expect((await file(page,'night')).played_at||0).toBe(0)
  await page.addInitScript(()=>{
    document.addEventListener('click',event=>{
      const link=event.target instanceof Element?event.target.closest('a'):null
      if(link?.href.startsWith('potplayer://')){
        event.preventDefault()
        document.documentElement.dataset.playerLaunchCount=String(Number(document.documentElement.dataset.playerLaunchCount||0)+1)
      }
    },true)
  })
  let lookups=0
  let writes=0
  await page.route('**/api/v1/files/night/media',route=>{
    lookups++
    return route.fulfill({json:{file:{id:'night'},options:[{id:'',label:'原始文件',url:'https://media.example.test/night.mp4?token=retry-history'}],proxy_default:false}})
  })
  await page.route('**/api/v1/files/night/played',async route=>{
    writes++
    if(writes===1){await route.fulfill({status:503,json:{error:'Temporary history write failure'}});return}
    await route.continue()
  })
  await page.goto('/files')
  const card=cardFor(page,name)
  await expect(card.locator('.played-badge')).toHaveCount(0)
  await card.click({button:'right'})
  await page.getByRole('menuitem',{name:'使用 PotPlayer 播放',exact:true}).click()
  await expect(page.getByText('播放记录保存失败',{exact:true})).toBeVisible()
  expect(writes).toBe(1)
  await expect(card.locator('.played-badge')).toHaveCount(0)
  expect((await file(page,'night')).played_at||0).toBe(0)
  expect((await file(page,'night')).position).toBe(42.75)
  await page.getByRole('button',{name:'重试保存',exact:true}).click()
  await expect(card.locator('.played-badge')).toHaveText('已播放')
  await expect.poll(async()=>(await file(page,'night')).played_at||0).toBeGreaterThan(0)
  expect((await file(page,'night')).position).toBe(42.75)
  expect(writes).toBe(2)
  expect(lookups).toBe(1)
  expect(await page.evaluate(()=>Number(document.documentElement.dataset.playerLaunchCount))).toBe(1)
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page).toHaveURL(/\/files$/)
  await page.reload()
  await expect(card.locator('.played-badge')).toHaveText('已播放')
  expect((await file(page,'night')).position).toBe(42.75)
})

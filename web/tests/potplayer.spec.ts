import { test, expect } from './fixtures'
import type { Page } from '@playwright/test'

const name='海岸线之旅 · 一场关于自由的电影.mp4'
const signed='https://media.example.test/movie.mp4?token=a%2Fb%2Bc&expires=9999999999&name=%E7%94%B5%E5%BD%B1'

async function launches(page:Page) {
  return page.evaluate(()=>JSON.parse(document.documentElement.dataset.playerLaunches||'[]') as string[])
}
test.beforeEach(async({page})=>{
  // Observe the real DOM activation and prevent opening a native app during CI.
  await page.addInitScript(()=>{
    document.addEventListener('click',event=>{
      const target=event.target
      const link=target instanceof Element?target.closest('a'):null
      if(link?.href.startsWith('potplayer://')){
        event.preventDefault()
        const values=JSON.parse(document.documentElement.dataset.playerLaunches||'[]')
        values.push(link.getAttribute('href'))
        document.documentElement.dataset.playerLaunches=JSON.stringify(values)
      }
    },true)
  })
  await page.goto('/files')
  await expect(page.getByRole('heading',{name:'我的文件',exact:true})).toBeVisible()
})

test('video menus launch a fresh signed direct URL and keep the current folder',async({page})=>{
  let lookups=0
  await page.route('**/api/v1/files/coast/media',async route=>{
    lookups++
    await new Promise(resolve=>setTimeout(resolve,250))
    await route.fulfill({json:{file:{id:'coast'},options:[{id:'',label:'原始文件',url:signed}],proxy_default:true}})
  })
  const card=page.locator('.file-card').filter({has:page.getByText(name,{exact:true})})
  await card.getByRole('button',{name:`${name} 的更多操作`}).click()
  await page.getByRole('menuitem',{name:'使用 PotPlayer 播放',exact:true}).click()
  await expect.poll(()=>launches(page)).toEqual([`potplayer://${signed}`])
  await expect(page).toHaveURL(/\/files$/)
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page.getByText('已请求打开 PotPlayer',{exact:true})).toBeVisible()
  await page.getByRole('button',{name:'再次打开',exact:true}).click()
  expect(await launches(page)).toHaveLength(2)
  expect(lookups).toBe(1)
  await card.click({button:'right'})
  await page.getByRole('menuitem',{name:'使用 PotPlayer 播放',exact:true}).click()
  await expect.poll(()=>launches(page)).toHaveLength(3)
  expect(lookups).toBe(2)
  await page.getByRole('button',{name:'旅途与风景 的更多操作',exact:true}).click()
  await expect(page.getByRole('menuitem',{name:'使用 PotPlayer 播放',exact:true})).toHaveCount(0)
})

test('viewer preserves quality, pauses web playback and fits desktop and phone',async({page})=>{
  // Use the fixture's real playable media to check pause state, without
  // depending on platform-specific video encoders in CI.
  const clip=await (await page.request.get('/api/v1/files/audio/content?proxy=1')).body()
  let lookups=0
  await page.route('**/api/v1/files/coast/media',async route=>{
    lookups++
    await route.fulfill({json:{file:{id:'coast',mime:'audio/wav'},options:[{id:'',label:'原始文件',url:signed+`&lookup=${lookups}`},{id:'hd',label:'1080P',url:signed+`&quality=hd&lookup=${lookups}`}],proxy_default:false}})
  })
  await page.route('https://media.example.test/**',route=>route.fulfill({body:clip,contentType:'audio/wav'}))
  await page.route('**/api/v1/files/coast/content?*',route=>route.fulfill({body:clip,contentType:'audio/wav'}))
  await page.locator('.file-card').filter({has:page.getByText(name,{exact:true})}).dblclick()
  await page.getByLabel('播放清晰度').selectOption('hd')
  await page.getByRole('button',{name:'服务器中转',exact:true}).click()
  const button=page.getByRole('button',{name:'使用 PotPlayer 播放',exact:true})
  for(const width of [1440,768,390,360]){
    await page.setViewportSize({width,height:900})
    await expect(button).toBeVisible()
    expect(await page.locator('.media-toolbar').evaluate(el=>el.scrollWidth<=el.clientWidth)).toBe(true)
    expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
    if(width!==360)await page.screenshot({path:`../artifacts/potplayer-${width}.png`,animations:'disabled'})
  }
  await expect.poll(()=>page.locator('audio').evaluate(el=>(el as HTMLAudioElement).readyState)).toBeGreaterThanOrEqual(2)
  await page.locator('audio').evaluate(async el=>{const video=el as HTMLAudioElement;video.muted=true;video.loop=true;await video.play()})
  await expect.poll(()=>page.locator('audio').evaluate(el=>(el as HTMLAudioElement).paused)).toBe(false)
  await button.click()
  await expect.poll(()=>launches(page)).toHaveLength(1)
  const href=(await launches(page))[0]
  expect(href).toContain('&quality=hd&lookup=')
  expect(href).not.toContain('/api/v1/')
  await expect.poll(()=>page.locator('audio').evaluate(el=>(el as HTMLAudioElement).paused)).toBe(true)
  await expect(page.getByRole('dialog')).toBeVisible()
})

test('failed or unsafe links never launch and closing a pending preview cancels launch',async({page})=>{
  let mode='error'
  await page.route('**/api/v1/files/coast/media',async route=>{
    if(mode==='error'){await route.fulfill({status:409,json:{error:'请先恢复这个视频'}});return}
    if(mode==='slow')await new Promise(resolve=>setTimeout(resolve,800))
    await route.fulfill({json:{file:{id:'coast',mime:'video/mp4'},options:mode==='empty'?[]:[{id:'',label:'原始文件',url:mode==='unsafe'?'file:///C:/video.mp4':signed}],proxy_default:false}})
  })
  const card=page.locator('.file-card').filter({has:page.getByText(name,{exact:true})})
  for(const value of ['error','unsafe','empty']){
    mode=value
    await card.getByRole('button',{name:`${name} 的更多操作`}).click()
    await page.getByRole('menuitem',{name:'使用 PotPlayer 播放',exact:true}).click()
    await expect(page.locator('[data-sonner-toast][data-type=error]').last()).toBeVisible()
    expect(await launches(page)).toEqual([])
  }
  mode='slow'
  await card.dblclick()
  await page.getByRole('button',{name:'使用 PotPlayer 播放',exact:true}).click()
  await expect(page.getByRole('button',{name:'正在获取地址…',exact:true})).toBeDisabled()
  await page.getByRole('button',{name:'关闭',exact:true}).click()
  await page.waitForTimeout(1100)
  expect(await launches(page)).toEqual([])
})

import { useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'
import { api, fileType } from './api'
import type { FileNode, MediaData } from './api'

export function canPlayExternally(file:FileNode) {
  return fileType(file)==='video' && !file.transfer && !file.trashed && !['missing','unbound','pending','conflict','trashed'].includes(file.state)
}

function playerLink(value:string) {
  // Keep the signed query unchanged. Never pass a shell command, local path,
  // site session, or authenticated content endpoint to the external player.
  if (!value || /[\s\u0000-\u001f\u007f"<>]/.test(value)) throw new Error('播放地址无效，请刷新后重试')
  let url:URL
  try { url=new URL(value) } catch { throw new Error('播放地址无效，请刷新后重试') }
  if (url.protocol!=='https:' || url.username || url.password) throw new Error('播放地址不可用，请刷新后重试')
  return `potplayer://${value}`
}

function launch(href:string) {
  const link=document.createElement('a')
  link.href=href
  link.hidden=true
  document.body.appendChild(link)
  link.click()
  link.remove()
}

export function usePotPlayer(scope:string|undefined) {
  const [busy,setBusy]=useState<string|null>(null)
  const request=useRef<symbol|null>(null)
  const notice=useRef<string|number|null>(null)
  useEffect(()=>{
    setBusy(null)
    return ()=>{
      request.current=null
      if(notice.current!==null)toast.dismiss(notice.current)
      notice.current=null
    }
  },[scope])

  async function play(file:FileNode,quality='',onLaunch?:()=>void) {
    if(request.current)return
    if(!canPlayExternally(file)){toast.error('请先恢复这个视频，再使用 PotPlayer 播放');return}
    const ticket=Symbol()
    request.current=ticket
    setBusy(file.id)
    if(notice.current!==null)toast.dismiss(notice.current)
    const id=toast.loading('正在获取 PotPlayer 播放地址…')
    notice.current=id
    try {
      const data=await api<MediaData>(`/files/${encodeURIComponent(file.id)}/media`)
      if(request.current!==ticket)return
      if(data.file.id!==file.id)throw new Error('视频信息已变化，请刷新后重试')
      const option=data.options.find(o=>o.id===quality)||(quality===''?data.options[0]:undefined)
      if(!option?.url)throw new Error(quality?'此清晰度暂时不可用，请重新选择':'视频暂时没有可用播放地址，请先恢复资源或稍后重试')
      const href=playerLink(option.url)
      const prepared=Date.now()
      const open=()=>{
        if(Date.now()-prepared>60000){void play(file,quality,onLaunch);return}
        launch(href)
        onLaunch?.()
      }
      open()
      // Browsers cannot report whether the native handler is installed. A
      // second explicit click also handles a slow lookup losing user activation.
      toast.info('已请求打开 PotPlayer',{
        id,duration:15000,
        description:'PotPlayer 将直连 PikPak。请允许浏览器打开应用；未启动时，确认本机已安装 PotPlayer 后再次打开。',
        action:{label:'再次打开',onClick:open},
      })
    } catch(error) {
      if(request.current===ticket)toast.error(error instanceof Error?error.message:'无法获取播放地址，请稍后重试',{id})
    } finally {
      if(request.current===ticket){request.current=null;setBusy(null)}
    }
  }
  return {play,busy}
}

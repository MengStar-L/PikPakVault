import { Fragment, useLayoutEffect, useRef, useState } from 'react'
import type { DragEvent, RefObject } from 'react'
import * as Dropdown from '@radix-ui/react-dropdown-menu'
import { motion } from 'motion/react'
import { ChevronRight, Ellipsis, Folder, House } from 'lucide-react'
import type { FilesResult } from './api'

type Props = {
  crumbs: FilesResult['breadcrumbs']
  parent: string
  title: string
  reduce: boolean
  headingRef: RefObject<HTMLHeadingElement | null>
  onNavigate: (id:string,name?:string) => void
  onDrop: (event:DragEvent,target:string) => void
}

export function FileBreadcrumb({crumbs,parent,title,reduce,headingRef,onNavigate,onDrop}:Props){
  const nav=useRef<HTMLElement>(null)
  const probe=useRef<HTMLDivElement>(null)
  const [fit,setFit]=useState({hidden:0,iconOnly:false})
  const ancestors=crumbs.slice(0,-1)
  // Names can change in place after a background refresh; measure those too.
  const signature=JSON.stringify([crumbs,title])
  useLayoutEffect(()=>{
    const container=nav.current, ruler=probe.current
    if(!container||!ruler)return
    let disposed=false
    function measure(){
      if(disposed||!container||!ruler)return
      const width=container.clientWidth
      if(!width)return
      const size=(selector:string)=>ruler.querySelector(selector)!.getBoundingClientRect().width
      const gap=parseFloat(getComputedStyle(container).columnGap)||0
      const separator=size('.path-separator')+gap*2
      const root=size('.breadcrumb-home')
      const heading=size('.breadcrumb-title')
      const overflow=size('.breadcrumb-overflow')+separator
      const parts=Array.from(ruler.querySelectorAll('.breadcrumb-ancestor'),el=>el.getBoundingClientRect().width+separator)
      let needed=root+heading+(crumbs.length?separator:gap)+parts.reduce((sum,w)=>sum+w,0)
      let hidden=0
      if(needed>width&&parts.length){
        needed+=overflow
        while(hidden<parts.length&&needed>width)needed-=parts[hidden++]
      }
      // Keep the current folder readable when even root + menu + title is long.
      // The home label is retained on small screens whenever it still fits.
      const iconOnly=!!crumbs.length&&needed>width
      setFit(previous=>previous.hidden===hidden&&previous.iconOnly===iconOnly?previous:{hidden,iconOnly})
    }
    measure()
    const observer=new ResizeObserver(measure)
    observer.observe(container)
    observer.observe(ruler)
    document.fonts.ready.then(measure)
    document.fonts.addEventListener('loadingdone',measure)
    return()=>{disposed=true;observer.disconnect();document.fonts.removeEventListener('loadingdone',measure)}
  },[signature])

  const hidden=ancestors.slice(0,fit.hidden)
  const separator=<ChevronRight className="path-separator" size={13} aria-hidden="true"/>
  return <nav ref={nav} className="breadcrumb file-path" aria-label="文件路径" title={['我的文件',...crumbs.map(c=>c.name)].join(' / ')}>
    <button className={`breadcrumb-home ${fit.iconOnly?'icon-only':''}`} onClick={()=>onNavigate('root')} onDragOver={e=>e.preventDefault()} onDrop={e=>onDrop(e,'root')} aria-label="我的文件根目录"><House size={17}/>{crumbs.length>0&&<span>我的文件</span>}</button>
    {hidden.length>0&&<>{separator}<Dropdown.Root><Dropdown.Trigger className="breadcrumb-overflow" aria-label="展开上级路径"><Ellipsis size={18}/></Dropdown.Trigger><Dropdown.Portal><Dropdown.Content className="dropdown-content breadcrumb-menu" sideOffset={8} align="start">{hidden.map(c=><Dropdown.Item key={c.id} onSelect={()=>onNavigate(c.id,c.name)} title={c.name}><Folder size={15}/><span>{c.name}</span></Dropdown.Item>)}</Dropdown.Content></Dropdown.Portal></Dropdown.Root></>}
    {ancestors.slice(fit.hidden).map(c=><Fragment key={c.id}>{separator}<button className="breadcrumb-ancestor" title={c.name} onClick={()=>onNavigate(c.id,c.name)} onDragOver={e=>e.preventDefault()} onDrop={e=>onDrop(e,c.id)}>{c.name}</button></Fragment>)}
    {crumbs.length>0&&separator}<motion.h1 ref={headingRef} className="breadcrumb-title" tabIndex={-1} key={parent+title} title={title} aria-current="page" initial={reduce?false:{opacity:.4,y:4}} animate={{opacity:1,y:0}} transition={{duration:.24,ease:[.16,1,.3,1]}}>{title}</motion.h1>
    {/* A clipped, noninteractive ruler measures natural text widths without
        repeatedly mounting visible paths or depending on their truncated size. */}
    <div className="breadcrumb-measure" aria-hidden="true" inert><div ref={probe} className="breadcrumb-probe">
      <span className="breadcrumb-home"><House size={17}/>{crumbs.length>0&&<span>我的文件</span>}</span>
      {separator}<span className="breadcrumb-overflow"><Ellipsis size={18}/></span>
      {ancestors.map(c=><span key={c.id} className="breadcrumb-ancestor">{c.name}</span>)}
      <span className="breadcrumb-title">{title}</span>
    </div></div>
  </nav>
}

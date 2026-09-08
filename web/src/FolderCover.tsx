import { useState } from 'react'
import type { CSSProperties } from 'react'
import type { FileNode } from './api'
import { FileIcon } from './components'

export function FolderCover({file}:{file:FileNode}){
  const [loaded,setLoaded]=useState<string[]>([])
  const [failed,setFailed]=useState<string[]>([])
  const previews=(file.folder_previews||[]).filter(p=>!failed.includes(p.url))
  const ready=previews.some(p=>loaded.includes(p.url))
  return <span className={`folder-preview ${ready?'is-ready':''}`} aria-hidden="true">
    {!ready&&<span className="folder-preview-fallback"><FileIcon file={file} size={55}/></span>}
    <span className="folder-preview-back"/>
    {previews.map((preview,index)=><span key={preview.url} className={`folder-preview-frame ${loaded.includes(preview.url)?'loaded':''}`} style={{'--preview-index':index} as CSSProperties}><img src={preview.url} alt="" loading="lazy" decoding="async" draggable={false} onLoad={()=>setLoaded(old=>old.includes(preview.url)?old:[...old,preview.url])} onError={()=>setFailed(old=>old.includes(preview.url)?old:[...old,preview.url])}/></span>)}
    <span className="folder-preview-front"><i/><i/></span>
  </span>
}

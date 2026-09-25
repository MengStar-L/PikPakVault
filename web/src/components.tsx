import type { ReactNode, ComponentType } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import * as Tooltip from '@radix-ui/react-tooltip'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { X, Folder, Cloud, LoaderCircle, FileText, Film, Image, Music2, Archive, ShieldCheck } from 'lucide-react'
import { fileType, stateLabels } from './api'
import type { FileNode } from './api'

export function Logo({ small = false }: { small?: boolean }) { return <div className={'brand' + (small ? ' small' : '')}><span className="brand-mark"><Cloud size={23} strokeWidth={2.1}/><i/></span><span>PikPak<span className="brand-vault">Vault</span></span></div> }
export function IconButton({ label, children, onClick, active, className = '', disabled = false }: {label: string;children: ReactNode;onClick?: () => void;active?: boolean;className?: string;disabled?: boolean}) { return <Tooltip.Provider delayDuration={350}><Tooltip.Root><Tooltip.Trigger asChild><button type="button" aria-label={label} disabled={disabled} onClick={onClick} className={`icon-button ${active ? 'active' : ''} ${className}`}>{children}</button></Tooltip.Trigger><Tooltip.Portal><Tooltip.Content sideOffset={7} className="tooltip">{label}</Tooltip.Content></Tooltip.Portal></Tooltip.Root></Tooltip.Provider> }
export function Modal({ open, onClose, title, subtitle, children, wide = false, drawer = false, className = '' }: {open: boolean;onClose: () => void;title: string;subtitle?: string;children: ReactNode;wide?: boolean;drawer?: boolean;className?: string}) {
  const reduce=useReducedMotion();
  const initial=reduce?{opacity:0}:drawer?{x:48,opacity:.4}:{y:18,opacity:0,scale:.965};
  const exit=reduce?{opacity:0}:drawer?{x:32,opacity:0}:{y:8,opacity:0,scale:.98};
  return <Dialog.Root open={open} onOpenChange={v=>!v&&onClose()}><AnimatePresence>{open&&<Dialog.Portal forceMount>
    <Dialog.Overlay forceMount asChild><motion.div className="modal-overlay" initial={{opacity:0}} animate={{opacity:1}} exit={{opacity:0}} transition={{duration:reduce?.08:.2}}/></Dialog.Overlay>
    <Dialog.Content forceMount asChild aria-describedby={undefined} onEscapeKeyDown={event=>{if(event.target instanceof Element && event.target.closest('[data-modal-escape-boundary]'))event.preventDefault()}}><motion.div className={`modal ${wide?'wide':''} ${drawer?'drawer':''} ${className}`} initial={initial} animate={{x:0,y:0,opacity:1,scale:1}} exit={{...exit,transition:{duration:reduce?.08:.16}}} transition={{duration:reduce?.08:drawer?.34:.28,ease:[.16,1,.3,1]}}>
      <div className="modal-head"><div><Dialog.Title>{title}</Dialog.Title>{subtitle&&<Dialog.Description>{subtitle}</Dialog.Description>}</div><Dialog.Close asChild><button className="icon-button" aria-label="关闭"><X size={19}/></button></Dialog.Close></div>{children}
    </motion.div></Dialog.Content>
  </Dialog.Portal>}</AnimatePresence></Dialog.Root>
}
export function EmptyState({ icon: Icon = Folder, title, description, children }: {icon?: ComponentType<{size?:number;strokeWidth?:number}>;title:string;description:string;children?:ReactNode}) { return <div className="empty-state"><div className="empty-art"><span/><Icon size={42} strokeWidth={1.4}/><i className="spark spark-one"/><i className="spark spark-two"/></div><h3>{title}</h3><p>{description}</p>{children}</div> }
export function Loading() { return <div className="loading"><LoaderCircle size={23} className="spin"/><span>正在加载…</span></div> }
export function ErrorState({ error, retry }: {error: unknown;retry?: () => void}) { return <div role="alert" className="error-box"><strong>暂时无法完成操作</strong><p>{error instanceof Error ? error.message : '连接失败，请稍后重试'}</p>{retry && <button type="button" className="button secondary" onClick={retry}>重试</button>}</div> }
export function FileIcon({ file, size = 24 }: {file: FileNode;size?: number}) { const kind = fileType(file);const Icon = {folder: Folder,video: Film,image: Image,audio: Music2,archive: Archive,document: FileText}[kind]; return <span className={`file-icon ${kind}`}><Icon size={size} strokeWidth={1.7} fill={kind === 'folder' ? 'currentColor' : 'none'}/></span> }
export function Status({ state }: {state:string}) { return <span className={`status ${state}`}><i/>{stateLabels[state] || state}</span> }
export function Confirm({ open, title, children, confirm, onClose, label = '确认', danger = false, busy = false }: {open:boolean;title:string;children:ReactNode;confirm:()=>void;onClose:()=>void;label?:string;danger?:boolean;busy?:boolean}) { return <Modal open={open} onClose={onClose} title={title}><div className="modal-body">{children}</div><div className="modal-actions"><button className="button secondary" onClick={onClose}>取消</button><button disabled={busy} className={`button ${danger ? 'danger' : 'primary'}`} onClick={confirm}>{busy && <LoaderCircle className="spin" size={16}/>} {label}</button></div></Modal> }
export { FolderPicker } from './FolderPicker'
export function BackupNote() { return <div className="backup-note"><ShieldCheck size={17}/><span>目录与来源会保存在本地，恢复记录不随远端删除而消失。</span></div> }

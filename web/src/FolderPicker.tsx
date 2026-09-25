import { useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { AnimatePresence, motion, useIsPresent, useReducedMotion } from 'motion/react'
import { ArrowLeft, Check, ChevronRight, Folder, FolderOpen, Home, LoaderCircle, RefreshCw } from 'lucide-react'
import { api } from './api'
import type { FilesResult } from './api'

type Crumb = FilesResult['breadcrumbs'][number]
const pageSize = 100
export const folderPath = (crumbs: Crumb[]) => ['我的文件', ...crumbs.map(c => c.name)].join(' / ')

export function useFolderPage(id: string, page = 0, enabled = true) {
  return useQuery({
    queryKey: ['picker', id, page],
    queryFn: () => api<FilesResult>(`/files?transfers=0&kind=folder&parent=${encodeURIComponent(id)}&limit=${pageSize}&page=${page}`),
    enabled,
    retry: false,
  })
}

export function FolderPickerPanel({ children, id }: { children: ReactNode; id: string }) {
  const present = useIsPresent()
  const reduce = useReducedMotion()
  return <motion.div id={id} className="destination-picker-panel" inert={!present} aria-hidden={!present || undefined}
    initial={{ height: 0, opacity: 0 }} animate={{ height: 'auto', opacity: 1 }} exit={{ height: 0, opacity: 0 }}
    transition={{ duration: reduce ? 0 : .26, ease: [.16, 1, .3, 1] }}>{children}</motion.div>
}

// Departing rows must stop accepting input immediately, even during their exit animation.
function FolderPage({ children, direction, reduce }: { children: ReactNode; direction: number; reduce: boolean }) {
  const present = useIsPresent()
  return <motion.div className="picker-page" inert={!present} aria-hidden={!present || undefined}
    initial={{ opacity: 0, x: reduce ? 0 : direction * 22 }}
    animate={{ opacity: 1, x: 0 }} exit={{ opacity: 0, x: reduce ? 0 : direction * -12 }}
    transition={{ duration: reduce ? 0 : present ? .28 : .1, ease: [.16, 1, .3, 1] }}>
    {children}
  </motion.div>
}

export function FolderPicker({ value, onChange, exclude = [] }: {
  value: string; onChange: (id: string, name: string, path: string) => void; exclude?: string[]
}) {
  // The displayed folder is the selected folder; there is no separate confirmation state.
  const current = value || 'root'
  const [paging, setPaging] = useState({ id: current, page: 0 })
  const page = paging.id === current ? paging.page : 0
  const [hint, setHint] = useState<{ id: string; crumbs: Crumb[] } | null>(null)
  const [direction, setDirection] = useState(1)
  const q = useFolderPage(current, page)
  const firstPage = useQueryClient().getQueryData<FilesResult>(['picker', current, 0])
  const reduce = !!useReducedMotion()
  const heading = useRef<HTMLDivElement>(null)
  const scroller = useRef<HTMLDivElement>(null)
  const crumbEnd = useRef<HTMLSpanElement>(null)
  const keyboardNavigation = useRef(false)
  const crumbs = q.data?.breadcrumbs ?? firstPage?.breadcrumbs ?? (hint?.id === current ? hint.crumbs : [])
  const name = crumbs.at(-1)?.name || (current === 'root' ? '我的文件' : '当前文件夹')
  const folders = q.data?.files.filter(f => f.kind === 'folder' && !f.transfer && !exclude.includes(f.id)) || []
  const total = q.data?.total ?? firstPage?.total ?? 0

  useLayoutEffect(() => {
    scroller.current?.scrollTo({ top: 0, behavior: 'instant' })
    if (keyboardNavigation.current) {
      heading.current?.focus({ preventScroll: true })
      keyboardNavigation.current = false
    }
    const end = crumbEnd.current
    if (end?.parentElement) end.parentElement.scrollTop = end.parentElement.scrollHeight
  }, [current, page, q.isPending])

  function navigate(next: Crumb[], backwards: boolean, keyboard: boolean) {
    const id = next.at(-1)?.id || 'root'
    if (id === current) return
    keyboardNavigation.current = keyboard
    setDirection(backwards ? -1 : 1)
    setPaging({ id, page: 0 })
    setHint({ id, crumbs: next })
    onChange(id, next.at(-1)?.name || '我的文件', folderPath(next))
  }

  return <div className="folder-picker" aria-label="保存位置选择器">
    <nav className="picker-crumb" aria-label="文件夹路径">
      <button type="button" aria-label="我的文件" aria-current={current === 'root' ? 'location' : undefined}
        onClick={e => navigate([], true, e.detail === 0)}><Home size={14}/><span>我的文件</span></button>
      {crumbs.map((c, i) => <span key={c.id}><ChevronRight size={12}/><button type="button" title={c.name}
        aria-current={c.id === current ? 'location' : undefined}
        onClick={e => navigate(crumbs.slice(0, i + 1), true, e.detail === 0)}>{c.name}</button></span>)}
      <span ref={crumbEnd} className="picker-crumb-end"/>
    </nav>
    <div className="picker-current" ref={heading} tabIndex={-1}>
      <button type="button" className="picker-up" aria-label="返回上级" title="返回上级"
        disabled={current === 'root' || !crumbs.length}
        onClick={e => navigate(crumbs.slice(0, -1), true, e.detail === 0)}><ArrowLeft size={17}/></button>
      <FolderOpen size={21}/><div><strong title={name}>{name}</strong><small>点击文件夹即可切换保存位置</small></div>
      <span className="picker-selected" role="status"><Check size={13}/>已选定</span>
    </div>
    <div className="picker-list" ref={scroller} aria-busy={q.isPending}>
      <AnimatePresence initial={false} mode="wait">
        <FolderPage key={`${current}:${page}:${q.isPending ? 'loading' : q.isError ? 'error' : 'ready'}`} direction={direction} reduce={reduce}>
          {q.isPending ? <div className="picker-message" role="status"><LoaderCircle size={22} className="spin"/><span>正在读取文件夹…</span></div>
            : q.isError ? <div className="picker-message picker-error" role="alert"><span>暂时无法读取子文件夹</span><small>{q.error.message}</small><button type="button" className="button secondary" onClick={() => q.refetch()} disabled={q.isFetching}><RefreshCw size={14} className={q.isFetching ? 'spin' : ''}/>重试</button></div>
            : folders.length ? folders.map(f => <button type="button" key={f.id} onClick={e => navigate([...crumbs, { id: f.id, name: f.name }], false, e.detail === 0)}>
              <Folder size={20}/><span>{f.name}</span><ChevronRight size={15}/>
            </button>) : <div className="picker-message"><FolderOpen size={28}/><span>{exclude.length ? '没有可进入的子文件夹' : '没有子文件夹'}</span><small>已选定此位置，可直接继续</small></div>}
        </FolderPage>
      </AnimatePresence>
    </div>
    {total > pageSize && <div className="pagination">
      <button type="button" disabled={page === 0} onClick={() => { setDirection(-1); setPaging({ id: current, page: page - 1 }) }}>上一页</button>
      <span>{page + 1} / {Math.ceil(total / pageSize)}</span>
      <button type="button" disabled={(page + 1) * pageSize >= total} onClick={() => { setDirection(1); setPaging({ id: current, page: page + 1 }) }}>下一页</button>
    </div>}
  </div>
}

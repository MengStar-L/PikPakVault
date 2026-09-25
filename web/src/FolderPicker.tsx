import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { KeyboardEventHandler, ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { AnimatePresence, motion, useIsPresent, useReducedMotion } from 'motion/react'
import * as Dropdown from '@radix-ui/react-dropdown-menu'
import { ArrowLeft, ChevronRight, Ellipsis, Folder, FolderOpen, FolderPlus, Home, LoaderCircle, PencilLine, RefreshCw, Trash2, X } from 'lucide-react'
import { toast } from 'sonner'
import { api } from './api'
import type { FilesResult } from './api'

type Crumb = FilesResult['breadcrumbs'][number]
type FolderEdit = { kind: 'create' | 'rename' | 'trash'; id: string; name: string }
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

function FolderEditor({ children, deleting, reduce, onKeyDown }: { children: ReactNode; deleting: boolean; reduce: boolean; onKeyDown: KeyboardEventHandler<HTMLDivElement> }) {
  const present = useIsPresent()
  return <motion.div data-modal-escape-boundary className={`picker-editor ${deleting ? 'is-delete' : ''}`} inert={!present} aria-hidden={!present || undefined}
    initial={{ height: 0, opacity: 0 }} animate={{ height: 'auto', opacity: 1 }} exit={{ height: 0, opacity: 0 }}
    transition={{ duration: reduce ? 0 : .18 }} onKeyDown={onKeyDown}>{children}</motion.div>
}

export function FolderPicker({ value, onChange, exclude = [], onBusyChange }: {
  value: string; onChange: (id: string, name: string, path: string) => void; exclude?: string[]; onBusyChange?: (busy: boolean) => void
}) {
  // The displayed folder is the selected folder; there is no separate confirmation state.
  const current = value || 'root'
  const [paging, setPaging] = useState({ id: current, page: 0 })
  const page = paging.id === current ? paging.page : 0
  const [hint, setHint] = useState<{ id: string; crumbs: Crumb[] } | null>(null)
  const [direction, setDirection] = useState(1)
  const q = useFolderPage(current, page)
  const client = useQueryClient()
  const firstPage = client.getQueryData<FilesResult>(['picker', current, 0])
  const [editing, setEditing] = useState<FolderEdit | null>(null)
  const [draft, setDraft] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submitting = useRef(false)
  const mounted = useRef(true)
  const currentRef = useRef(current)
  currentRef.current = current
  const reduce = !!useReducedMotion()
  const heading = useRef<HTMLDivElement>(null)
  const scroller = useRef<HTMLDivElement>(null)
  const crumbEnd = useRef<HTMLSpanElement>(null)
  const keyboardNavigation = useRef(false)
  const crumbs = q.data?.breadcrumbs ?? firstPage?.breadcrumbs ?? (hint?.id === current ? hint.crumbs : [])
  const name = crumbs.at(-1)?.name || (current === 'root' ? '我的文件' : '当前文件夹')
  const folders = q.data?.files.filter(f => f.kind === 'folder' && !f.transfer && !exclude.includes(f.id)) || []
  const total = q.data?.total ?? firstPage?.total ?? 0

  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  // An inline editor must never accidentally submit the surrounding import/RSS form.
  useEffect(() => { onBusyChange?.(!!editing); return () => onBusyChange?.(false) }, [!!editing, onBusyChange])
  useEffect(() => {
    if (q.data && page > 0 && page * pageSize >= q.data.total) {
      setPaging({ id: current, page: Math.max(0, Math.ceil(q.data.total / pageSize) - 1) })
    }
  }, [q.data, current, page])

  useLayoutEffect(() => {
    scroller.current?.scrollTo({ top: 0, behavior: 'instant' })
    if (keyboardNavigation.current) {
      heading.current?.focus({ preventScroll: true })
      keyboardNavigation.current = false
    }
    const end = crumbEnd.current
    if (end?.parentElement) end.parentElement.scrollLeft = end.parentElement.scrollWidth
  }, [current, page, q.isPending])

  useLayoutEffect(() => {
    const nav = crumbEnd.current?.parentElement
    if (!nav) return
    const observer = new ResizeObserver(() => { nav.scrollLeft = nav.scrollWidth })
    observer.observe(nav)
    return () => observer.disconnect()
  }, [])

  function navigate(next: Crumb[], backwards: boolean, keyboard: boolean) {
    if (submitting.current) return
    const id = next.at(-1)?.id || 'root'
    if (id === current) return
    keyboardNavigation.current = keyboard
    setDirection(backwards ? -1 : 1)
    setPaging({ id, page: 0 })
    setHint({ id, crumbs: next })
    setEditing(null); setError('')
    onChange(id, next.at(-1)?.name || '我的文件', folderPath(next))
  }

  function edit(kind: FolderEdit['kind'], id = current, label = name) {
    if (submitting.current) return
    setEditing({ kind, id, name: label }); setDraft(kind === 'create' ? '' : label); setError('')
  }
  async function saveFolder() {
    if (!editing || submitting.current || (editing.kind !== 'trash' && !draft.trim())) return
    const operation = editing, origin = current, nextName = draft.trim(), trail = [...crumbs]
    submitting.current = true; setBusy(true); setError('')
    try {
      let created = ''
      if (operation.kind === 'create') {
        const result = await api<{ id: string }>('/files', { parent_id: origin, name: nextName })
        created = result.id
      } else {
        await api('/files/action', { ids: [operation.id], action: operation.kind, ...(operation.kind === 'rename' ? { name: nextName } : {}) })
      }
      await client.cancelQueries({ queryKey: ['picker'] })
      // Apply known local results before refetching, including inactive breadcrumb caches.
      client.setQueriesData<FilesResult>({ queryKey: ['picker'] }, old => !old ? old : ({ ...old,
        breadcrumbs: old.breadcrumbs.map(c => operation.kind === 'rename' && c.id === operation.id ? { ...c, name: nextName } : c),
        files: old.files.filter(f => operation.kind !== 'trash' || f.id !== operation.id).map(f => operation.kind === 'rename' && f.id === operation.id ? { ...f, name: nextName } : f),
        total: old.total - (operation.kind === 'trash' && old.files.some(f => f.id === operation.id) ? 1 : 0),
      }))
      if (mounted.current && currentRef.current === origin) {
        let next = trail
        if (created) {
          next = [...trail, { id: created, name: nextName }]
          client.setQueryData<FilesResult>(['picker', created, 0], { files: [], breadcrumbs: next, total: 0, page: 0, limit: pageSize })
        } else if (operation.kind === 'trash' && operation.id === origin) next = trail.slice(0, -1)
        else if (operation.kind === 'rename') next = trail.map(c => c.id === operation.id ? { ...c, name: nextName } : c)
        const id = next.at(-1)?.id || 'root'
        setHint({ id, crumbs: next }); setPaging({ id, page: created || id !== origin ? 0 : page }); setDirection(id !== origin && !created ? -1 : 1)
        onChange(id, next.at(-1)?.name || '我的文件', folderPath(next))
        setEditing(null)
        heading.current?.focus({ preventScroll: true })
        toast.success(created ? '文件夹已创建，已选为保存位置' : operation.kind === 'rename' ? '文件夹名称已更新' : '文件夹已移到回收站')
      }
      await Promise.all(['picker', 'files', 'jobs', 'summary', 'rss', 'teldrive', 'detail'].map(key => client.invalidateQueries({ queryKey: [key] })))
    } catch (e) { if (mounted.current) setError((e as Error).message) }
    finally { submitting.current = false; if (mounted.current) setBusy(false) }
  }

  function folderMenu(id: string, label: string, currentFolder = false) {
    return <Dropdown.Root><Dropdown.Trigger asChild><button type="button" className="picker-menu" disabled={busy} aria-label={currentFolder ? '当前文件夹操作' : `${label} 的文件夹操作`} title="文件夹操作"><Ellipsis size={17}/></button></Dropdown.Trigger><Dropdown.Portal><Dropdown.Content className="dropdown-content" align="end" sideOffset={5} onCloseAutoFocus={e => e.preventDefault()}>
      <Dropdown.Item onSelect={() => edit('rename', id, label)}><PencilLine size={15}/>重命名</Dropdown.Item>
      <Dropdown.Item className="destructive" onSelect={() => edit('trash', id, label)}><Trash2 size={15}/>移到回收站</Dropdown.Item>
    </Dropdown.Content></Dropdown.Portal></Dropdown.Root>
  }

  return <div className="folder-picker" aria-label="保存位置选择器">
    <div className="picker-toolbar" ref={heading} tabIndex={-1}>
      <button type="button" className="picker-up" aria-label="返回上级" title="返回上级" disabled={busy || current === 'root' || !crumbs.length}
        onClick={e => navigate(crumbs.slice(0, -1), true, e.detail === 0)}><ArrowLeft size={16}/></button>
    <nav className="picker-crumb" aria-label="文件夹路径">
      <button type="button" aria-label="我的文件" aria-current={current === 'root' ? 'location' : undefined}
        disabled={busy} onClick={e => navigate([], true, e.detail === 0)}><Home size={14}/><span>我的文件</span></button>
      {crumbs.map((c, i) => <span key={c.id}><ChevronRight size={12}/><button type="button" title={c.name}
        aria-current={c.id === current ? 'location' : undefined}
        disabled={busy} onClick={e => navigate(crumbs.slice(0, i + 1), true, e.detail === 0)}>{c.name}</button></span>)}
      <span ref={crumbEnd} className="picker-crumb-end"/>
    </nav>
      <button type="button" className="picker-create" aria-label="新建文件夹" title="新建文件夹" disabled={busy || (!q.data && !firstPage)} onClick={() => edit('create')}><FolderPlus size={16}/><span>新建</span></button>
      {current !== 'root' && !!crumbs.length && folderMenu(current, name, true)}
    </div>
    <AnimatePresence initial={false} mode="wait">{editing && <FolderEditor deleting={editing.kind === 'trash'} reduce={reduce} key={`${editing.kind}:${editing.id}`}
      onKeyDown={e => { if (e.nativeEvent.isComposing) return; if (e.key === 'Enter' && e.target instanceof HTMLInputElement) { e.preventDefault(); e.stopPropagation(); void saveFolder() } if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); if (!busy) { setEditing(null); setError('') } } }}>
      <div className="picker-editor-inner"><div className="picker-editor-heading"><strong>{editing.kind === 'create' ? '新建文件夹' : editing.kind === 'rename' ? '重命名文件夹' : '移到回收站？'}</strong><button type="button" aria-label="取消文件夹操作" disabled={busy} onClick={() => { setEditing(null); setError('') }}><X size={15}/></button></div>
        {editing.kind === 'trash' ? <p>“{editing.name}”及其中的内容将移到本地和当前账号的回收站，之后可以还原。{editing.id === current && '删除后将返回上一级目录。'}</p> : <input key={`${editing.kind}:${editing.id}`} autoFocus aria-label="文件夹名称" placeholder="输入文件夹名称" maxLength={300} value={draft} disabled={busy} onChange={e => setDraft(e.target.value)}/>}
        {error && <div role="alert" className="picker-edit-error">{error}</div>}
        <div className="picker-editor-actions"><button type="button" className="button secondary small" disabled={busy} onClick={() => { setEditing(null); setError('') }}>取消</button><button type="button" className={`button small ${editing.kind === 'trash' ? 'danger' : 'primary'}`} disabled={busy || (editing.kind !== 'trash' && !draft.trim())} onClick={() => void saveFolder()}>{busy && <LoaderCircle size={14} className="spin"/>}{editing.kind === 'create' ? '创建并进入' : editing.kind === 'rename' ? '保存名称' : '移到回收站'}</button></div>
      </div></FolderEditor>}</AnimatePresence>
    <div className="picker-list" ref={scroller} aria-busy={q.isPending}>
      <AnimatePresence initial={false} mode="wait">
        <FolderPage key={`${current}:${page}:${q.isPending ? 'loading' : q.isError ? 'error' : 'ready'}`} direction={direction} reduce={reduce}>
          {q.isPending ? <div className="picker-message" role="status"><LoaderCircle size={22} className="spin"/><span>正在读取文件夹…</span></div>
            : q.isError ? <div className="picker-message picker-error" role="alert"><span>暂时无法读取子文件夹</span><small>{q.error.message}</small><button type="button" className="button secondary" onClick={() => q.refetch()} disabled={q.isFetching}><RefreshCw size={14} className={q.isFetching ? 'spin' : ''}/>重试</button></div>
            : folders.length ? folders.map(f => <div className="picker-row" key={f.id}><button type="button" disabled={busy} onClick={e => navigate([...crumbs, { id: f.id, name: f.name }], false, e.detail === 0)}>
              <Folder size={20}/><span>{f.name}</span><ChevronRight size={15}/>
            </button>{folderMenu(f.id, f.name)}</div>) : <div className="picker-message"><FolderOpen size={28}/><span>{exclude.length ? '没有可进入的子文件夹' : '没有子文件夹'}</span><small>可在此新建文件夹，或直接保存到当前位置</small></div>}
        </FolderPage>
      </AnimatePresence>
    </div>
    {total > pageSize && <div className="pagination">
      <button type="button" disabled={busy || page === 0} onClick={() => { setEditing(null); setDirection(-1); setPaging({ id: current, page: page - 1 }) }}>上一页</button>
      <span>{page + 1} / {Math.ceil(total / pageSize)}</span>
      <button type="button" disabled={busy || (page + 1) * pageSize >= total} onClick={() => { setEditing(null); setDirection(1); setPaging({ id: current, page: page + 1 }) }}>下一页</button>
    </div>}
  </div>
}

import { useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { ArrowUpRight, Check, ChevronDown, Clock3, Folder, History, LoaderCircle, Pause, Play, Plus, RefreshCw, Rss, Settings2, ShieldCheck, Trash2, Unplug } from 'lucide-react'
import { toast } from 'sonner'
import { api } from './api'
import type { Job } from './api'
import { Confirm, EmptyState, ErrorState, FolderPicker, IconButton, Loading, Modal, Status } from './components'
import { FolderPickerPanel } from './FolderPicker'
import { TaskDetail } from './pages'
import { useJobRetry } from './useJobRetry'
import './rss.css'

export type RSSSubscription = {
  id: string; name: string; url: string; parent_id: string; target_path: string
  interval_minutes: number; enabled: boolean; initialized: boolean; import_existing: boolean
  last_checked: number; next_check: number; last_error: string; last_job: Job | null
  counts: { total: number; saved: number; pending: number; failed: number; skipped: number }
}
type RSSEntry = { id: string; title: string; published: number; discovered: number; state: string; message: string; job_id: string; account_id: string; source_id: string }
const time = (value: number) => value ? new Date(value * 1000).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '尚未检查'
const interval = (minutes: number) => minutes >= 1440 && minutes % 1440 === 0 ? `每 ${minutes / 1440} 天` : minutes < 60 ? `每 ${minutes} 分钟` : `每 ${minutes / 60} 小时`
const running = (job: Job | null) => !!job && ['queued', 'running', 'waiting', 'retry'].includes(job.state)
const entryLabels: Record<string, string> = { saved: '已保存', pending: '保存中', failed: '待处理', skipped: '已跳过' }
function feedAddress(value: string) {
  try { const url = new URL(value); return url.host + url.pathname } catch { return '订阅地址待检查' }
}

export default function RSSPage() {
  const client = useQueryClient()
  const q = useQuery({ queryKey: ['rss'], queryFn: () => api<{ subscriptions: RSSSubscription[]; active_account: string }>('/rss'), refetchInterval: 5000 })
  const [editing, setEditing] = useState<RSSSubscription | 'new' | null>(null)
  const [removing, setRemoving] = useState<RSSSubscription | null>(null)
  const [details, setDetails] = useState<RSSSubscription | null>(null)
  const [pending, setPending] = useState('')
  const submitting = useRef(false)
  const reduce = useReducedMotion()
  const subscriptions = q.data?.subscriptions || []
  const active = subscriptions.filter(s => s.enabled).length
  const saved = subscriptions.reduce((sum, s) => sum + s.counts.saved, 0)
  async function action(s: RSSSubscription, kind: 'check' | 'toggle' | 'delete') {
    if (submitting.current) return
    submitting.current = true; setPending(`${s.id}:${kind}`)
    try {
      if (kind === 'check') {
        await api(`/rss/${s.id}/check`, {})
        toast.success('检查已排队，新资源会保存到指定文件夹')
      } else if (kind === 'toggle') {
        await api(`/rss/${s.id}`, { enabled: !s.enabled }, 'PATCH')
        toast.success(s.enabled ? '自动检查已暂停，已创建的传输任务继续运行' : '自动检查已开启')
      } else {
        await api(`/rss/${s.id}`, undefined, 'DELETE')
        setRemoving(null); toast.success('订阅已移除，已保存的资源仍保留')
      }
      await Promise.all(['rss', 'rss-entries', 'jobs', 'files'].map(key => client.invalidateQueries({ queryKey: [key] })))
    } catch (e) { toast.error((e as Error).message) }
    finally { submitting.current = false; setPending('') }
  }
  return <div className="scroll-page rss-page">
    <div className="page-heading"><div className="heading-copy"><span className="eyebrow">NEW FINDS, AUTOMATICALLY KEPT</span><h1>RSS 订阅</h1><p>订阅你喜欢的更新，新资源自动存入专属文件夹。</p></div><button className="button primary" onClick={() => setEditing('new')}><Plus size={17}/>添加订阅</button></div>
    <div className="rss-overview"><span className="rss-symbol"><Rss size={27}/></span><div className="rss-overview-copy"><strong>每一份更新，都有自己的归处。</strong><p>一个订阅，一个保存位置。自动检查新条目，已发现的内容不重复转存。</p></div><div className="rss-stats"><span><strong>{active}</strong><small>自动订阅</small></span><span><strong>{saved}</strong><small>已保存</small></span></div></div>
    {q.data && !q.data.active_account && <div className="warning-box rss-account-note"><Unplug size={18}/><span>连接并验证 PikPak 账号后，新资源才能开始保存。</span><Link to="/accounts">连接账号<ArrowUpRight size={14}/></Link></div>}
    {q.isPending ? <Loading/> : q.error ? <ErrorState error={q.error} retry={() => q.refetch()}/> : !subscriptions.length ? <EmptyState icon={Rss} title="让新内容，自己来到收藏里" description="添加 RSS 或 Atom 地址，选择一个文件夹。默认从首次成功检查之后的新条目开始保存。"><button className="button secondary" onClick={() => setEditing('new')}><Plus size={16}/>创建第一个订阅</button></EmptyState> :
      <div className="rss-subscriptions">{subscriptions.map((s, index) => <motion.article className={`rss-card${s.enabled ? '' : ' is-paused'}`} key={s.id} initial={reduce ? false : { opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: .28, delay: Math.min(index * .035, .18) }}>
        <div className="rss-card-heading"><span className="rss-feed-icon"><Rss size={20}/></span><div><h2>{s.name}</h2><p title={feedAddress(s.url)}>{feedAddress(s.url)}</p></div><span className={`rss-mode ${s.enabled ? 'automatic' : ''}`}>{s.enabled ? <Clock3 size={13}/> : <Pause size={13}/>} {s.enabled ? interval(s.interval_minutes) : '已暂停'}</span></div>
        <Link className="rss-target" to={`/files?folder=${encodeURIComponent(s.parent_id)}`}><Folder size={19}/><span><small>保存到</small><strong>{s.target_path || '我的文件'}</strong></span><ArrowUpRight size={16}/></Link>
        <div className="rss-card-stats"><span><i className="saved"/><strong>{s.counts.saved}</strong> 已保存</span><span><i className="pending"/><strong>{s.counts.pending}</strong> 保存中</span><span><i className="failed"/><strong>{s.counts.failed}</strong> 待处理</span><span><strong>{s.counts.skipped}</strong> 已跳过</span></div>
        <div className="rss-last-check"><span><Clock3 size={13}/>上次检查 · {time(s.last_checked)}</span>{s.enabled && s.next_check > 0 && <span>下次 · {time(s.next_check)}</span>}</div>
        {s.last_error ? <div className="rss-card-error" role="status">{s.last_error}</div> : s.last_job ? <div className="rss-job-state"><Status state={s.last_job.state}/><span>{s.last_job.message || '正在检查订阅内容'}</span></div> : <p className="rss-first-check">{s.initialized ? '等待下次检查，新内容会自动出现在目标目录。' : s.import_existing ? '首次检查将同时保存订阅中现有的资源。' : '首次成功检查建立记录，之后只保存新增资源。'}</p>}
        <footer><button className="text-button" onClick={() => setDetails(s)}><History size={15}/>订阅记录</button><div className="rss-card-controls"><IconButton label={`编辑 ${s.name}`} onClick={() => setEditing(s)}><Settings2 size={16}/></IconButton><IconButton label={`移除 ${s.name}`} disabled={!!pending} onClick={() => setRemoving(s)}><Trash2 size={16}/></IconButton><button className="button secondary small" disabled={!!pending} onClick={() => void action(s, 'toggle')}>{pending === `${s.id}:toggle` ? <LoaderCircle size={14} className="spin"/> : s.enabled ? <Pause size={14}/> : <Play size={14}/>} {s.enabled ? '暂停' : '启用'}</button><button className="button primary small" disabled={!!pending || running(s.last_job) || !q.data?.active_account} onClick={() => void action(s, 'check')}>{pending === `${s.id}:check` || running(s.last_job) ? <LoaderCircle size={14} className="spin"/> : <RefreshCw size={14}/>} {running(s.last_job) ? '检查中' : '立即检查'}</button></div></footer>
      </motion.article>)}</div>}
    <p className="rss-help"><ShieldCheck size={16}/><span>支持 RSS / Atom 中的磁链、PikPak 分享链接和公开可下载附件。保存进度可在订阅记录和传输任务中查看；暂停订阅不取消已创建的任务。后续新任务使用当前 PikPak 账号。</span></p>
    {editing && <SubscriptionDialog subscription={editing === 'new' ? null : editing} onClose={() => setEditing(null)}/>}
    {details && <EntryDialog subscription={subscriptions.find(s => s.id === details.id) || details} onClose={() => setDetails(null)}/>}
    <Confirm open={!!removing} onClose={() => setRemoving(null)} title="移除这个 RSS 订阅？" label="移除订阅" danger busy={pending.endsWith(':delete')} confirm={() => removing && void action(removing, 'delete')}><p>停止跟踪“{removing?.name}”的新内容，并移除订阅记录。已经保存的文件、来源与传输任务都会保留。</p></Confirm>
  </div>
}

function SubscriptionDialog({ subscription, onClose }: { subscription: RSSSubscription | null; onClose: () => void }) {
  const client = useQueryClient()
  const reduce = useReducedMotion()
  const [name, setName] = useState(subscription?.name || '')
  const [url, setURL] = useState(subscription?.url || '')
  const [target, setTarget] = useState(subscription?.parent_id || 'root')
  const [targetName, setTargetName] = useState(subscription?.target_path || '我的文件')
  const [picker, setPicker] = useState(false)
  const [minutes, setMinutes] = useState(subscription?.interval_minutes || 30)
  const [enabled, setEnabled] = useState(subscription?.enabled ?? true)
  const [existing, setExisting] = useState(subscription?.import_existing || false)
  const [busy, setBusy] = useState(false)
  const submitting = useRef(false)
  const [error, setError] = useState('')
  async function save(e: React.FormEvent) {
    e.preventDefault()
    if (submitting.current) return
    submitting.current = true; setBusy(true); setError('')
    try {
      await api(subscription ? `/rss/${subscription.id}` : '/rss', { name: name.trim(), url: url.trim(), parent_id: target, interval_minutes: minutes, enabled, import_existing: existing }, subscription ? 'PATCH' : 'POST')
      await client.invalidateQueries({ queryKey: ['rss'] })
      toast.success(subscription ? '订阅设置已保存' : enabled ? '订阅已添加，自动检查已开启' : '订阅已添加，可随时手动检查')
      onClose()
    } catch (e) { setError((e as Error).message) }
    finally { submitting.current = false; setBusy(false) }
  }
  return <Modal open onClose={onClose} title={subscription ? '编辑 RSS 订阅' : '添加 RSS 订阅'} subtitle="为一份订阅，指定一个保存位置。" wide className="rss-editor-modal">
    <form className="rss-form" onSubmit={save}><div className="modal-body rss-editor">
      <label>订阅名称<input value={name} onChange={e => setName(e.target.value)} placeholder="例如：每周电影更新" maxLength={200} required autoFocus/></label>
      <label>RSS / Atom 地址<input type="url" value={url} onChange={e => setURL(e.target.value)} placeholder="https://example.com/feed.xml" required readOnly={!!subscription} autoComplete="off" spellCheck={false}/><small>{subscription ? '订阅地址与历史记录关联，如需更换地址请添加新订阅。' : '填写订阅源地址，条目中需包含磁链、PikPak 分享链接或公开可下载附件。'}</small></label>
      <div className="destination-section"><div className="destination-row"><span><Folder size={18}/>保存到</span><button type="button" aria-expanded={picker} aria-controls="rss-folder-picker" onClick={() => setPicker(v => !v)}><span className="destination-path" title={targetName}>{targetName}</span><span className="destination-toggle">{picker ? '收起' : '更改'}<motion.span animate={{ rotate: picker ? 180 : 0 }} transition={{ duration: reduce ? 0 : .22 }}><ChevronDown size={15}/></motion.span></span></button></div><AnimatePresence initial={false}>{picker && <FolderPickerPanel id="rss-folder-picker"><FolderPicker value={target} onChange={(id, _name, path) => { setTarget(id); setTargetName(path) }}/></FolderPickerPanel>}</AnimatePresence>{subscription && <p className="rss-field-note">更换位置只影响之后发现的资源，已有文件不会被移动。</p>}</div>
      <label>检查频率<select aria-label="检查频率" value={minutes} onChange={e => setMinutes(Number(e.target.value))}>{[...new Set([5, 15, 30, 60, 180, 360, 1440, 10080, minutes])].sort((a, b) => a - b).map(value => <option key={value} value={value}>{interval(value)}</option>)}</select></label>
      <label className="rss-option"><input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)}/><span><strong>自动检查并保存新资源</strong><small>关闭后仍可点击“立即检查”。</small></span></label>
      {!subscription && <label className="rss-option"><input type="checkbox" checked={existing} onChange={e => setExisting(e.target.checked)}/><span><strong>同时保存订阅中现有的资源</strong><small>默认仅记录首次成功检查时的条目，从下一次更新开始保存。</small></span></label>}
    </div>{error && <div className="rss-form-error inline-error" role="alert">{error}</div>}<div className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>取消</button><button className="button primary" disabled={busy || !name.trim() || !url.trim()}>{busy ? <LoaderCircle className="spin" size={16}/> : <Check size={16}/>}保存订阅</button></div></form>
  </Modal>
}

function EntryDialog({ subscription: s, onClose }: { subscription: RSSSubscription; onClose: () => void }) {
  const client = useQueryClient()
  const [page, setPage] = useState(0)
  const [job, setJob] = useState<string | null>(null)
  const retry = useJobRetry()
  const q = useQuery({ queryKey: ['rss-entries', s.id, page], queryFn: () => api<{ entries: RSSEntry[]; total: number; page: number; limit: number }>(`/rss/${s.id}/entries?page=${page}&limit=50`), refetchInterval: 5000 })
  async function retryEntry(id: string) {
    await retry.retry(id)
    await Promise.all(['rss', 'rss-entries'].map(key => client.invalidateQueries({ queryKey: [key] })))
  }
  return <>
    <Modal open onClose={onClose} title="订阅记录" subtitle={s.name} wide className="rss-record-modal"><div className="modal-body rss-record-body">
      <div className="rss-record-summary"><span>{q.data?.total || 0} 条记录</span><button type="button" className="text-button" disabled={q.isFetching} onClick={() => q.refetch()}><RefreshCw size={14} className={q.isFetching ? 'spin' : ''}/>刷新</button></div>
      {q.isPending ? <Loading/> : q.error ? <ErrorState error={q.error} retry={() => q.refetch()}/> : !q.data?.entries.length ? <EmptyState icon={History} title="还没有发现条目" description="检查订阅后，条目的保存进度与处理结果会显示在这里。"/> : <div className="rss-entries">{q.data.entries.map(entry => <article className="rss-entry" key={entry.id}><div className="rss-entry-heading"><h3>{entry.title || '未命名条目'}</h3><span className={`rss-entry-status ${entry.state}`}>{entryLabels[entry.state] || entry.state}</span></div><p>{entry.message || (entry.state === 'pending' ? '正在等待保存结果' : '')}</p><div className="rss-entry-bottom"><span>发现于 {time(entry.discovered)}{entry.published > 0 && <> · 发布于 {time(entry.published)}</>}</span>{entry.job_id && <div><button type="button" className="text-button" onClick={() => setJob(entry.job_id)}>任务详情<ArrowUpRight size={14}/></button>{entry.state === 'failed' && <button type="button" className="text-button" disabled={!!retry.busyJob} onClick={() => void retryEntry(entry.job_id)}>{retry.busyJob === entry.job_id ? <LoaderCircle size={14} className="spin"/> : <RefreshCw size={14}/>}重试原任务</button>}</div>}</div></article>)}</div>}
      {q.data && q.data.total > q.data.limit && <nav className="pagination" aria-label="订阅记录分页"><button type="button" disabled={page === 0} onClick={() => setPage(p => p - 1)}>上一页</button><span>{page + 1} / {Math.ceil(q.data.total / q.data.limit)}</span><button type="button" disabled={(page + 1) * q.data.limit >= q.data.total} onClick={() => setPage(p => p + 1)}>下一页</button></nav>}
    </div><div className="modal-actions"><button type="button" className="button secondary" onClick={onClose}>关闭</button></div></Modal>
    <TaskDetail jobID={job} onClose={() => setJob(null)}/>
  </>
}

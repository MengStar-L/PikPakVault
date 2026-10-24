import { useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'motion/react'
import { toast } from 'sonner'
import { ArrowDownToLine, ArrowUpRight, CheckCircle2, Cloud, LoaderCircle, Pause, Play, RefreshCw, RotateCcw, Search, ShieldCheck, X } from 'lucide-react'
import { api, date } from './api'
import type { Job } from './api'
import { Confirm, EmptyState, ErrorState, IconButton, Loading, Status } from './components'
import { useJobRetry } from './useJobRetry'
import { JobLane } from './JobLane'
import { TaskDetail } from './TaskDetail'
import './tasks.css'

type JobsResult = { jobs: Job[]; retry_summary?: { account_id: string; count: number } }
type BatchResult = { account_id: string; restarted: number; skipped: number; skipped_reasons: Record<string, number>; message: string }
const running = ['queued', 'running', 'waiting', 'retry']
const needsAttention = ['failed', 'partial', 'attention', 'paused']
const skipReasons: Record<string, string> = {
  duplicate_check: '相同检查已排队，保留其余历史记录',
  executing: '原任务仍在保存进度，请稍后重试',
  account_unavailable: '账号暂不可用，请先处理账号异常',
  invalid_data: '任务记录异常，请查看任务详情',
}

function batchMessage(result: BatchResult) {
  const message = result.message || `已将 ${result.restarted} 项原任务重新排队。`
  if (!result.skipped) return message
  const reasons = Object.entries(result.skipped_reasons || {}).filter(([, count]) => count > 0)
    .map(([reason, count]) => `${count} 项${skipReasons[reason] || '暂不可重试，请查看任务详情'}`)
  return `${message}。跳过 ${result.skipped} 项${reasons.length ? `：${reasons.join('；')}` : '，请查看任务详情'}。`
}

export function TasksPage() {
  const retry = useJobRetry()
  const client = useQueryClient()
  const [filter, setFilter] = useState('all')
  const [detail, setDetail] = useState<Job | null>(null)
  const [cancel, setCancel] = useState<Job | null>(null)
  const [batchBusy, setBatchBusy] = useState(false)
  const [outcome, setOutcome] = useState<{ accountID: string; message: string; error?: boolean } | null>(null)
  const submitting = useRef(false)
  const q = useQuery({
    queryKey: ['jobs'], queryFn: () => api<JobsResult>('/jobs'),
    // Poll on this page as a fallback for SSE, including queued tasks outside
    // the capped history returned by the list endpoint.
    refetchInterval: 5000,
  })
  const summary = q.data?.retry_summary
  const visibleOutcome = outcome?.accountID === summary?.account_id ? outcome : null
  const jobs = q.data?.jobs.filter(j => filter === 'all'
    || filter === 'running' && running.includes(j.state)
    || filter === 'failed' && needsAttention.includes(j.state)
    || filter === 'completed' && j.state === 'completed') || []

  const refresh = () => Promise.all(['jobs', 'summary', 'files', 'job-detail', 'accounts'].map(key => client.invalidateQueries({ queryKey: [key] })))

  async function retryAll() {
    if (submitting.current || retry.busyJob || !summary?.account_id || !summary.count) return
    const accountID = summary.account_id
    submitting.current = true
    setBatchBusy(true)
    setOutcome(null)
    try {
      const result = await api<BatchResult>('/jobs/retry-all', { account_id: accountID })
      const message = batchMessage(result)
      setOutcome({ accountID, message })
      await refresh()
      const stillCurrent = client.getQueryData<JobsResult>(['jobs'])?.retry_summary?.account_id === accountID
      if (!stillCurrent) toast.info(`原账号的批量操作已提交，重新排队 ${result.restarted} 项任务；当前账号未受影响`)
      else if (result.restarted) toast.success(`已重新排队 ${result.restarted} 项任务，进度将持续更新`)
      else toast.info(message)
    } catch (e) {
      const message = (e as Error).message
      setOutcome({ accountID, message, error: true })
      toast.error(message)
      await refresh()
    } finally {
      submitting.current = false
      setBatchBusy(false)
    }
  }

  async function control(job: Job, action: string) {
    if (submitting.current) return
    if (action === 'retry') { await retry.retry(job.id); return }
    try {
      await api(`/jobs/${job.id}/${action}`, {})
      void client.invalidateQueries({ queryKey: ['jobs'] })
      void client.invalidateQueries({ queryKey: ['summary'] })
      toast.success(action === 'pause' ? '任务已暂停' : '任务已取消')
      setCancel(null)
    } catch (e) { toast.error((e as Error).message) }
  }

  return <div className="scroll-page tasks-page">
    <div className="page-heading">
      <div className="heading-copy"><div className="eyebrow">A LITTLE WORK IN THE CLOUD</div><h1>传输任务<span className="heading-dot"/></h1><p>每一次保存和恢复，都有清晰的进度。</p></div>
      <div className="heading-actions">
        <button className="button secondary" disabled={batchBusy} onClick={() => void q.refetch()}><RefreshCw size={16} className={q.isFetching ? 'spin' : ''}/>刷新</button>
        <button className="button primary task-retry-all" aria-label="一键全部重新开始" aria-describedby="task-retry-scope" disabled={batchBusy || !!retry.busyJob || q.isLoading || !!q.error || !summary?.count || !summary.account_id} onClick={() => void retryAll()}>
          {batchBusy ? <LoaderCircle size={16} className="spin"/> : <RotateCcw size={16}/>}
          {batchBusy ? '正在重新排队…' : '一键全部重新开始'}
          {!batchBusy && !!summary?.count && <span className="task-retry-count">{summary.count}</span>}
        </button>
      </div>
    </div>
    <p className="task-retry-scope" id="task-retry-scope">继续当前账号失败、暂停及需要处理的任务，保留已有下载和上传进度。</p>
    {visibleOutcome && <div className={`task-batch-result${visibleOutcome.error ? ' error' : ''}`} role={visibleOutcome.error ? 'alert' : 'status'}>{visibleOutcome.message}</div>}
    <div className="page-tabs">{Object.entries({ all: '全部任务', running: '进行中', completed: '已完成', failed: '需要处理' }).map(([key, label]) => <button className={filter === key ? 'active' : ''} key={key} onClick={() => setFilter(key)}>{label}{key === 'running' && <span>{q.data?.jobs.filter(j => running.includes(j.state)).length || 0}</span>}</button>)}</div>
    {q.isLoading ? <Loading/> : q.error ? <ErrorState error={q.error}/> : !jobs.length ? <EmptyState icon={ArrowDownToLine} title="这里暂时没有任务" description="添加资源或开始恢复后，可以在这里查看进度。"/> : <div className="task-list">{jobs.map(job => <article className={`task-card ${job.state}`} key={job.id}>
      <div className={`task-icon ${job.kind}`}>{job.kind === 'recover' ? <ShieldCheck size={23}/> : job.kind === 'scan' ? <Search size={22}/> : job.kind === 'verify' || job.kind === 'root' ? <Cloud size={23}/> : job.state === 'completed' ? <CheckCircle2 size={23}/> : <ArrowDownToLine size={23}/>}</div>
      <div className="task-main">
        <div className="task-title-row"><button className="task-title" onClick={() => setDetail(job)}>{job.title}</button><Status state={job.state}/></div>
        <div className="task-progress"><motion.i initial={false} animate={{ width: `${job.progress}%` }} transition={{ duration: .5 }}/></div>
        <div className="task-meta"><JobLane job={job}/><span>{date(job.created)}<i/>{job.progress}%</span>{job.attempts > 0 && <span>已重试 {job.attempts} 次</span>}{job.next_run > 0 && ['retry', 'waiting'].includes(job.state) && <span>下次核对 {new Date(job.next_run * 1000).toLocaleTimeString('zh-CN')}</span>}</div>
        {job.message && <p className="task-message">{job.message}</p>}
      </div>
      <div className="task-actions">
        {running.includes(job.state) ? <IconButton label={`暂停 ${job.title}`} disabled={batchBusy} onClick={() => void control(job, 'pause')}><Pause size={17}/></IconButton> : job.state !== 'completed' && job.state !== 'cancelled' && <IconButton label={`重试 ${job.title}`} disabled={batchBusy || !!retry.busyJob} onClick={() => void control(job, 'retry')}>{retry.busyJob === job.id ? <LoaderCircle size={17} className="spin"/> : <Play size={17}/>}</IconButton>}
        {job.state !== 'completed' && job.state !== 'cancelled' && <IconButton label={`取消 ${job.title}`} disabled={batchBusy} onClick={() => setCancel(job)}><X size={17}/></IconButton>}
        <IconButton label={`查看 ${job.title} 的详情`} disabled={batchBusy} onClick={() => setDetail(job)}><ArrowUpRight size={18}/></IconButton>
      </div>
    </article>)}</div>}
    <TaskDetail job={detail} onClose={() => setDetail(null)}/>
    <Confirm open={!!cancel} onClose={() => setCancel(null)} title="取消此任务？" label="取消任务" confirm={() => cancel && control(cancel, 'cancel')}><p>本地来源和已经保存的文件会保留。云端已接受的传输任务仍可能继续，请在需要时重新检查。</p></Confirm>
  </div>
}

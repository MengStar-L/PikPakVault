import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { AlertCircle, ArrowLeft, ArrowUpRight, CheckCircle2, LoaderCircle, RefreshCw, ShieldCheck } from 'lucide-react'
import { api } from './api'
import type { Account } from './api'
import { Logo } from './components'
import { completeAccountVerification } from './verificationReturn'
import './account-verification.css'

type Challenge = { url: string; expires_at: number }
const officialHosts = new Set(['user.mypikpak.com', 'user.mypikpak.net', 'mypikpak.com', 'mypikpak.net'])

function validatedChallenge(value: Challenge): Challenge {
  const url = new URL(value.url)
  if (url.protocol !== 'https:' || !officialHosts.has(url.hostname) || url.username || url.password || url.port ||
    !Number.isFinite(value.expires_at) || value.expires_at * 1000 <= Date.now()) {
    throw new Error('未取得有效的 PikPak 验证链接，请稍后重新获取。')
  }
  return value
}

export function AccountVerification({ account, onEdit }: { account: Account; onEdit: () => void }) {
  const client = useQueryClient()
  const [challenge, setChallenge] = useState<Challenge | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [openedAt, setOpenedAt] = useState(0)
  const [clock, setClock] = useState(Date.now())
  const submitting = useRef(false)
  const mounted = useRef(true)
  useEffect(() => { mounted.current = true; return () => { mounted.current = false } }, [])
  useEffect(() => {
    if (!challenge) return
    const timer = window.setInterval(() => setClock(Date.now()), 1000)
    const focus = () => setClock(Date.now())
    window.addEventListener('focus', focus)
    return () => { clearInterval(timer); window.removeEventListener('focus', focus) }
  }, [challenge])

  async function prepare() {
    if (submitting.current || account.verification_pending) return
    submitting.current = true
    setBusy(true); setError(''); setChallenge(null); setOpenedAt(0)
    try {
      const result = validatedChallenge(await api<Challenge>(`/accounts/${account.id}/verification`, {
        callback_url: new URL('/account-verification', window.location.origin).href,
      }))
      if (mounted.current) { setChallenge(result); setClock(Date.now()) }
    } catch (reason) {
      if (mounted.current) setError((reason as Error).message)
    } finally {
      submitting.current = false
      if (mounted.current) setBusy(false)
      client.invalidateQueries({ queryKey: ['accounts'] })
    }
  }

  const expired = !!challenge && clock >= challenge.expires_at * 1000
  const waiting = openedAt > 0 && clock - openedAt >= 30000
  const remaining = challenge ? Math.max(0, Math.ceil((challenge.expires_at * 1000 - clock) / 60000)) : 0
  return <section className="account-verification" aria-label={`安全验证 ${account.name}`}>
    {!challenge && <button type="button" className="text-button" disabled={busy || account.verification_pending} onClick={prepare}>
      {busy ? <LoaderCircle size={15} className="spin" /> : <ShieldCheck size={15} />}
      {busy ? '正在准备验证…' : error ? '重新获取验证链接' : '开始安全验证'}
    </button>}
    {error && <p className="verification-error" role="alert"><AlertCircle size={14}/>{error}</p>}
    {challenge && <>
      <div className="verification-next" role="status">
        <strong>{expired ? '验证链接已过期' : openedAt ? '等待你完成验证' : '验证链接已准备好'}</strong>
        <span>{expired ? '重新获取后，请使用新的页面完成验证。' : `请在 ${remaining} 分钟内打开；完成后会自动返回核验账号。`}</span>
      </div>
      <div className="verification-actions">
        {!expired && <a className="button primary small" href={challenge.url} target="_blank" rel="noopener noreferrer nofollow"
          onClick={event => {
            if (Date.now() >= challenge.expires_at * 1000) { event.preventDefault(); setClock(Date.now()); return }
            setOpenedAt(Date.now())
          }}>打开 PikPak 验证<ArrowUpRight size={14}/></a>}
        <button type="button" className="text-button" disabled={busy || account.verification_pending} onClick={prepare}><RefreshCw size={13}/>重新获取</button>
      </div>
      {(waiting || expired) && <p className="verification-help">页面一直显示 Please wait 或无法加载时，请关闭旧页面后重新获取。仍无法验证，可在官方客户端确认账号状态，再<button type="button" className="text-button" onClick={onEdit}>更新凭据</button>。</p>}
    </>}
  </section>
}

export function AccountVerificationReturn({ csrf }: { csrf: string }) {
  const client = useQueryClient()
  const [status, setStatus] = useState<'pending' | 'success' | 'error'>('pending')
  const [error, setError] = useState('')
  useEffect(() => {
    let active = true
    completeAccountVerification(csrf).then(() => {
      client.invalidateQueries({ queryKey: ['accounts'] })
      client.invalidateQueries({ queryKey: ['jobs'] })
      client.invalidateQueries({ queryKey: ['summary'] })
      if (active) setStatus('success')
    }).catch(reason => {
      if (active) { setError((reason as Error).message); setStatus('error') }
    })
    return () => { active = false }
  }, [client, csrf])
  return <main className="verification-return-page"><Logo/><section className={`verification-return-card ${status}`} aria-live="polite">
    <span className="verification-return-icon">{status === 'pending' ? <LoaderCircle size={30} className="spin"/> : status === 'success' ? <CheckCircle2 size={30}/> : <AlertCircle size={30}/>}</span>
    <h1>{status === 'pending' ? '正在核验账号' : status === 'success' ? '安全验证已完成' : '安全验证尚未完成'}</h1>
    <p>{status === 'pending' ? '已收到 PikPak 返回信息，正在确认账号身份和可用状态。' : status === 'success' ? '账号连接已核验。已暂停的任务仍保留，可在传输任务中继续或重试。' : error}</p>
    {status === 'error' && <small>没有解除账号限制，也没有重新创建传输任务。请回到账号管理重新开始验证。</small>}
    <div className="verification-return-actions"><Link className="button secondary" to="/accounts"><ArrowLeft size={15}/>返回账号管理</Link>{status === 'success' && <Link className="button primary" to="/tasks">查看传输任务<ArrowUpRight size={15}/></Link>}</div>
  </section></main>
}

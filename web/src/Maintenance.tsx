import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowUpRight, CheckCircle2, Download, FolderInput, Github, LoaderCircle, RefreshCw, ShieldCheck, Upload } from 'lucide-react'
import { toast } from 'sonner'
import { api, csrf, setCsrf } from './api'
import { Modal } from './components'
import './maintenance.css'

type Preview = { id: string; accounts: number; files: number; sources: number; jobs: number }
export function BackupImport({ setup = false, onImported }: { setup?: boolean; onImported?: () => void }) {
  const client = useQueryClient()
  const [open, setOpen] = useState(false)
  const [file, setFile] = useState<File | null>(null)
  const [token, setToken] = useState('')
  const [password, setPassword] = useState('')
  const [preview, setPreview] = useState<Preview | null>(null)
  const [confirmed, setConfirmed] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const prefix = setup ? '/auth/import' : '/data/import'
  function close() { if (!busy) { setOpen(false); setPreview(null); setConfirmed(false); setFile(null); setPassword(''); setToken(''); setError('') } }
  async function request(action: string, body: BodyInit) {
    const r = await fetch(`/api/v1${prefix}/${action}`, { method: 'POST', headers: { 'X-CSRF-Token': csrf, 'X-Setup-Token': token, 'Content-Type': action === 'preview' ? 'application/zip' : 'application/json' }, body })
    const value = await r.json()
    if (!r.ok) throw new Error(value.error || '导入失败，请稍后重试')
    return value
  }
  async function inspect() {
    if (!file) return
    setBusy(true); setError('')
    try { setPreview(await request('preview', file)); setConfirmed(false) } catch (e) { setError((e as Error).message) } finally { setBusy(false) }
  }
  async function commit() {
    setBusy(true); setError('')
    try {
      await request('commit', JSON.stringify({ id: preview?.id, confirm: confirmed, current_password: password }))
      setCsrf(''); client.clear(); client.setQueryData(['auth'], { configured: true, authenticated: false }); setOpen(false)
      toast.success('数据已导入，请使用备份中的管理员密码登录'); onImported?.()
    } catch (e) { setError((e as Error).message) } finally { setBusy(false) }
  }
  return <>
    <button type="button" className={`button ${setup ? 'secondary login-submit' : 'secondary'}`} onClick={() => setOpen(true)}><FolderInput size={17} />{setup ? '从完整备份导入' : '导入完整备份'}</button>
    <Modal open={open} onClose={close} title={setup ? '从备份开启资源库' : '导入完整备份'} subtitle="迁移账号、目录、来源与使用记录。" className="import-backup-modal">
      <div className="modal-body maintenance-body">
        <div className="maintenance-note"><ShieldCheck size={20} /><p>完整备份包含账号认证信息及解密密钥。请仅导入你信任的备份，并妥善保存 ZIP 文件。备份不含云端文件内容。</p></div>
        {setup && <label>初始化代码<input autoComplete="off" value={token} disabled={!!preview || busy} onChange={e => setToken(e.target.value)} placeholder="从本机启动日志中获取" /></label>}
        {!preview ? <label className="backup-drop"><Upload size={27} /><strong>{file?.name || '选择完整备份 ZIP'}</strong><span>先检查内容，再确认导入 · 最大 512 MiB</span><input aria-label="完整备份文件" type="file" accept=".zip,application/zip" disabled={busy} onChange={e => { const f = e.target.files?.[0] || null; setFile(f && f.size <= 512 * 1024 ** 2 ? f : null); setError(f && f.size > 512 * 1024 ** 2 ? '备份超过 512 MiB，请使用命令行恢复。' : '') }} /></label> : <>
          <div className="backup-preview"><span><strong>{preview.accounts}</strong>账号</span><span><strong>{preview.files}</strong>文件与目录</span><span><strong>{preview.sources}</strong>来源批次</span><span><strong>{preview.jobs}</strong>任务记录</span></div>
          <p className="maintenance-description">账号登录信息、云端映射、路径、收藏、播放进度、任务、设置和操作日志都会恢复。导入后使用<strong>备份中的管理员密码</strong>登录，浏览器会话全部退出；未完成任务暂停，等待你核对账号。</p>
          {!setup && <label>当前管理员密码<input type="password" autoComplete="current-password" disabled={busy} value={password} onChange={e => setPassword(e.target.value)} /></label>}
          <label className="maintenance-consent"><input type="checkbox" checked={confirmed} disabled={busy} onChange={e => setConfirmed(e.target.checked)} /><span>{setup ? '确认使用这份备份初始化资源库。' : '确认替换当前全部数据。程序会先在服务器保存一份导入前备份。'}</span></label>
        </>}
        {error && <div className="inline-error" role="alert">{error}</div>}
      </div>
      <div className="modal-actions"><button type="button" className="button secondary" disabled={busy} onClick={close}>取消</button><button type="button" className="button primary" disabled={busy || (preview ? !confirmed || (!setup && !password) : !file || (setup && !token))} onClick={preview ? commit : inspect}>{busy ? <LoaderCircle size={16} className="spin" /> : <FolderInput size={16} />}{busy ? '正在处理…' : preview ? '确认导入全部数据' : '校验并预览备份'}</button></div>
    </Modal>
  </>
}

type UpdateInfo = { current: string; repository: string; checked: number; available: boolean; can_install: boolean; reason: string; error: string; release: { tag_name: string; html_url: string; body: string; assets: { name: string; browser_download_url: string }[] } | null; status: { phase: string; message: string; tag: string } }
const activePhases = ['queued', 'downloading', 'stopping', 'installing', 'verifying', 'rolling_back']
export function UpdatePanel() {
  const client = useQueryClient()
  const query = useQuery({ queryKey: ['updates'], queryFn: () => api<UpdateInfo>('/updates'), refetchInterval: q => activePhases.includes(q.state.data?.status.phase || '') ? 2000 : 30000, retry: false })
  const [checking, setChecking] = useState(false)
  const [installing, setInstalling] = useState(false)
  const [confirm, setConfirm] = useState(false)
  const data = query.data
  const pending = activePhases.includes(data?.status.phase || '')
  async function check() { setChecking(true); try { client.setQueryData(['updates'], await api<UpdateInfo>('/updates/check', {})) } catch (e) { toast.error((e as Error).message) } finally { setChecking(false) } }
  async function install() { setInstalling(true); try { client.setQueryData(['updates'], await api<UpdateInfo>('/updates/install', { tag: data?.release?.tag_name, confirm: true })); setConfirm(false) } catch (e) { toast.error((e as Error).message) } finally { setInstalling(false) } }
  return <section className="settings-section update-section"><div className="settings-title"><span className="settings-icon blue"><Github size={22} /></span><div><h2>程序更新</h2><p>从 GitHub 获取正式版本，由你确认后安装。</p></div></div><div className="update-body">
    <div className="update-version"><span className="update-version-icon"><RefreshCw size={24} className={pending ? 'spin' : ''} /></span><div><strong>{pending ? '正在更新程序' : data?.available ? `发现新版本 ${data.release?.tag_name}` : `PikPak Vault ${data?.current ? 'v' + data.current : ''}`}</strong><p>{data?.checked ? `最近检查：${new Date(data.checked * 1000).toLocaleString('zh-CN')}` : '检查已发布的 Linux 安装包'}</p></div><button type="button" className="button secondary" disabled={checking || pending || !data} onClick={check}><RefreshCw size={15} className={checking ? 'spin' : ''} />{checking ? '检查中…' : '检查更新'}</button></div>
    {data?.error && <div className="inline-error" role="alert">{data.error}</div>}
    {query.error && <p role="status">{pending ? '服务正在重启，页面会自动重新连接…' : '暂时无法获取状态，正在重新连接…'}</p>}
    {data?.status.phase && <div className={`update-progress ${pending ? 'active' : ''}`} role="status">{pending ? <LoaderCircle className="spin" size={17} /> : <CheckCircle2 size={17} />}<span>{data.status.message || data.status.phase}</span></div>}
    {data?.release && <div className="release-preview"><a href={data.release.html_url} target="_blank" rel="noreferrer">{data.release.tag_name} · 查看发布说明<ArrowUpRight size={15} /></a>{data.release.body && <p>{data.release.body}</p>}</div>}
    <div className="update-foot"><p>{data?.reason || '安装前自动备份程序与全部数据；启动检查失败时回滚。服务重启期间页面会自动重连。'}</p>{data?.available && (data.can_install ? <button type="button" className="button primary" disabled={pending} onClick={() => setConfirm(true)}><Download size={16} />安装更新</button> : <a className="button secondary" target="_blank" rel="noreferrer" href={data.release?.html_url}><Download size={16} />下载安装包</a>)}</div>
    <a className="update-repository" target="_blank" rel="noreferrer" href={`https://github.com/${data?.repository || 'MengStar-L/PikPakVault'}`}><Github size={14} />{data?.repository || 'MengStar-L/PikPakVault'}<ArrowUpRight size={13} /></a>
  </div><Modal open={confirm} onClose={() => !installing && setConfirm(false)} title={`安装 ${data?.release?.tag_name || '更新'}`}><div className="modal-body maintenance-body"><p>程序将下载并校验安装包，停止服务、备份数据并启动新版本。未完成的传输任务在重启后核对状态继续执行，网页会短暂断开。</p><p>如果新版本启动检查失败，将自动恢复旧程序和更新前的数据。</p></div><div className="modal-actions"><button type="button" className="button secondary" disabled={installing} onClick={() => setConfirm(false)}>取消</button><button type="button" className="button primary" disabled={installing} onClick={install}>{installing && <LoaderCircle className="spin" size={16} />}确认更新并重启</button></div></Modal></section>
}

export function ImportReviewBanner() {
  const client = useQueryClient()
  const query = useQuery({ queryKey: ['settings'], queryFn: () => api<{ import_review_required: boolean }>('/settings') })
  const [busy, setBusy] = useState(false)
  if (!query.data?.import_review_required) return null
  async function resume() { setBusy(true); try { await api('/data/resume', {}); await client.invalidateQueries({ queryKey: ['settings'] }); toast.success('后台检查已恢复，暂停的任务可在传输任务中手动继续') } catch (e) { toast.error((e as Error).message) } finally { setBusy(false) } }
  return <div className="import-review" role="status"><ShieldCheck size={20} /><p><strong>备份已导入，后台任务暂时暂停</strong><span>请先核对账号、保存路径和任务。确认后恢复后台检查，传输任务由你手动继续。</span></p><a href="/accounts" className="text-button">核对账号</a><button type="button" className="button secondary small" disabled={busy} onClick={resume}>已核对，恢复后台检查</button></div>
}

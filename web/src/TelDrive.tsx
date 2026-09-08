import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { motion, useReducedMotion } from 'motion/react'
import { ArrowRight, Check, ChevronRight, CloudUpload, Folder, FolderSearch, KeyRound, LoaderCircle, Plus, RefreshCw, Settings2, Timer, Unplug } from 'lucide-react'
import { toast } from 'sonner'
import { api, bytes } from './api'
import type { Account, Job } from './api'
import { Confirm, EmptyState, ErrorState, FolderPicker, Loading, Modal, Status } from './components'
import './teldrive.css'

type Monitor = { id:string; name:string; base_url:string; folder_id:string; folder_path:string; parent_id:string; target_path:string; auto_minutes:number; last_run:number; last_job:Job|null }
type RemoteFile = { id:string; name:string; type:string; size:number }
type RemotePage = { items:RemoteFile[]; meta:{ count:number; currentPage:number; totalPages:number } }
const intervals = [0,5,15,30,60,180,360,1440]
const intervalLabel = (value:number) => value === 0 ? '仅手动同步' : value < 60 ? `每 ${value} 分钟` : `每 ${value / 60} 小时`

export default function TelDrivePage() {
  const client = useQueryClient()
  const q = useQuery({ queryKey:['teldrive'], queryFn:()=>api<{monitors:Monitor[];active_account:string}>('/teldrive'), refetchInterval:5000 })
  const accounts = useQuery({queryKey:['accounts'],queryFn:()=>api<{accounts:Account[];active_id:string}>('/accounts')})
  const active = accounts.data?.accounts.find(a=>a.id===q.data?.active_account)
  const [editing,setEditing] = useState<Monitor|'new'|null>(null)
  const [starting,setStarting] = useState('')
  const reduce = useReducedMotion()
	const cache=useQuery({queryKey:['teldrive-cache'],queryFn:()=>api<{bytes:number;files:number;reclaimable:number;aria2_available:boolean}>('/teldrive/cache'),refetchInterval:5000})
	const [clearing,setClearing]=useState(false)
	const [confirmClear,setConfirmClear]=useState(false)
	async function clearCache(){setClearing(true);try{await api('/teldrive/cache/clear',{});await cache.refetch();setConfirmClear(false);toast.success('闲置下载缓存已清理，重试时将重新下载')}catch(e){toast.error((e as Error).message)}finally{setClearing(false)}}
  async function sync(m:Monitor) {
    setStarting(m.id)
    try { await api(`/teldrive/${m.id}/sync`,{}); toast.success('扫描已排队，新文件将显示在目标文件夹中'); client.invalidateQueries({queryKey:['teldrive']}); client.invalidateQueries({queryKey:['jobs']}) }
    catch(e) { toast.error((e as Error).message) } finally { setStarting('') }
  }
  return <div className="scroll-page td-page">
    <div className="page-heading"><div><span className="eyebrow">FROM TELEGRAM TO YOUR COLLECTION</span><h1>TelDrive 同步</h1><p>把 Telegram 中的收藏，上传到你的 PikPak 资源库。</p></div><button className="button primary" onClick={()=>setEditing('new')}><Plus size={17}/>添加监控</button></div>
    <div className="td-overview"><span className="td-symbol"><CloudUpload size={29}/></span><div><strong>文件夹到文件夹，收藏自动归位。</strong><p>保留子目录，跳过已保存的文件。按需手动同步，也可以开启定时检查。</p></div><span className="td-account"><small>当前上传账号</small><strong>{active?.name || '尚未连接'}</strong></span></div>
    {!active && <div className="warning-box"><Unplug size={18}/><span>连接并验证 PikPak 账号后即可开始同步。</span><Link to="/accounts">连接账号</Link></div>}
    {q.isLoading ? <Loading/> : q.error ? <ErrorState error={q.error} retry={()=>q.refetch()}/> : !q.data?.monitors.length ? <EmptyState icon={FolderSearch} title="选择一个值得收藏的文件夹" description="连接你的 TelDrive，指定来源和保存位置。默认只在你点击同步时运行。"/> :
      <div className="td-monitors">{q.data.monitors.map((m,index)=><motion.article className="td-card" key={m.id} initial={reduce?false:{opacity:0,y:12}} animate={{opacity:1,y:0}} transition={{duration:.25,delay:Math.min(index*.04,.2)}}>
        <div className="td-card-heading"><span className="td-folder-icon"><Folder size={23}/></span><h2>{m.name}</h2><span className={`td-mode ${m.auto_minutes?'automatic':''}`}><Timer size={13}/>{intervalLabel(m.auto_minutes)}</span></div>
        <div className="td-paths"><div><small>TelDrive 来源</small><strong title={m.folder_path||'/'}>{m.folder_path||'/'}</strong><span>{m.base_url}</span></div><ArrowRight size={18}/><div><small>PikPak 资源库内路径</small><strong>{m.target_path||'我的文件'}</strong><span>位于设置中的云端保存目录下</span></div></div>
        <div className="td-last-run">{m.last_job?<><Status state={m.last_job.state}/><span>{m.last_job.message||'等待扫描目录'}</span></>:<span>尚未扫描 · 准备好后开始第一次同步</span>}</div>
        <footer><Link className="text-button" to="/tasks">查看传输任务<ChevronRight size={14}/></Link><button className="button secondary small" onClick={()=>setEditing(m)}><Settings2 size={15}/>配置</button><button className="button primary small" disabled={!!starting||active?.status!=='ready'} onClick={()=>sync(m)}>{starting===m.id?<LoaderCircle className="spin" size={15}/>:<RefreshCw size={15}/>}立即同步</button></footer>
      </motion.article>)}</div>}
    <div className="td-cache"><div><strong>专用 aria2 · 下载缓存</strong><p>{cache.data?`${cache.data.files} 个任务 · 已占用 ${bytes(cache.data.bytes)} · 可清理 ${bytes(cache.data.reclaimable)}`:'正在读取缓存状态'}</p>{cache.data&&!cache.data.aria2_available&&<p>首次同步时将自动下载并校验专用 aria2 1.37.0，无需系统安装。</p>}{cache.error&&<p className="danger-text">{(cache.error as Error).message}</p>}</div><button className="button secondary small" disabled={!cache.data?.reclaimable||clearing} onClick={()=>setConfirmClear(true)}>清理闲置缓存</button></div>
    <div className="td-help"><CloudUpload size={18}/><p>先由 aria2 下载到服务器缓存，再上传至 PikPak；传输会消耗服务器磁盘及上下行流量。上传核验成功后自动清理缓存，暂停或失败时保留以便续传。源端删除不会删除云端副本。切换账号后，后续扫描使用新账号，已有任务仍属于原账号。</p></div>
    <Confirm open={confirmClear} onClose={()=>setConfirmClear(false)} title="清理闲置下载缓存？" label="清理缓存" busy={clearing} confirm={clearCache}><p>清理已停止任务的临时文件及下载进度，保留资源记录、来源和上传会话。之后重试会重新下载；运行或排队中的任务不会被清理。</p></Confirm>
    {editing && <MonitorDialog monitor={editing==='new'?null:editing} onClose={()=>setEditing(null)}/>}
  </div>
}

function MonitorDialog({monitor,onClose}:{monitor:Monitor|null;onClose:()=>void}) {
  const client=useQueryClient()
  const [name,setName]=useState(monitor?.name||'Telegram 收藏')
  const [base,setBase]=useState(monitor?.base_url||'')
  const [token,setToken]=useState('')
  const [minutes,setMinutes]=useState(monitor?.auto_minutes||0)
  const [source,setSource]=useState<{id:string;path:string}|null>(monitor?{id:monitor.folder_id,path:monitor.folder_path}:null)
  const [target,setTarget]=useState(monitor?.parent_id||'root')
  const [targetName,setTargetName]=useState(monitor?.target_path||'我的文件')
  const [targetOpen,setTargetOpen]=useState(false)
  const [trail,setTrail]=useState<{id:string;name:string}[]>([])
  const [listing,setListing]=useState<RemotePage|null>(null)
  const [browsing,setBrowsing]=useState(false)
  const [saving,setSaving]=useState(false)
  const [error,setError]=useState('')
  async function browse(next=trail,page=1) {
    setBrowsing(true);setError('')
    try { const result=await api<RemotePage>('/teldrive/browse',{id:monitor?.id||'',base_url:base,token,folder_id:next.at(-1)?.id||'',page});setListing(result);setTrail(next) }
    catch(e) {setError((e as Error).message);setListing(null)} finally {setBrowsing(false)}
  }
  async function save() {
    if(!source)return
    setSaving(true);setError('')
    try { await api(monitor?`/teldrive/${monitor.id}`:'/teldrive',{name,base_url:base,token,folder_id:source.id,folder_path:source.path,parent_id:target,auto_minutes:minutes},monitor?'PATCH':'POST');client.invalidateQueries({queryKey:['teldrive']});toast.success(minutes?'监控已保存，定时同步已开启':'监控已保存，可随时手动同步');onClose() }
    catch(e) {setError((e as Error).message)} finally {setSaving(false)}
  }
  const currentPath='/'+trail.map(t=>t.name).join('/')
  return <Modal open onClose={onClose} title={monitor?'配置 TelDrive 监控':'连接 TelDrive 文件夹'} subtitle="指定来源、保存位置与同步方式。" wide>
    <div className="modal-body td-editor">
      <label>监控名称<input value={name} onChange={e=>setName(e.target.value)} maxLength={200} placeholder="例如：Telegram 电影收藏"/></label>
      <label>TelDrive 站点地址<input type="url" value={base} disabled={!!monitor} onChange={e=>{setBase(e.target.value);setSource(null);setListing(null);setTrail([])}} placeholder="https://teldrive.example.com"/></label>
      <label><span><KeyRound size={14}/> TelDrive access_token</span><input type="password" autoComplete="new-password" value={token} onChange={e=>{setToken(e.target.value);setListing(null);if(!monitor)setSource(null)}} placeholder={monitor?'留空保留现有认证信息':'填写已登录 TelDrive 的 access_token 值'}/><small>在 TelDrive 浏览器开发者工具 → 应用 / 存储 → Cookie 中查看 access_token。认证过期时在这里更新。</small></label>
      {!monitor&&<button className="button secondary td-browse-button" disabled={browsing||!base.trim()||!token.trim()} onClick={()=>browse([],1)}>{browsing?<LoaderCircle className="spin" size={16}/>:<FolderSearch size={16}/>}连接并选择文件夹</button>}
      {listing&&!monitor&&<div className="td-picker" aria-busy={browsing}>
        <div className="td-crumb"><button disabled={browsing} onClick={()=>browse([],1)}>TelDrive</button>{trail.map((t,i)=><span key={t.id}><ChevronRight size={13}/><button disabled={browsing} onClick={()=>browse(trail.slice(0,i+1),1)}>{t.name}</button></span>)}</div>
        <button className="td-select-current" disabled={browsing} onClick={()=>setSource({id:trail.at(-1)?.id||'',path:currentPath})}><Folder size={18}/>选择当前文件夹{source?.id===(trail.at(-1)?.id||'')&&<Check size={17}/>}</button>
        <div className="td-picker-rows">{listing.items.map(f=>f.type==='folder'?<button key={f.id} disabled={browsing} onClick={()=>browse([...trail,{id:f.id,name:f.name}])}><Folder size={18}/><span>{f.name}</span><ChevronRight size={16}/></button>:<div key={f.id}><span>{f.name}</span><small>{bytes(f.size)}</small></div>)}{!listing.items.length&&<p className="muted">这是一个空文件夹，新增的文件也可以被同步。</p>}</div>
        {listing.meta.totalPages>1&&<div className="pagination"><button disabled={browsing||listing.meta.currentPage<=1} onClick={()=>browse(trail,listing.meta.currentPage-1)}>上一页</button><span>{listing.meta.currentPage} / {listing.meta.totalPages}</span><button disabled={browsing||listing.meta.currentPage>=listing.meta.totalPages} onClick={()=>browse(trail,listing.meta.currentPage+1)}>下一页</button></div>}
      </div>}
      {source&&<div className="td-selection"><small>监控此目录及其全部子目录</small><strong><Folder size={17}/>{source.path||'/'}</strong></div>}
      <div className="td-destination"><span>保存到资源库</span><button className="text-button" disabled={!!monitor} onClick={()=>setTargetOpen(!targetOpen)}><Folder size={16}/>{targetName}<ChevronRight size={15}/></button></div>
      {targetOpen&&<FolderPicker value={target} onChange={(id,label)=>{setTarget(id);setTargetName(label);setTargetOpen(false)}}/>}
      <label className="td-interval">同步方式<select aria-label="同步方式" value={minutes} onChange={e=>setMinutes(Number(e.target.value))}>{[...new Set([...intervals,minutes])].sort((a,b)=>a-b).map(v=><option key={v} value={v}>{intervalLabel(v)}</option>)}</select></label>
      <p className="td-editor-note">同步新增文件及当前账号缺少的副本，不覆盖已有内容。关闭定时同步不会取消已排队的任务，可以在传输任务中暂停或取消。来源和目标保存后固定；更换目录请新建监控。</p>
      {error&&<div className="inline-error" role="alert">{error}</div>}
    </div>
    <div className="modal-actions"><button className="button secondary" onClick={onClose}>取消</button><button className="button primary" disabled={saving||browsing||!source||!name.trim()||!base.trim()} onClick={save}>{saving?<LoaderCircle className="spin" size={16}/>:<Check size={16}/>}保存监控</button></div>
  </Modal>
}

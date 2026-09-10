import { CloudDownload, Info, LoaderCircle, Pause, AlertCircle } from 'lucide-react'
import { bytes, date, fileType } from './api'
import type { FileNode } from './api'
import { FileIcon } from './components'
import { useJobRetry } from './useJobRetry'

const labels:Record<string,string>={queued:'排队中',running:'传输中',waiting:'传输中',retry:'等待重试',paused:'已暂停',attention:'需要处理',failed:'传输失败',partial:'部分完成'}

export function TransferCard({file,mode,onDetails}:{file:FileNode;mode:string;onDetails:()=>void}){
  const retry=useJobRetry();const transfer=retry.job||file.transfer!;const busy=!!retry.busyJob
  const active=['queued','running','waiting','retry'].includes(transfer.state)
  const failed=['failed','attention','partial'].includes(transfer.state)
  const label=labels[transfer.state]||'等待传输';const progress=Math.min(99,Math.max(0,transfer.progress))
  const Icon=failed?AlertCircle:!active?Pause:progress>0?LoaderCircle:CloudDownload
  return <div className={`file-card ${mode} ${fileType(file)} transfer-card ${failed?'transfer-failed':''}`} data-testid="transfer-card" data-job-id={file.transfer!.job_id} role="button" tabIndex={0} aria-label={`${file.name}，${label}`} onClick={onDetails} onKeyDown={e=>{if(e.target===e.currentTarget&&(e.key==='Enter'||e.key===' ')){e.preventDefault();onDetails()}}}>
    <div className={`file-cover ${fileType(file)}`}><div className="cover-pattern"/>{file.kind==='transfer'?<CloudDownload size={mode==='list'?25:48}/>:<FileIcon file={file} size={mode==='list'?25:file.kind==='folder'?55:43}/>}<span className="transfer-cover-hint">{file.kind==='transfer'?'正在获取资源信息':'完成后即可打开'}</span></div>
    <div className="file-info"><span className="file-title" title={file.name}>{file.name}</span><span className={`transfer-label ${transfer.state}`} title={transfer.message||label} role="status">{busy?<LoaderCircle size={12} className="spin"/>:<Icon size={12} className={active&&progress>0?'spin':''}/>} {busy?'正在重试':label}{active&&progress>0&&<strong>{progress}%</strong>}{!active&&<button className="transfer-resume" disabled={busy} onClick={e=>{e.stopPropagation();void retry.retry(file.transfer!.job_id)}}>重试</button>}</span></div>
    {mode==='list'&&<><span className="list-size">{file.kind==='folder'||!file.size?'—':bytes(file.size)}</span><span className="list-date">{date(file.created)}</span></>}
    <div className="file-status"><span className="transfer-note">{active?'云端转存':failed?'点击查看原因':'可继续任务'}</span></div>
    <button className="file-more" aria-label={`${file.name} 的传输详情`} onClick={e=>{e.stopPropagation();onDetails()}}><Info size={16}/></button>
    <div className={`transfer-meter ${active&&!progress?'indeterminate':''}`} role="progressbar" aria-label={`${file.name} 传输进度`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={progress||undefined} aria-valuetext={`${label}${progress?' '+progress+'%':''}`}><i style={{width:`${progress}%`}}/></div>
  </div>
}

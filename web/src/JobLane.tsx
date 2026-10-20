import { Cloud, HardDriveDownload } from 'lucide-react'
import type { Job } from './api'
import './job-lane.css'

export function JobLane({ job }: { job: Job }) {
  const lane = job.lane || (job.kind === 'teldrive_upload' ? 'teldrive' : undefined)
  if (!lane) return null
  return <span className={`job-lane ${lane}`} title={lane === 'teldrive'
    ? '从 TelDrive 下载并上传，与云端转存分别排队'
    : '由 PikPak 处理，与 TelDrive 下载上传分别排队；磁链可能需要等待云端下载'}>
    {lane === 'teldrive' ? <HardDriveDownload size={12} aria-hidden="true"/> : <Cloud size={12} aria-hidden="true"/>}
    {lane === 'teldrive' ? 'TelDrive 下载上传' : '云端转存'}
  </span>
}

export function RecoveryLanesNote() {
  return <p className="recovery-lanes-note">混合来源会自动分为「云端转存」与「TelDrive 下载上传」两路并行，各路同时执行一个任务，可分别暂停或重试。磁链仍可能需要等待 PikPak 下载。</p>
}

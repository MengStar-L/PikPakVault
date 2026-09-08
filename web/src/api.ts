export type FileTransfer = { job_id: string; state: string; progress: number; message: string }
export type FileNode = { folder_previews?: {id:string;url:string}[]; transfer?: FileTransfer; id: string; parent_id: string; name: string; kind: string; size: number; hash: string; mime: string; source_id: string; source_path: string; source_key: string; favorite: boolean; trashed: boolean; created: number; modified: number; opened: number; position: number; revision: number; state: string; remote_id?: string; thumbnail?: string; checked: number }
export type Account = { id: string; name: string; identity: string; root_id: string; status: string; error: string; verification_url: string; quota_limit: number; quota_used: number }
export type Job = { id: string; account_id: string; kind: string; title: string; state: string; progress: number; message: string; attempts: number; next_run: number; created: number }
export type Entry = { id: string; path: string; name: string; kind: string; size: number; hash: string }
export type Source = { id: string; kind: string; link: string; share_id: string; selected: string[]; manifest: Entry[] }
export type FilesResult = { transferring?: number; files: FileNode[]; total: number; page: number; limit: number; breadcrumbs: {id: string; name: string}[] }
export type Summary = { count: number; folders: number; bytes: number; missing: number; tasks: number; active_account: string; last_scan: string; version: string }
export let csrf = ''
export function setCsrf(value: string) { csrf = value }
export class APIError extends Error { constructor(public status: number, message: string, public detail?: Record<string, unknown>) { super(message) } }
export async function api<T = Record<string, unknown>>(path: string, body?: unknown, method?: string): Promise<T> {
  const res = await fetch('/api/v1' + path, { method: method || (body === undefined ? 'GET' : 'POST'), headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: body === undefined ? undefined : JSON.stringify(body) })
  const data = await res.json()
  if (!res.ok) { if (res.status === 401 && !path.startsWith('/auth')) window.dispatchEvent(new Event('vault:unauthorized')); throw new APIError(res.status, data.error || '请求失败，请稍后重试', data) }
  return data as T
}
export async function downloadBackup() { const res = await fetch('/api/v1/backup', { method: 'POST', headers: { 'X-CSRF-Token': csrf } }); if (!res.ok) { const v = await res.json(); throw new Error(v.error) }; const blob = await res.blob(); const url = URL.createObjectURL(blob); const link = document.createElement('a'); link.href = url; link.download = 'pikpak-vault-backup.zip'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000) }
export function bytes(value: number) { if (!value) return '0 B'; const unit = Math.min(Math.floor(Math.log(value) / Math.log(1024)), 4); return `${(value / 1024 ** unit).toFixed(unit ? 1 : 0)} ${['B','KB','MB','GB','TB'][unit]}` }
export function date(value: number) { return value ? new Date(value * 1000).toLocaleDateString('zh-CN', { month: '2-digit', day: '2-digit', year: 'numeric' }) : '—' }
export function fileType(file: FileNode) { if (file.kind === 'folder') return 'folder'; if (file.mime.startsWith('video/') || /\.(mp4|mkv|mov|avi|webm|m4v)$/i.test(file.name)) return 'video'; if (file.mime.startsWith('image/') || /\.(jpg|jpeg|png|gif|webp|avif)$/i.test(file.name)) return 'image'; if (file.mime.startsWith('audio/') || /\.(mp3|flac|wav|m4a|ogg)$/i.test(file.name)) return 'audio'; if (/\.(zip|7z|rar|tar|gz)$/i.test(file.name)) return 'archive'; return 'document' }
export const stateLabels: Record<string, string> = { present: '已保存', missing: '远端缺失', unbound: '待恢复', drift: '路径有变化', conflict: '内容冲突', unknown: '待检查', pending: '处理中', trashed: '回收站', queued: '排队中', running: '进行中', waiting: '云端处理中', retry: '等待重试', paused: '已暂停', attention: '需要处理', partial: '部分完成', failed: '失败', completed: '已完成', cancelled: '已取消', ready: '可用', unverified: '待验证', verification_required: '需要验证' }

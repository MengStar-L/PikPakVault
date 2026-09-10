import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api } from './api'
import type { Job } from './api'

// Observe the first execution after a manual retry, including when SSE is
// reconnecting. A queued response is not evidence that the failure is resolved.
export function useJobRetry() {
  const client = useQueryClient()
  const [pending, setPending] = useState<string|null>(null)
  const [watch, setWatch] = useState<string|null>(null)
  const watching = useRef<string|null>(null)
  const submitting = useRef(false)
  const q = useQuery({queryKey:['job-detail',watch], queryFn:()=>api<{job:Job}>(`/jobs/${watch}`), enabled:!!watch, staleTime:0, refetchInterval:watch?1000:false, retry:false})
  const refresh = () => Promise.all(['files','jobs','summary','job-detail'].map(key=>client.invalidateQueries({queryKey:[key]})))

  useEffect(()=>{
    if (!watch) return
    if (q.error) {
      watching.current=null
      toast.error(`重试已提交，读取进度失败：${(q.error as Error).message}`,{id:`retry-${watch}`})
      setWatch(null)
      return
    }
    const job=q.data?.job
    if (!job || ['queued','running'].includes(job.state)) return
    const options={id:`retry-${watch}`,duration:8000}
    watching.current=null
    if (['attention','failed','partial','paused'].includes(job.state)) toast.error(job.message||'任务仍需处理，请查看任务详情',options)
    else if (job.state==='completed') toast.success('任务已完成',options)
    else toast.info(job.message||'任务已继续，正在等待云端结果',options)
    setWatch(null)
    void Promise.all(['files','jobs','summary'].map(key=>client.invalidateQueries({queryKey:[key]})))
  },[watch,q.data,q.error,client])

  useEffect(()=>()=>{if(watching.current)toast.dismiss(`retry-${watching.current}`)},[])

  async function retry(id:string) {
    if (submitting.current || watching.current) return
    submitting.current=true
    setPending(id)
    try {
      const job=await api<Job>(`/jobs/${id}/retry`,{})
      client.setQueryData(['job-detail',id],(old:Record<string,unknown>|undefined)=>({...old,job}))
      watching.current=id
      setWatch(id)
      toast.loading(job.message||'正在重试原任务…',{id:`retry-${id}`})
      await refresh()
    } catch(e) {
      toast.error((e as Error).message)
    } finally { submitting.current=false; setPending(null) }
  }
  return {retry,busyJob:pending||watch,job:watch?q.data?.job:undefined}
}

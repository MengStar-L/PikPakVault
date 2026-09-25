import { useRef } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import type { InfiniteData } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api } from './api'
import type { FileNode, FilesResult, MediaData } from './api'

export function usePlaybackHistory() {
  const client = useQueryClient()
  const pending = useRef(new Map<string, Promise<boolean>>())

  function recordPlayed(id: string): Promise<boolean> {
    const current = pending.current.get(id)
    if (current) return current
    const request = (async () => {
      try {
        const result = await api<{ played_at: number }>(`/files/${encodeURIComponent(id)}/played`, {})
        const update = (file: FileNode) => file.id === id ? { ...file, played_at: result.played_at } : file
        client.setQueriesData<InfiniteData<FilesResult> | FilesResult>({ queryKey: ['files'] }, data => {
          if (!data) return data
          return 'pages' in data
            ? { ...data, pages: data.pages.map(page => ({ ...page, files: page.files.map(update) })) }
            : { ...data, files: data.files.map(update) }
        })
        client.setQueryData<MediaData>(['media', id], data => data && { ...data, file: update(data.file) })
        client.setQueryData<{ file: FileNode }>(['detail', id], data => data && { ...data, file: update(data.file) })
        // Refetch lists which may have been in flight when playback started.
        void client.invalidateQueries({ queryKey: ['files'] })
        return true
      } catch {
        toast.error('播放记录保存失败', {
          description: '播放可继续，点击重试保存“已播放”标记。',
          action: { label: '重试保存', onClick: () => { void recordPlayed(id) } },
        })
        return false
      } finally {
        pending.current.delete(id)
      }
    })()
    pending.current.set(id, request)
    return request
  }

  return recordPlayed
}

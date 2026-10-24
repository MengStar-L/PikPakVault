import { useIsMutating, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { AlertCircle, ArrowRight, LoaderCircle, RefreshCw } from 'lucide-react'
import { toast } from 'sonner'
import { api } from './api'
import type { Account } from './api'
import './account-health.css'

type Accounts = { accounts: Account[]; active_id: string }
type AccountIssue = { state: string; title: string; guidance: string; tone: 'danger' | 'warning' }

const issues: Record<string, AccountIssue> = {
  blocked: { state: 'blocked', title: '账号已被限制或冻结', guidance: '请在 PikPak 官方客户端确认账号状态，解除限制后重新检查，或切换其他可用账号。', tone: 'danger' },
  auth_required: { state: 'auth_required', title: '账号登录失效', guidance: '请更新账号密码或登录令牌，然后重新检查连接。', tone: 'danger' },
  quota_exceeded: { state: 'quota_exceeded', title: '云端空间不足', guidance: '请在 PikPak 释放空间、扩容或切换账号，再重新检查并重试保存。', tone: 'warning' },
  verification_required: { state: 'verification_required', title: '账号需要安全验证', guidance: '请在账号卡片中开始安全验证，完成后会自动返回核验账号。', tone: 'warning' },
  error: { state: 'error', title: '账号连接异常', guidance: '暂时无法确认账号可用性。请检查网络后重新检查账号。', tone: 'warning' },
  unverified: { state: 'unverified', title: '账号尚未验证', guidance: '连接检查完成前，暂时无法确认账号可用性。', tone: 'warning' },
}

export function accountIssue(account?: Account): AccountIssue | null {
  if (!account || (account.status === 'ready' && !account.error)) return null
  return issues[account.status] || issues.error
}

export function useAccounts() {
  return useQuery({
    queryKey: ['accounts'],
    queryFn: () => api<Accounts>('/accounts'),
    // SSE normally supplies updates. Poll only while an explicit check is pending
    // so a lost event cannot leave the check button disabled indefinitely.
    refetchInterval: query => query.state.data?.accounts.some(account => account.verification_pending) ? 1500 : false,
  })
}

export function AccountVerifyButton({ account, compact = false }: { account: Account; compact?: boolean }) {
  const client = useQueryClient()
  const mutationKey = ['account-verify', account.id]
  const submitting = useIsMutating({ mutationKey }) > 0
  const mutation = useMutation({
    mutationKey,
    mutationFn: () => api(`/accounts/${account.id}/verify`, {}),
    onMutate: () => {
      client.setQueryData<Accounts>(['accounts'], old => old && ({
        ...old,
        accounts: old.accounts.map(item => item.id === account.id ? { ...item, verification_pending: true } : item),
      }))
    },
    onSuccess: () => toast.info('已开始检查账号，结果会自动更新'),
    onError: error => toast.error(error.message),
    onSettled: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['accounts'] }),
        client.invalidateQueries({ queryKey: ['jobs'] }),
      ])
    },
  })
  const pending = submitting || !!account.verification_pending
  return <button type="button" className={`button secondary account-verify ${compact ? 'small' : ''}`}
    aria-label={`${pending ? '正在检查' : '重新检查'} ${account.name}`} disabled={pending}
    onClick={() => {
      if (account.verification_pending || client.isMutating({ mutationKey })) return
      mutation.mutate()
    }}>
    {pending ? <LoaderCircle size={14} className="spin" /> : <RefreshCw size={14} />}
    {pending ? '正在检查' : '重新检查'}
  </button>
}

export function AccountHealthAlert({ account }: { account?: Account }) {
  const navigate = useNavigate()
  const issue = accountIssue(account)
  if (!issue || !account) return null
  return <section className={`account-health-alert ${issue.tone}`} role="alert" aria-label="当前账号异常">
    <AlertCircle size={18} className="account-health-icon" />
    <div className="account-health-copy">
      <strong><span>{account.name}</span> · {issue.title}</strong>
      <p>{account.error || issue.guidance}</p>
    </div>
    <div className="account-health-actions">
      <button type="button" className="text-button" onClick={() => navigate('/accounts')}>处理账号<ArrowRight size={14} /></button>
      <AccountVerifyButton account={account} compact />
    </div>
  </section>
}

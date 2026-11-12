import type { AdminVideoTask, VideoTask } from '@/api/videos'

export function videoProviderLabel(provider: string): string {
  return ({ minimax: 'MiniMax H3', seedance: 'Seedance / BytePlus' } as Record<string, string>)[provider] ?? provider
}

export function videoBillingModeLabel(mode: string): string {
  return ({ usage_tokens: '实际 Token', local_price: '按秒单价', upstream_cost: '上游成本' } as Record<string, string>)[mode] ?? mode
}

export function videoBillingStatusLabel(task: VideoTask): string {
  const status = task.billing_status ?? (task.settled_at ? 'settled' : task.status === 'error' ? 'not_charged' : task.status === 'done' ? 'legacy' : 'pending')
  return ({ settled: '已结算', pending: '待结算', not_charged: '未扣费', legacy: '历史记录' })[status]
}

export function videoChargeLabel(task: VideoTask): string {
  const status = task.billing_status ?? (task.settled_at ? 'settled' : undefined)
  if (task.status === 'error' || status === 'not_charged') return '未扣费'
  if (task.status !== 'done') return '—'
  if (status === 'pending') return '待结算'
  return videoMoneyLabel(task.billed_cost_usd)
}

export function videoUsageLabel(task: VideoTask): string {
  return task.billing_mode === 'usage_tokens'
    ? `${(task.input_tokens ?? 0).toLocaleString()} 输入 / ${(task.output_tokens ?? 0).toLocaleString()} 输出 Token`
    : `${task.resolution || '—'} · ${task.duration_seconds || '—'}s`
}

export function videoUpstreamCostLabel(task: Pick<AdminVideoTask, 'upstream_cost_usd' | 'upstream_cost_reported'>): string {
  if (!task.upstream_cost_reported && !(task.upstream_cost_usd > 0)) return '未提供'
  return videoMoneyLabel(task.upstream_cost_usd)
}

export function videoMoneyLabel(value: number): string {
  return `$${(value ?? 0).toLocaleString('en-US', { minimumFractionDigits: 4, maximumFractionDigits: 8 })}`
}

export function videoTaskCanDelete(task: VideoTask): boolean {
  if (task.deleted_at || task.status === 'queued' || task.status === 'processing') return false
  return task.status !== 'done' || task.billing_status !== 'pending'
}

import { describe, expect, it } from 'vitest'
import type { VideoTask } from '@/api/videos'
import { videoBillingModeLabel, videoBillingStatusLabel, videoChargeLabel, videoUsageLabel, videoUpstreamCostLabel, videoTaskCanDelete } from '../videoTaskPresentation'

const task = { status: 'done', billing_mode: 'usage_tokens', input_tokens: 20, output_tokens: 1000, billed_cost_usd: 0, billing_status: 'pending' } as VideoTask

describe('video task billing presentation', () => {
  it('distinguishes unreported provider cost from explicit zero or legacy nonzero cost', () => {
    expect(videoUpstreamCostLabel({ ...task, upstream_cost_usd: 0 })).toBe('未提供')
    expect(videoUpstreamCostLabel({ ...task, upstream_cost_usd: 0, upstream_cost_reported: true })).toBe('$0.0000')
    expect(videoUpstreamCostLabel({ ...task, upstream_cost_usd: 0.1 })).toBe('$0.1000')
  })

  it('protects successful unsettled tasks from deletion', () => {
    expect(videoTaskCanDelete(task)).toBe(false)
    expect(videoTaskCanDelete({ ...task, status: 'processing' })).toBe(false)
    expect(videoTaskCanDelete({ ...task, billing_status: 'settled' })).toBe(true)
    expect(videoTaskCanDelete({ ...task, status: 'error' })).toBe(true)
  })
  it('distinguishes unsettled success from an explicitly settled zero charge', () => {
    expect(videoChargeLabel(task)).toBe('待结算')
    expect(videoChargeLabel({ ...task, billing_status: 'settled' })).toBe('$0.0000')
    expect(videoChargeLabel({ ...task, billing_status: 'settled', billed_cost_usd: 0.00000001 })).toBe('$0.00000001')
    expect(videoUpstreamCostLabel({ ...task, upstream_cost_usd: 0.000005 })).toBe('$0.000005')
  })

  it('shows failed generations as uncharged and token units without seconds pricing', () => {
    expect(videoChargeLabel({ ...task, status: 'error' })).toBe('未扣费')
    expect(videoBillingModeLabel(task.billing_mode)).toBe('实际 Token')
    expect(videoUsageLabel(task)).toContain('输出 Token')
    expect(videoBillingStatusLabel({ ...task, billing_status: 'legacy' })).toBe('历史记录')
  })
})

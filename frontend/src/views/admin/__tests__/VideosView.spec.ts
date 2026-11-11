import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import VideosView from '../VideosView.vue'

const { list, stats, fail, get, retry, reconcile } = vi.hoisted(() => ({ list: vi.fn(), stats: vi.fn(), fail: vi.fn(), get: vi.fn(), retry: vi.fn(), reconcile: vi.fn() }))
vi.mock('@/api/videos', () => ({ adminVideosAPI: { list, stats, fail, get, retry, reconcile } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))

const pendingTask = {
  task_id: 'sdv_unsettled', provider: 'seedance', model: 'seedance-1-0-pro-250528',
  status: 'done', billing_mode: 'usage_tokens', billing_status: 'pending',
  billed_cost_usd: 0, upstream_cost_usd: 0, upstream_cost_reported: false,
  duration_seconds: 5, output_tokens: 1000, created_at: '2026-11-11T00:00:00Z',
}
const mountView = () => mount(VideosView, {
  global: { stubs: { AppLayout: { template: '<main><slot /></main>' }, Pagination: true } },
})

describe('admin video accounting safeguards', () => {
  beforeEach(() => {
    list.mockReset().mockResolvedValue({ items: [pendingTask], total: 1 })
    stats.mockReset().mockResolvedValue({ stats: { total: 1, done: 1, unknown_upstream_cost_count: 1, billed_cost_usd: 1 }, margin_percent: 100 })
    fail.mockReset().mockResolvedValue({})
    get.mockReset().mockResolvedValue({ ...pendingTask, procurement: { estimated_cost: 0.1, actual_cost: null, revision: 2, reference: '' } })
    retry.mockReset().mockResolvedValue({ updated: true })
    reconcile.mockReset().mockResolvedValue({ ...pendingTask })
  })

  it('withholds profit when upstream cost is unknown and protects pending settlement', async () => {
    const wrapper = mountView()
    try {
      await flushPromises()
      expect(wrapper.text()).toContain('待核算')
      expect(wrapper.text()).toContain('未提供')
      expect(wrapper.text()).not.toContain('100.0%')
      expect(wrapper.findAll('button').find(button => button.text() === '删除')!.attributes('disabled')).toBeDefined()
    } finally { wrapper.unmount() }
  })

  it('explains that force failure neither cancels the provider nor refunds before submitting', async () => {
    list.mockResolvedValue({ items: [{ ...pendingTask, status: 'processing' }], total: 1 })
    const prompt = vi.spyOn(window, 'prompt').mockReturnValue('confirmed provider issue')
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text() === '强制失败')!.trigger('click')
      await flushPromises()
      expect(prompt.mock.calls[0][0]).toContain('不会取消供应商生成')
      expect(prompt.mock.calls[0][0]).toContain('不会退款')
      expect(prompt.mock.calls[0][0]).toContain('不再自动向用户结算')
      expect(fail).toHaveBeenCalledWith('sdv_unsettled', 'confirmed provider issue')
    } finally { wrapper.unmount(); prompt.mockRestore() }
  })

  it('sends exception, account, group and reconciliation filters to both list and stats', async () => {
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.get('[aria-label="分组 ID"]').setValue('4')
      await wrapper.get('[aria-label="账号 ID"]').setValue('125')
      await wrapper.get('[data-testid="video-exceptions-tab"]').trigger('click')
      await wrapper.get('[aria-label="采购对账状态"]').setValue('pending')
      await flushPromises()
      expect(list).toHaveBeenLastCalledWith(expect.objectContaining({ exception: 'any', reconciliation_status: 'pending', group_id: 4, account_id: 125 }))
      expect(stats).toHaveBeenLastCalledWith(expect.objectContaining({ exception: 'any', group_id: 4, account_id: 125, page: undefined }))
      expect(wrapper.find('a[href*="guide#video-admin"]').exists()).toBe(false)
    } finally { wrapper.unmount() }
  })

  it('records a confirmed zero cost distinctly from unknown using the fetched revision', async () => {
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text() === '采购对账')!.trigger('click')
      await flushPromises()
      expect((wrapper.get('[aria-label="供应商实际扣费"]').element as HTMLInputElement).value).toBe('')
      await wrapper.get('[aria-label="供应商实际扣费"]').setValue('0')
      await wrapper.get('[aria-label="处理备注"]').setValue('供应商确认未收费')
      await wrapper.get('form').trigger('submit')
      expect(reconcile).not.toHaveBeenCalled()
      expect(wrapper.get('[role="alert"]').text()).toContain('凭证')
      await wrapper.get('[aria-label="供应商账单或扣费凭证"]').setValue('bill-123')
      await wrapper.get('form').trigger('submit')
      await flushPromises()
      expect(reconcile).toHaveBeenCalledWith('sdv_unsettled', { estimated_cost: 0.1, actual_cost: 0, reference: 'bill-123', note: '供应商确认未收费', expected_revision: 2 })
      expect(fail).not.toHaveBeenCalled()
      expect(retry).not.toHaveBeenCalled()
    } finally { wrapper.unmount() }
  })

  it('keeps edits open after an optimistic concurrency conflict and never silently overwrites', async () => {
    reconcile.mockRejectedValue({ status: 409 })
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text() === '采购对账')!.trigger('click')
      await flushPromises()
      await wrapper.get('[aria-label="估算采购成本"]').setValue('')
      await wrapper.get('[aria-label="处理备注"]').setValue('撤销估算')
      await wrapper.get('form').trigger('submit')
      await flushPromises()
      expect(reconcile).toHaveBeenCalledWith('sdv_unsettled', expect.objectContaining({ estimated_cost: null, actual_cost: null, expected_revision: 2 }))
      expect(wrapper.get('[role="alert"]').text()).toContain('已被其他管理员更新')
      expect(reconcile).toHaveBeenCalledTimes(1)
    } finally { wrapper.unmount() }
  })

  it('only enables safe recovery supplied by the backend and submits the operator reason', async () => {
    list.mockResolvedValue({ items: [{ ...pendingTask, can_retry: true, exception_codes: ['missing_usage'] }], total: 1 })
    const prompt = vi.spyOn(window, 'prompt').mockReturnValue('  上游已补回用量  ')
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text() === '恢复处理')!.trigger('click')
      await flushPromises()
      expect(prompt.mock.calls[0][0]).toContain('不会创建新视频订单')
      expect(retry).toHaveBeenCalledWith('sdv_unsettled', '上游已补回用量')
      list.mockResolvedValue({ items: [{ ...pendingTask, can_retry: false, retry_unavailable_reason: 'submission_unknown' }], total: 1 })
      await wrapper.findAll('button').find(button => button.text() === '刷新')!.trigger('click')
      await flushPromises()
      const button = wrapper.findAll('button').find(button => button.text() === '恢复处理')!
      expect(button.attributes('disabled')).toBeDefined()
      expect(button.attributes('title')).toContain('无法安全重新提交')
    } finally { wrapper.unmount(); prompt.mockRestore() }
  })
})

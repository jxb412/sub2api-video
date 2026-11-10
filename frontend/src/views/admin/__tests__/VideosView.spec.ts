import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import VideosView from '../VideosView.vue'

const { list, stats, fail } = vi.hoisted(() => ({ list: vi.fn(), stats: vi.fn(), fail: vi.fn() }))
vi.mock('@/api/videos', () => ({ adminVideosAPI: { list, stats, fail } }))
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
})

import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import VideosView from '../VideosView.vue'

const { list, stats } = vi.hoisted(() => ({ list: vi.fn(), stats: vi.fn() }))
vi.mock('@/api/videos', () => ({ videosAPI: { list, stats } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))

const seedanceTask = {
  task_id: 'sdv_test', provider: 'seedance', model: 'seedance-1-0-pro-250528',
  prompt: 'sea', status: 'done', billing_mode: 'usage_tokens', billing_status: 'pending',
  duration_seconds: 5, input_tokens: 0, output_tokens: 1000, billed_cost_usd: 0, upstream_cost_usd: 0,
  created_at: '2026-11-11T00:00:00Z',
}
const mountView = () => mount(VideosView, {
  global: { stubs: {
    AppLayout: { template: '<main><slot /></main>' },
    Pagination: { props: ['total', 'page', 'pageSize'], emits: ['update:page'], template: '<button data-testid="next-page" @click="$emit(\'update:page\', 2)">{{ total }}</button>' },
  } },
})

describe('unified video task page', () => {
  beforeEach(() => {
    list.mockReset().mockResolvedValue({ items: [seedanceTask], total: 41, page: 1, page_size: 20 })
    stats.mockReset().mockResolvedValue({ queued: 0, processing: 0, today: 1, success_rate: 100, billed_cost_usd: 0 })
  })

  it('shows Seedance usage and unsettled billing, and sends the provider filter to the API', async () => {
    const wrapper = mountView()
    try {
      await flushPromises()
      expect(wrapper.text()).toContain('Seedance / BytePlus')
      expect(wrapper.text()).toContain('输出 Token')
      expect(wrapper.text()).toContain('待结算')
      await wrapper.get('[data-testid="video-provider-filter"]').setValue('seedance')
      await flushPromises()
      expect(list).toHaveBeenLastCalledWith(expect.objectContaining({ provider: 'seedance', page: 1 }))
      expect(stats).toHaveBeenLastCalledWith(expect.objectContaining({ provider: 'seedance' }))
    } finally { wrapper.unmount() }
  })

  it('queries billing history on the server and paginates it using its own total', async () => {
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text() === '扣费记录')!.trigger('click')
      await flushPromises()
      expect(list).toHaveBeenLastCalledWith(expect.objectContaining({ billing_only: true, page: 1 }))
      await wrapper.get('[data-testid="next-page"]').trigger('click')
      await flushPromises()
      expect(list).toHaveBeenLastCalledWith(expect.objectContaining({ billing_only: true, page: 2 }))
    } finally { wrapper.unmount() }
  })
})

import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import VideosView from '../VideosView.vue'

const { list, stats, get, download, showError } = vi.hoisted(() => ({ list: vi.fn(), stats: vi.fn(), get: vi.fn(), download: vi.fn(), showError: vi.fn() }))
vi.mock('@/api/videos', () => ({ videosAPI: { list, stats, get, download } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError }) }))

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
    get.mockReset().mockResolvedValue(seedanceTask)
    download.mockReset()
    showError.mockReset()
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

  it('keeps purchase costs and supplier identifiers out of customer tables, details and examples', async () => {
    const task = { ...seedanceTask, billing_mode: 'upstream_cost', billing_status: 'settled',
      billed_cost_usd: 0.6, upstream_cost_usd: 98765.4321, upstream_cost_reported: true,
      upstream_id: 'supplier-private-task', account_name: 'private-channel' }
    list.mockResolvedValue({ items: [task], total: 1 })
    get.mockResolvedValue(task)
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text() === 'sdv_test')!.trigger('click')
      await flushPromises()
      expect(wrapper.text()).toContain('$0.6000')
      expect(wrapper.text()).toContain('按请求计价')
      for (const value of ['上游成本', '上游 ID', 'upstream_cost_usd', 'supplier-private-task', 'private-channel', '98,765.4321']) {
        expect(wrapper.text()).not.toContain(value)
      }
      await wrapper.findAll('button').find(button => button.text() === '✕')!.trigger('click')
      await wrapper.findAll('button').find(button => button.text() === '扣费记录')!.trigger('click')
      await flushPromises()
      expect(wrapper.findAll('th').map(cell => cell.text())).not.toContain('上游成本')
      expect(wrapper.text()).not.toContain('98,765.4321')
    } finally { wrapper.unmount() }
  })

  it('documents H3 gateway inputs and separates Seedance routes and statuses', async () => {
    const wrapper = mountView()
    try {
      await flushPromises()
      expect(wrapper.get('[data-testid="h3-model-guide"]').text()).toContain('仅支持 768P')
      expect(wrapper.text()).toContain('整数 1–15 秒')
      expect(wrapper.text()).toContain('1344 × 768')
      for (const label of ['MiniMax-H3 文生', 'MiniMax-H3 图生', 'MiniMax-H3 首尾帧', 'MiniMax-H3 多参考']) {
        await wrapper.findAll('button').find(button => button.text() === label)!.trigger('click')
        const example = wrapper.get('[data-testid="video-api-sample"]').text()
        expect(example).toContain('$BASE/v1/videos/generations')
        expect(example).not.toContain('\n+ ')
        const body = JSON.parse(example.match(/--data '([\s\S]+)'$/)![1])
        expect(body.model).toBe('MiniMax-H3')
        expect(body.resolution).toBe('768P')
        if (body.content) {
          expect(body).not.toHaveProperty('prompt')
          expect(body).not.toHaveProperty('image_url')
          expect(body.content.filter((item: { type: string }) => item.type === 'text')).toHaveLength(1)
        } else expect(body.prompt).toBeTruthy()
      }
      await wrapper.findAll('button').find(button => button.text() === 'MiniMax-H3 查询与下载')!.trigger('click')
      expect(wrapper.get('[data-testid="video-api-sample"]').text()).toContain('status=done')
      expect(wrapper.text()).toContain('done 表示生成完成，结算状态请查看本页扣费记录')
      await wrapper.findAll('button').find(button => button.text() === 'Seedance 文生')!.trigger('click')
      expect(wrapper.find('[data-testid="h3-model-guide"]').exists()).toBe(false)
      expect(wrapper.get('[data-testid="seedance-model-guide"]').text()).toContain('succeeded')
      expect(wrapper.get('[data-testid="video-api-sample"]').text()).toContain('$BASE/api/v3/contents/generations/tasks')
      expect(wrapper.text()).not.toContain('api.ll2.cn')
    } finally { wrapper.unmount() }
  })

  it.each([
    [410, '视频文件已过期，无法再次下载。生成记录和扣费记录仍保留。'],
    [502, '视频文件暂时无法获取，请稍后重试或联系客服。'],
  ])('explains download failure HTTP %i without changing task or billing state', async (status, message) => {
    download.mockRejectedValue({ status })
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text() === '下载')!.trigger('click')
      await flushPromises()
      expect(download).toHaveBeenCalledWith('sdv_test')
      expect(showError).toHaveBeenCalledWith(message)
      expect(wrapper.text()).toContain('已完成')
      expect(wrapper.text()).toContain('待结算')
      expect(wrapper.get('[data-testid="video-retention-notice"]').text()).toContain('历史生成记录和扣费记录仍会保留')
    } finally { wrapper.unmount() }
  })
})

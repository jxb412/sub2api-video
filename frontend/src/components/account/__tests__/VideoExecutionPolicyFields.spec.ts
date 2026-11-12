import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import VideoExecutionPolicyFields from '../VideoExecutionPolicyFields.vue'
import { defaultVideoExecutionPolicy } from '../videoExecutionPolicy'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('video execution policy form', () => {
  it('shows provider defaults without persisting them until customization is enabled', async () => {
    const wrapper = mount(VideoExecutionPolicyFields, { props: { modelValue: null, protocol: 'll2' } })
    expect((wrapper.get('[data-testid="video-policy-request_timeout_seconds"]').element as HTMLInputElement).value).toBe('30')
    expect(wrapper.get('[data-testid="video-policy-request_timeout_seconds"]').attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    await wrapper.get('[data-testid="video-policy-custom"]').setValue(true)
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([defaultVideoExecutionPolicy(false, 'll2')])
    wrapper.unmount()
  })
  it('keeps a disabled retry policy and zero count explicit when another field changes', async () => {
    const policy = { ...defaultVideoExecutionPolicy(true, ''), max_retries: 0, retry_enabled: false }
    const wrapper = mount(VideoExecutionPolicyFields, { props: { modelValue: policy, seedance: true } })
    expect(wrapper.get('[data-testid="video-policy-max_retries"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="video-policy-request_timeout_seconds"]').setValue('60')
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([{ ...policy, request_timeout_seconds: 60 }])
    await wrapper.get('[data-testid="video-policy-custom"]').setValue(false)
    expect(wrapper.emitted('update:modelValue')?.[1]).toEqual([null])
    wrapper.unmount()
  })
})

import { describe, expect, it } from 'vitest'
import { applyVideoExecutionPolicy, defaultVideoExecutionPolicy, readVideoExecutionPolicy } from '../videoExecutionPolicy'

describe('video account execution policy', () => {
  it('uses safe provider defaults including blocking Getoken create', () => {
    expect(defaultVideoExecutionPolicy(false, 'getoken').request_timeout_seconds).toBe(1200)
    expect(defaultVideoExecutionPolicy(false, 'll2').request_timeout_seconds).toBe(30)
    expect(defaultVideoExecutionPolicy(true, 'getoken')).toMatchObject({ request_timeout_seconds: 45, task_timeout_seconds: 172800 })
  })
  it('does not persist defaults on unrelated edits and clears customization explicitly', () => {
    const extra: Record<string, unknown> = { quota_limit: 10 }
    applyVideoExecutionPolicy(extra, true, null)
    expect(extra).toEqual({ quota_limit: 10 })
    applyVideoExecutionPolicy(extra, true, defaultVideoExecutionPolicy(false, 'll2'))
    expect(readVideoExecutionPolicy(extra)?.max_retries).toBe(3)
    applyVideoExecutionPolicy(extra, true, null)
    expect(extra).toEqual({ quota_limit: 10, video_execution_policy: null })
  })
  it('preserves explicit disabled retries and zero retries without aliasing stored policy', () => {
    const stored = { ...defaultVideoExecutionPolicy(false, 'll2'), max_retries: 0, retry_enabled: false }
    const read = readVideoExecutionPolicy({ video_execution_policy: stored })!
    expect(read.max_retries).toBe(0)
    expect(read.retry_enabled).toBe(false)
    read.max_retries = 5
    expect(stored.max_retries).toBe(0)
    const extra: Record<string, unknown> = {}
    applyVideoExecutionPolicy(extra, false, stored)
    expect(extra).not.toHaveProperty('video_execution_policy')
  })
})

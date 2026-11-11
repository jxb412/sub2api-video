export interface VideoExecutionPolicy {
  request_timeout_seconds: number
  task_timeout_seconds: number
  retry_enabled: boolean
  max_retries: number
  retry_interval_seconds: number
}

export function defaultVideoExecutionPolicy(seedance: boolean, protocol: string): VideoExecutionPolicy {
  return {
    request_timeout_seconds: seedance ? 45 : protocol === 'll2' ? 30 : 1200,
    task_timeout_seconds: seedance ? 172800 : 1200,
    retry_enabled: true,
    max_retries: 3,
    retry_interval_seconds: 30,
  }
}

export function readVideoExecutionPolicy(extra: Record<string, unknown> | undefined): VideoExecutionPolicy | null {
  const value = extra?.video_execution_policy
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  return { ...(value as VideoExecutionPolicy) }
}

export function applyVideoExecutionPolicy(extra: Record<string, unknown>, capable: boolean, policy: VideoExecutionPolicy | null) {
  if (capable && policy) extra.video_execution_policy = { ...policy }
  // Explicit null clears the stored policy when the account API merges extra.
  else if ('video_execution_policy' in extra) extra.video_execution_policy = null
}

import { apiClient } from './client'

/** 视频任务（MiniMax H3 / Seedance 等异步视频模型） */
export interface VideoTask {
  task_id: string
  provider: string
  model: string
  prompt: string
  status: 'queued' | 'processing' | 'done' | 'error'
  resolution?: string
  duration_seconds: number
  user_id: number
  api_key_id: number
  group_id: number
  error_message?: string
  billed_cost_usd: number
  billing_mode: string
  input_tokens?: number
  output_tokens?: number
  settled_at?: string | null
  billing_status?: 'settled' | 'pending' | 'not_charged' | 'legacy'
  created_at: string
  completed_at?: string | null
  deleted_at?: string | null
  group_name?: string
  api_key_name?: string
}

export interface VideoTaskStats {
  total: number
  queued: number
  processing: number
  done: number
  error: number
  today: number
  billed_cost_usd: number
  success_rate: number
}

export interface VideoProcurement {
  estimated_cost: number | null
  actual_cost: number | null
  revision: number
  reference: string
  note: string
  updated_by: number | null
  updated_at: string | null
  reconciliation_status: 'pending' | 'reconciled'
}

export interface VideoExecutionState {
  failure_count: number
  last_error?: string
  next_retry_at?: string | null
  paused_reason?: string
  deadline_at?: string | null
  retry_generation?: number
}

export interface VideoOperationsHistory {
  id: number
  action: 'reconcile' | 'resume' | 'fail'
  actor_id: number
  created_at: string
  before: Partial<VideoProcurement & VideoExecutionState>
  after: Partial<VideoProcurement & VideoExecutionState>
  note: string
}

/** Procurement and execution details are returned only by admin endpoints. */
export interface AdminVideoTask extends VideoTask {
  upstream_id?: string
  upstream_model?: string
  account_id: number
  account_name?: string
  user_email?: string
  upstream_cost_usd: number
  upstream_cost_reported?: boolean
  can_retry?: boolean
  retry_unavailable_reason?: string
  procurement?: VideoProcurement
  execution?: VideoExecutionState
  exception_codes?: string[]
  operations_history?: VideoOperationsHistory[]
}

export interface AdminVideoTaskStats extends VideoTaskStats {
  upstream_cost_usd: number
  unknown_upstream_cost_count?: number
}

export interface VideoProcurementStats {
  estimated_cost_total: number
  actual_cost_total: number
  actual_cost_count: number
  pending_count: number
  reconciled_revenue: number
  confirmed_margin_percent: number | null
}

export interface VideoReconcileInput {
  estimated_cost?: number | null
  actual_cost?: number | null
  reference: string
  note: string
  expected_revision: number
}

export interface AdminVideoTaskListParams extends VideoTaskListParams {
  exception?: string
  reconciliation_status?: 'pending' | 'reconciled'
}

export interface VideoTaskListParams {
  page?: number
  page_size?: number
  status?: string
  provider?: string
  billing_only?: boolean
  model?: string
  search?: string
  group_id?: number
  user_id?: number
  account_id?: number
  from?: string
  to?: string
  include_deleted?: boolean
}

interface Paged<T> {
  items: T[]; total: number; page: number; page_size: number
}

/** 用户端：当前用户的视频任务 */
export const videosAPI = {
  async list(params: VideoTaskListParams = {}) {
    const { data } = await apiClient.get<Paged<VideoTask>>('/videos', { params })
    return data
  },
  async stats(params: VideoTaskListParams = {}) {
    const { data } = await apiClient.get<VideoTaskStats>('/videos/stats', { params })
    return data
  },
  async get(taskId: string) {
    const { data } = await apiClient.get<VideoTask>(`/videos/${encodeURIComponent(taskId)}`)
    return data
  },
  async remove(taskId: string) {
    const { data } = await apiClient.delete<{ deleted: boolean }>(`/videos/${encodeURIComponent(taskId)}`)
    return data
  },
  /** 下载视频内容（带鉴权，返回 Blob） */
  async download(taskId: string) {
    const res = await apiClient.get(`/videos/${encodeURIComponent(taskId)}/content`, { responseType: 'blob', timeout: 600000 })
    return res.data as Blob
  },
}

/** 管理端：全量视频任务 */
export const adminVideosAPI = {
  async list(params: AdminVideoTaskListParams = {}) {
    const { data } = await apiClient.get<Paged<AdminVideoTask>>('/admin/videos', { params })
    return data
  },
  async stats(params: AdminVideoTaskListParams = {}) {
    const { data } = await apiClient.get<{ stats: AdminVideoTaskStats; margin_percent: number | null; procurement?: VideoProcurementStats; exception_counts?: Record<string, number> }>(
      '/admin/videos/stats',
      { params },
    )
    return data
  },
  async get(taskId: string) {
    const { data } = await apiClient.get<AdminVideoTask>(`/admin/videos/${encodeURIComponent(taskId)}`)
    return data
  },
  async remove(taskId: string) {
    const { data } = await apiClient.delete<{ deleted: boolean }>(`/admin/videos/${encodeURIComponent(taskId)}`)
    return data
  },
  async fail(taskId: string, reason: string) {
    const { data } = await apiClient.post(`/admin/videos/${encodeURIComponent(taskId)}/fail`, { reason })
    return data
  },
  async reconcile(taskId: string, input: VideoReconcileInput) {
    const { data } = await apiClient.post<AdminVideoTask>(`/admin/videos/${encodeURIComponent(taskId)}/reconcile`, input)
    return data
  },
  async retry(taskId: string, reason: string) {
    const { data } = await apiClient.post<{ updated: boolean }>(`/admin/videos/${encodeURIComponent(taskId)}/retry`, { reason })
    return data
  },
}

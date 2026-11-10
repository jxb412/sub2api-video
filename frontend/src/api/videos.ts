import { apiClient } from './client'

/** 视频任务（MiniMax H3 / Seedance 等异步视频模型） */
export interface VideoTask {
  task_id: string
  upstream_id?: string
  upstream_model?: string
  provider: string
  model: string
  prompt: string
  status: 'queued' | 'processing' | 'done' | 'error'
  resolution?: string
  duration_seconds: number
  user_id: number
  api_key_id: number
  group_id: number
  account_id: number
  error_message?: string
  upstream_cost_usd: number
  upstream_cost_reported?: boolean
  billed_cost_usd: number
  billing_mode: string
  input_tokens?: number
  output_tokens?: number
  settled_at?: string | null
  billing_status?: 'settled' | 'pending' | 'not_charged' | 'legacy'
  created_at: string
  completed_at?: string | null
  deleted_at?: string | null
  user_email?: string
  group_name?: string
  account_name?: string
  api_key_name?: string
}

export interface VideoTaskStats {
  total: number
  queued: number
  processing: number
  done: number
  error: number
  today: number
  upstream_cost_usd: number
  unknown_upstream_cost_count?: number
  billed_cost_usd: number
  success_rate: number
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
  async list(params: VideoTaskListParams = {}) {
    const { data } = await apiClient.get<Paged<VideoTask>>('/admin/videos', { params })
    return data
  },
  async stats(params: VideoTaskListParams = {}) {
    const { data } = await apiClient.get<{ stats: VideoTaskStats; margin_percent: number }>(
      '/admin/videos/stats',
      { params },
    )
    return data
  },
  async get(taskId: string) {
    const { data } = await apiClient.get<VideoTask>(`/admin/videos/${encodeURIComponent(taskId)}`)
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
}

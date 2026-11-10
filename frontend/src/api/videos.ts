import { apiClient } from './client'

/** 视频任务（MiniMax H3 / Seedance 等异步视频模型） */
export interface VideoTask {
  task_id: string
  upstream_id?: string
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
  billed_cost_usd: number
  billing_mode: string
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
  billed_cost_usd: number
  success_rate: number
}

export interface VideoTaskListParams {
  page?: number
  page_size?: number
  status?: string
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
  data: { items: T[]; total: number; page: number; page_size: number }
}

/** 用户端：当前用户的视频任务 */
export const videosAPI = {
  async list(params: VideoTaskListParams = {}) {
    const { data } = await apiClient.get<Paged<VideoTask>>('/videos', { params })
    return data.data
  },
  async stats(params: VideoTaskListParams = {}) {
    const { data } = await apiClient.get<{ data: VideoTaskStats }>('/videos/stats', { params })
    return data.data
  },
  async get(taskId: string) {
    const { data } = await apiClient.get<{ data: VideoTask }>(`/videos/${taskId}`)
    return data.data
  },
  async remove(taskId: string) {
    const { data } = await apiClient.delete<{ data: { deleted: boolean } }>(`/videos/${taskId}`)
    return data.data
  },
  /** 下载视频内容（带鉴权，返回 Blob） */
  async download(taskId: string) {
    const res = await apiClient.get(`/videos/${taskId}/content`, { responseType: 'blob' })
    return res.data as Blob
  },
}

/** 管理端：全量视频任务 */
export const adminVideosAPI = {
  async list(params: VideoTaskListParams = {}) {
    const { data } = await apiClient.get<Paged<VideoTask>>('/admin/videos', { params })
    return data.data
  },
  async stats(params: VideoTaskListParams = {}) {
    const { data } = await apiClient.get<{ data: { stats: VideoTaskStats; margin_percent: number } }>(
      '/admin/videos/stats',
      { params },
    )
    return data.data
  },
  async get(taskId: string) {
    const { data } = await apiClient.get<{ data: VideoTask }>(`/admin/videos/${taskId}`)
    return data.data
  },
  async remove(taskId: string) {
    const { data } = await apiClient.delete<{ data: { deleted: boolean } }>(`/admin/videos/${taskId}`)
    return data.data
  },
  async fail(taskId: string, reason: string) {
    const form = new FormData()
    form.append('reason', reason)
    const { data } = await apiClient.post(`/admin/videos/${taskId}/fail`, form)
    return data
  },
}

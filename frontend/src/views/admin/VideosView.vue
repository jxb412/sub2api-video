<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import { useAppStore } from '@/stores/app'
import { adminVideosAPI, type VideoTask, type VideoTaskStats } from '@/api/videos'
import { videoProviderLabel, videoBillingModeLabel, videoBillingStatusLabel, videoChargeLabel, videoUsageLabel, videoUpstreamCostLabel, videoTaskCanDelete, videoMoneyLabel } from '../videoTaskPresentation'

const appStore = useAppStore()
const tasks = ref<VideoTask[]>([])
let loadSequence = 0
const stats = ref<VideoTaskStats | null>(null)
const margin = ref(0)
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const statusFilter = ref('')
const providerFilter = ref('')
const modelFilter = ref('')
const search = ref('')
const userId = ref('')
const includeDeleted = ref(false)
const detail = ref<VideoTask | null>(null)
let pollTimer: number | undefined
let disposed = false

const statusMeta: Record<string, string> = {
  queued: 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300',
  processing: 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300',
  done: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300',
  error: 'bg-rose-50 text-rose-700 dark:bg-rose-900/30 dark:text-rose-300',
}
const statusLabel: Record<string, string> = {
  queued: '排队中', processing: '生成中', done: '已完成', error: '失败',
}
const modelOptions = computed(() => [...new Set([modelFilter.value, 'MiniMax-H3', 'seedance-1-0-pro-250528', 'seedance-1-0-lite-t2v-250428', 'seedance-1-0-lite-i2v-250428', ...tasks.value.map((t) => t.model)])].filter(Boolean).sort())

const money = videoMoneyLabel
function shortId(id: string) { return id.length > 14 ? `${id.slice(0, 14)}…` : id }
function fmt(v?: string | null) {
  if (!v) return '—'
  const d = new Date(v); const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

async function load() {
  const sequence = ++loadSequence
  loading.value = true
  try {
    const params = {
      page: page.value,
      page_size: pageSize.value,
      status: statusFilter.value || undefined,
      provider: providerFilter.value || undefined,
      model: modelFilter.value || undefined,
      search: search.value || undefined,
      user_id: userId.value ? Number(userId.value) : undefined,
      include_deleted: includeDeleted.value || undefined,
    }
    const [list, st] = await Promise.all([adminVideosAPI.list(params), adminVideosAPI.stats({ ...params, page: undefined, page_size: undefined })])
    if (sequence !== loadSequence) return
    tasks.value = list.items ?? []
    total.value = list.total ?? 0
    stats.value = st.stats
    margin.value = st.margin_percent ?? 0
    if (detail.value) detail.value = tasks.value.find(t => t.task_id === detail.value?.task_id) ?? detail.value
  } catch {
    if (sequence === loadSequence) appStore.showError('加载视频任务失败，请重试')
  } finally {
    if (sequence === loadSequence) loading.value = false
    if (sequence === loadSequence) schedulePoll()
  }
}

function schedulePoll() {
  if (pollTimer) window.clearTimeout(pollTimer)
  if (disposed || !tasks.value.some(t => t.status === 'queued' || t.status === 'processing' || t.billing_status === 'pending')) return
  pollTimer = window.setTimeout(load, 5000)
}

async function forceFail(t: VideoTask) {
  const reason = window.prompt('此操作仅将平台任务标记失败，不会取消供应商生成，也不会退款。供应商可能继续生成并收费，且该任务将不再自动向用户结算，请先核对供应商状态。\n\n请输入标记失败原因：', '管理端强制失败')
  if (!reason) return
  try {
    await adminVideosAPI.fail(t.task_id, reason)
    await load()
  } catch { appStore.showError('标记失败未成功，任务可能已完成') }
}

async function remove(t: VideoTask) {
  if (!videoTaskCanDelete(t)) return
  if (!window.confirm('确认删除该任务？（软删除，可审计）')) return
  try {
    await adminVideosAPI.remove(t.task_id)
    if (detail.value?.task_id === t.task_id) detail.value = null
    await load()
  } catch { appStore.showError('删除任务失败') }
}

async function openDetail(t: VideoTask) {
  try { detail.value = await adminVideosAPI.get(t.task_id) } catch { appStore.showError('加载任务详情失败') }
}

watch([page, pageSize], load)
watch([statusFilter, providerFilter, modelFilter, includeDeleted], () => { page.value = 1; load() })
onMounted(load)
onUnmounted(() => { disposed = true; loadSequence++; if (pollTimer) window.clearTimeout(pollTimer) })
</script>

<template>
  <AppLayout>
    <div class="mx-auto w-full max-w-7xl space-y-5 px-4 py-6">
      <div class="flex items-center justify-between">
        <div>
          <h1 class="text-xl font-semibold text-slate-900 dark:text-slate-100">视频任务管理</h1>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">MiniMax H3 与 Seedance 的生成任务、Token 用量、结算与成本</p>
          <a href="https://www.yunqiao.me/guide#video-admin" target="_blank" rel="noopener noreferrer" class="mt-1 inline-block text-xs text-indigo-600 hover:underline">分组、账号与计费配置说明</a>
        </div>
        <button class="rounded-lg border border-slate-200 px-3 py-2 text-sm dark:border-slate-700" :disabled="loading" @click="load()">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>

      <div class="grid grid-cols-2 gap-4 lg:grid-cols-6">
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500">总任务</div>
          <div class="mt-1 text-2xl font-semibold">{{ stats?.total ?? 0 }}</div>
        </div>
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500">今日新增</div>
          <div class="mt-1 text-2xl font-semibold">{{ stats?.today ?? 0 }}</div>
        </div>
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500">生成中</div>
          <div class="mt-1 text-2xl font-semibold text-amber-600">{{ (stats?.queued ?? 0) + (stats?.processing ?? 0) }}</div>
        </div>
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500">失败率</div>
          <div class="mt-1 text-2xl font-semibold text-rose-600">
            {{ stats && stats.total ? (((stats.error ?? 0) / stats.total) * 100).toFixed(1) : '0.0' }}%
          </div>
        </div>
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500">上游成本 / 实收</div>
          <div class="mt-1 text-sm font-semibold">
            {{ money(stats?.upstream_cost_usd ?? 0) }} / {{ money(stats?.billed_cost_usd ?? 0) }}
          </div>
          <div v-if="stats?.unknown_upstream_cost_count" class="mt-1 text-xs text-amber-600">{{ stats.unknown_upstream_cost_count }} 个已完成任务未提供上游成本；成本合计仅含已知金额</div>
        </div>
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500">毛利率</div>
          <div class="mt-1 text-2xl font-semibold text-emerald-600">{{ stats?.unknown_upstream_cost_count ? '待核算' : `${margin.toFixed(1)}%` }}</div>
        </div>
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <select v-model="providerFilter" aria-label="视频供应商" data-testid="video-provider-filter" class="rounded-lg border border-slate-200 px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900">
          <option value="">全部供应商</option>
          <option value="minimax">MiniMax H3</option>
          <option value="seedance">Seedance / BytePlus</option>
        </select>
        <input v-model="userId" placeholder="用户 ID" class="w-28 rounded-lg border border-slate-200 px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900" />
        <select v-model="statusFilter" class="rounded-lg border border-slate-200 px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900">
          <option value="">全部状态</option>
          <option value="queued">排队中</option>
          <option value="processing">生成中</option>
          <option value="done">已完成</option>
          <option value="error">失败</option>
        </select>
        <select v-model="modelFilter" class="rounded-lg border border-slate-200 px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900">
          <option value="">全部模型</option>
          <option v-for="m in modelOptions" :key="m" :value="m">{{ m }}</option>
        </select>
        <input v-model="search" placeholder="任务 ID / 提示词" class="min-w-[200px] flex-1 rounded-lg border border-slate-200 px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900" @keyup.enter="page = 1; load()" />
        <label class="flex items-center gap-1 text-xs text-slate-500">
          <input v-model="includeDeleted" type="checkbox" /> 含已删除
        </label>
        <button class="rounded-lg bg-slate-900 px-3 py-2 text-sm text-white dark:bg-slate-700" @click="page = 1; load()">查询</button>
      </div>

      <div class="overflow-hidden rounded-2xl border border-slate-200/70 bg-white shadow-sm dark:border-slate-800 dark:bg-slate-900">
        <table class="w-full text-sm">
          <thead class="bg-slate-50 text-xs uppercase text-slate-500 dark:bg-slate-800/60 dark:text-slate-400">
            <tr>
              <th class="px-4 py-3 text-left">任务</th>
              <th class="px-4 py-3 text-left">用户</th>
              <th class="px-4 py-3 text-left">分组 / 账号</th>
              <th class="px-4 py-3 text-left">模型</th>
              <th class="px-4 py-3 text-left">状态</th>
              <th class="px-4 py-3 text-right">上游成本</th>
              <th class="px-4 py-3 text-right">实收</th>
              <th class="px-4 py-3 text-left">创建</th>
              <th class="px-4 py-3 text-right">操作</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
            <tr v-for="t in tasks" :key="t.task_id" class="hover:bg-slate-50/70 dark:hover:bg-slate-800/40">
              <td class="px-4 py-3">
                <button class="font-mono text-xs hover:text-indigo-600" @click="openDetail(t)">{{ shortId(t.task_id) }}</button>
                <div v-if="t.upstream_id" class="mt-0.5 font-mono text-[10px] text-slate-400">{{ t.upstream_id.slice(0, 8) }}</div>
              </td>
              <td class="px-4 py-3 text-xs text-slate-600 dark:text-slate-400">{{ t.user_email || t.user_id }}</td>
              <td class="px-4 py-3 text-xs text-slate-600 dark:text-slate-400">{{ t.group_name || t.group_id }} / {{ t.account_name || t.account_id }}</td>
              <td class="px-4 py-3"><span class="rounded-md bg-indigo-50 px-2 py-0.5 text-xs text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-300">{{ t.model }}</span><div class="mt-1 text-[11px] text-slate-500">{{ videoProviderLabel(t.provider) }}</div><div class="mt-1 text-[11px] text-slate-500">{{ videoUsageLabel(t) }}</div></td>
              <td class="px-4 py-3">
                <span class="rounded-full px-2 py-0.5 text-xs" :class="statusMeta[t.status]">{{ statusLabel[t.status] }}</span>
                <div v-if="t.deleted_at" class="mt-0.5 text-[10px] text-slate-400">已删除</div>
              </td>
              <td class="px-4 py-3 text-right font-mono text-xs">{{ videoUpstreamCostLabel(t) }}</td>
              <td class="px-4 py-3 text-right font-mono text-xs">{{ videoChargeLabel(t) }}<div class="mt-1 text-[11px] text-slate-500">{{ videoBillingStatusLabel(t) }}</div></td>
              <td class="px-4 py-3 text-xs text-slate-500">{{ fmt(t.created_at) }}</td>
              <td class="px-4 py-3 text-right">
                <button class="rounded-md px-2 py-1 text-xs text-amber-600 hover:bg-amber-50" :disabled="t.status === 'done' || t.status === 'error'" @click="forceFail(t)">强制失败</button>
                <button class="rounded-md px-2 py-1 text-xs text-rose-600 hover:bg-rose-50 disabled:text-slate-300" :disabled="!videoTaskCanDelete(t)" :title="t.billing_status === 'pending' ? '待结算任务不能删除' : undefined" @click="remove(t)">删除</button>
              </td>
            </tr>
            <tr v-if="!tasks.length && !loading">
              <td colspan="9" class="px-4 py-16 text-center text-sm text-slate-500">暂无视频任务</td>
            </tr>
          </tbody>
        </table>
        <div class="flex items-center justify-between border-t border-slate-100 px-4 py-3 dark:border-slate-800">
          <span class="text-xs text-slate-500">共 {{ total }} 条</span>
          <Pagination v-model:page="page" v-model:page-size="pageSize" :total="total" />
        </div>
      </div>
    </div>

    <div v-if="detail" class="fixed inset-0 z-40 flex justify-end bg-black/30" @click.self="detail = null">
      <div class="h-full w-full max-w-lg overflow-y-auto bg-white p-5 shadow-xl dark:bg-slate-900">
        <div class="flex items-center justify-between">
          <h2 class="text-base font-semibold">任务详情</h2>
          <button class="text-slate-400" @click="detail = null">✕</button>
        </div>
        <div class="mt-4 space-y-2 text-sm">
          <div class="flex justify-between"><span class="text-slate-500">任务 ID</span><span class="font-mono text-xs break-all text-right">{{ detail.task_id }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">用户</span><span>{{ detail.user_email || detail.user_id }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">分组 / 账号</span><span>{{ detail.group_name }} / {{ detail.account_name }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">模型</span><span>{{ detail.model }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">供应商</span><span>{{ videoProviderLabel(detail.provider) }}</span></div>
          <div class="flex justify-between gap-3"><span class="text-slate-500">计费用量</span><span class="text-right text-xs">{{ videoUsageLabel(detail) }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">API Key</span><span>{{ detail.api_key_name || detail.api_key_id }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">状态</span><span>{{ statusLabel[detail.status] }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">上游成本</span><span class="font-mono">{{ videoUpstreamCostLabel(detail) }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">实收</span><span class="font-mono">{{ videoChargeLabel(detail) }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">计费模式</span><span>{{ videoBillingModeLabel(detail.billing_mode) }}</span></div>
          <div class="flex justify-between"><span class="text-slate-500">结算状态</span><span>{{ videoBillingStatusLabel(detail) }}</span></div>
          <div v-if="detail.settled_at" class="flex justify-between"><span class="text-slate-500">结算时间</span><span>{{ fmt(detail.settled_at) }}</span></div>
          <div v-if="detail.error_message" class="rounded-lg bg-rose-50 p-3 text-xs text-rose-700 dark:bg-rose-950 dark:text-rose-300">{{ detail.error_message }}</div>
          <div><div class="text-slate-500">提示词</div><div class="mt-1 rounded-lg bg-slate-50 p-2 text-xs dark:bg-slate-800">{{ detail.prompt }}</div></div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

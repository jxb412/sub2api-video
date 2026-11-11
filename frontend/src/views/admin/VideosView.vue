<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import { useAppStore } from '@/stores/app'
import { adminVideosAPI, type AdminVideoTask, type AdminVideoTaskListParams, type VideoProcurementStats, type VideoTaskStats } from '@/api/videos'
import { videoProviderLabel, videoBillingModeLabel, videoBillingStatusLabel, videoChargeLabel, videoUsageLabel, videoUpstreamCostLabel, videoTaskCanDelete, videoMoneyLabel } from '../videoTaskPresentation'

const appStore = useAppStore()
const tasks = ref<AdminVideoTask[]>([])
const stats = ref<VideoTaskStats | null>(null)
const procurementStats = ref<VideoProcurementStats | null>(null)
const exceptionCounts = ref<Record<string, number>>({})
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const actionBusy = ref(false)
const statusFilter = ref('')
const providerFilter = ref('')
const modelFilter = ref('')
const exceptionFilter = ref('')
const reconciliationFilter = ref<'' | 'pending' | 'reconciled'>('')
const search = ref('')
const userId = ref('')
const groupId = ref('')
const accountId = ref('')
const includeDeleted = ref(false)
const detail = ref<AdminVideoTask | null>(null)
const reconciliation = ref<AdminVideoTask | null>(null)
const estimateInput = ref<string | number>('')
const actualInput = ref<string | number>('')
const referenceInput = ref('')
const noteInput = ref('')
const formError = ref('')
let loadSequence = 0
let detailSequence = 0
let pollTimer: number | undefined
let disposed = false

const statusMeta: Record<string, string> = {
  queued: 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300',
  processing: 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300',
  done: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300',
  error: 'bg-rose-50 text-rose-700 dark:bg-rose-900/30 dark:text-rose-300',
}
const statusLabel: Record<string, string> = { queued: '排队中', processing: '生成中', done: '已完成', error: '失败' }
const operationLabels: Record<string, string> = { reconcile: '采购对账', resume: '恢复处理', fail: '人工标记失败' }
const exceptionLabels: Record<string, string> = {
  timeout: '任务超时', submission_unknown: '提交结果不明', retry_exhausted: '重试已停止',
  missing_usage: '缺少计费用量', settlement_pending: '待用户结算',
  reconciliation_pending: '待采购对账', configuration_error: '账号配置异常',
}
const pauseLabels: Record<string, string> = {
  task_timeout: '超过自动处理时限', retries_exhausted: '重试次数已耗尽', retry_disabled: '已关闭自动重试',
  submission_uncertain: '提交结果不明，需核对供应商', submission_expired: '已超过安全提交恢复时限',
  configuration_changed: '账号凭据或协议已变更',
}
const retryReasons: Record<string, string> = {
  already_active: '任务正在自动处理，无需重复恢复',
  completed: '任务已结束或已结算', deleted: '任务已删除', submission_unknown: '提交结果不明，请先核对供应商，无法安全重新提交',
  submission_expired: '已超过安全提交恢复时限', no_snapshot: '历史任务缺少恢复快照',
}
const modelOptions = computed(() => [...new Set([modelFilter.value, 'MiniMax-H3', 'seedance-1-0-pro-250528', 'seedance-1-0-lite-t2v-250428', 'seedance-1-0-lite-i2v-250428', ...tasks.value.map(t => t.model)])].filter(Boolean).sort())
const money = videoMoneyLabel
const knownMoney = (value?: number | null) => value == null ? '未核实' : money(value)
const estimatedMoney = (value?: number | null) => value == null ? '未估算' : money(value)
function shortId(id: string) { return id.length > 14 ? `${id.slice(0, 14)}…` : id }
function fmt(value?: string | null) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN', { hour12: false })
}
const canRetry = (task: AdminVideoTask) => task.can_retry === true
const retryReason = (task: AdminVideoTask) => retryReasons[task.retry_unavailable_reason || ''] || task.retry_unavailable_reason || '恢复原任务的查询、结算或安全幂等提交'
async function load() {
  const sequence = ++loadSequence
  loading.value = true
  try {
    const params: AdminVideoTaskListParams = {
      page: page.value, page_size: pageSize.value, status: statusFilter.value || undefined,
      provider: providerFilter.value || undefined, model: modelFilter.value || undefined,
      exception: exceptionFilter.value || undefined, reconciliation_status: reconciliationFilter.value || undefined,
      search: search.value || undefined, user_id: userId.value ? Number(userId.value) : undefined,
      group_id: groupId.value ? Number(groupId.value) : undefined, account_id: accountId.value ? Number(accountId.value) : undefined,
      include_deleted: includeDeleted.value || undefined,
    }
    const [list, summary] = await Promise.all([adminVideosAPI.list(params), adminVideosAPI.stats({ ...params, page: undefined, page_size: undefined })])
    if (sequence !== loadSequence || disposed) return
    tasks.value = list.items ?? []; total.value = list.total ?? 0; stats.value = summary.stats
    procurementStats.value = summary.procurement ?? null; exceptionCounts.value = summary.exception_counts ?? {}
    const updatedDetail = detail.value && tasks.value.find(task => task.task_id === detail.value?.task_id)
    if (updatedDetail && detail.value) detail.value = { ...detail.value, ...updatedDetail, operations_history: detail.value.operations_history }
  } catch { if (sequence === loadSequence) appStore.showError('加载视频任务失败，请重试') }
  finally { if (sequence === loadSequence) { loading.value = false; schedulePoll() } }
}
function schedulePoll() {
  if (pollTimer) window.clearTimeout(pollTimer)
  if (disposed || !tasks.value.some(task => task.status === 'queued' || task.status === 'processing' || task.billing_status === 'pending')) return
  pollTimer = window.setTimeout(load, 5000)
}
async function refreshDetail() { if (detail.value) await openDetail(detail.value) }
async function retry(task: AdminVideoTask) {
  if (actionBusy.value || !canRetry(task)) return
  const reason = window.prompt('恢复该任务的状态查询或结算；符合条件的 LL2 任务使用原幂等键恢复提交，不会创建新视频订单。请先核对供应商状态。\n\n请输入处理原因：')?.trim()
  if (!reason) return
  actionBusy.value = true
  try { await adminVideosAPI.retry(task.task_id, reason); await Promise.all([load(), refreshDetail()]) }
  catch { appStore.showError('恢复失败：任务可能正在处理，或已不具备安全恢复条件。请刷新并核对供应商状态。') }
  finally { actionBusy.value = false }
}
async function forceFail(task: AdminVideoTask) {
  if (actionBusy.value) return
  const reason = window.prompt('此操作仅将平台任务标记失败，不会取消供应商生成，也不会退款。供应商可能继续生成并收费，且该任务将不再自动向用户结算，请先核对供应商状态。\n\n请输入标记失败原因：', '管理端强制失败')?.trim()
  if (!reason) return
  actionBusy.value = true
  try { await adminVideosAPI.fail(task.task_id, reason); await Promise.all([load(), refreshDetail()]) }
  catch { appStore.showError('标记失败未成功，任务可能已完成') }
  finally { actionBusy.value = false }
}
async function remove(task: AdminVideoTask) {
  if (actionBusy.value || !videoTaskCanDelete(task) || !window.confirm('确认删除该任务？（软删除，可审计）')) return
  actionBusy.value = true
  try { await adminVideosAPI.remove(task.task_id); if (detail.value?.task_id === task.task_id) closeDetail(); await load() }
  catch { appStore.showError('删除任务失败') }
  finally { actionBusy.value = false }
}
async function openDetail(task: AdminVideoTask) {
  const sequence = ++detailSequence
  try { const result = await adminVideosAPI.get(task.task_id); if (sequence === detailSequence && !disposed) detail.value = result }
  catch { if (sequence === detailSequence) appStore.showError('加载任务详情失败') }
}
function closeDetail() { detailSequence++; detail.value = null }
async function openReconciliation(task: AdminVideoTask) {
  if (actionBusy.value) return
  actionBusy.value = true
  try {
    const fresh = await adminVideosAPI.get(task.task_id)
    reconciliation.value = fresh
    estimateInput.value = fresh.procurement?.estimated_cost?.toString() ?? ''
    actualInput.value = fresh.procurement?.actual_cost?.toString() ?? ''
    referenceInput.value = fresh.procurement?.reference ?? ''; noteInput.value = ''; formError.value = ''
  } catch { appStore.showError('加载采购对账数据失败') }
  finally { actionBusy.value = false }
}
async function saveReconciliation() {
  if (!reconciliation.value || actionBusy.value) return
  const estimate = String(estimateInput.value).trim() === '' ? null : Number(estimateInput.value)
  const actual = String(actualInput.value).trim() === '' ? null : Number(actualInput.value)
  if ([estimate, actual].some(value => value !== null && (!Number.isFinite(value) || value < 0 || value > 1e9))) {
    formError.value = '金额必须在 0 到 1,000,000,000 之间；留空表示未知。'; return
  }
  if (!noteInput.value.trim() || (actual !== null && !referenceInput.value.trim())) {
    formError.value = '请填写处理备注；录入实际扣费时还必须填写供应商账单或扣费凭证。'; return
  }
  actionBusy.value = true; formError.value = ''
  try {
    const updated = await adminVideosAPI.reconcile(reconciliation.value.task_id, {
      estimated_cost: estimate, actual_cost: actual, reference: referenceInput.value.trim(), note: noteInput.value.trim(),
      expected_revision: reconciliation.value.procurement?.revision ?? 0,
    })
    if (detail.value?.task_id === updated.task_id) detail.value = updated
    reconciliation.value = null; await load()
  } catch (error) {
    const status = (error as { response?: { status?: number }; status?: number }).response?.status ?? (error as { status?: number }).status
    formError.value = status === 409 ? '对账记录已被其他管理员更新。请关闭并重新打开，核对最新金额后再保存。' : '保存失败，请检查金额、凭证和备注后重试。'
  } finally { actionBusy.value = false }
}
function query() { if (page.value === 1) void load(); else page.value = 1 }
watch([page, pageSize], load)
watch([statusFilter, providerFilter, modelFilter, includeDeleted, exceptionFilter, reconciliationFilter], query)
onMounted(load)
onUnmounted(() => { disposed = true; loadSequence++; detailSequence++; if (pollTimer) window.clearTimeout(pollTimer) })
</script>

<template>
  <AppLayout>
    <div class="video-admin mx-auto min-w-0 w-full max-w-screen-2xl space-y-5 py-6 sm:px-4">
      <div class="flex min-w-0 items-start justify-between gap-3">
        <div class="min-w-0"><h1 class="text-xl font-semibold">视频任务管理</h1><p class="mt-1 text-sm text-slate-500">MiniMax H3 与 Seedance 的异常处理、用户结算与采购成本对账</p></div>
        <button class="btn btn-secondary shrink-0" :disabled="loading" @click="load()">{{ loading ? '刷新中…' : '刷新' }}</button>
      </div>
      <details class="rounded-lg border border-slate-200 p-3 text-xs text-slate-600 dark:border-slate-700 dark:text-slate-300">
        <summary class="cursor-pointer font-medium">管理说明：重试、超时与成本口径</summary>
        <p class="mt-2">超时与重试策略在账号编辑页设置，仅影响新任务。正常查询生成进度不消耗重试次数；超时或重试耗尽会暂停自动处理。恢复操作沿用原任务，不会创建新视频订单。</p>
        <p class="mt-2">采购估算、接口报告金额、供应商实际扣费分别记录。实际扣费以供应商账单为准，LL2 未提供金额时保持“未核实”。采购对账不会再次扣用户余额，也不会修改原结算。</p>
        <p class="mt-2">金额按站内额度记录：充值人民币 1 元对应 1 额度，界面沿用 $ 符号，不进行汇率换算。失败任务也可能产生供应商费用。已对账毛利仅统计录入实际扣费的任务，不代表全部任务利润。</p>
      </details>
      <div class="grid grid-cols-2 gap-3 lg:grid-cols-6">
        <div class="min-w-0 rounded-xl border border-slate-200 p-4 dark:border-slate-700"><div class="text-xs text-slate-500">筛选内任务 / 生成中</div><div class="mt-1 text-xl font-semibold">{{ stats?.total ?? 0 }} / {{ (stats?.queued ?? 0) + (stats?.processing ?? 0) }}</div></div>
        <div class="min-w-0 rounded-xl border border-slate-200 p-4 dark:border-slate-700"><div class="text-xs text-slate-500">用户实收</div><div class="mt-1 whitespace-nowrap font-semibold">{{ money(stats?.billed_cost_usd ?? 0) }}</div></div>
        <div class="min-w-0 rounded-xl border border-slate-200 p-4 dark:border-slate-700"><div class="text-xs text-slate-500">采购估算合计</div><div class="mt-1 whitespace-nowrap font-semibold">{{ money(procurementStats?.estimated_cost_total ?? 0) }}</div><p class="mt-1 text-xs text-slate-500">仅含已录入估算</p></div>
        <div class="min-w-0 rounded-xl border border-slate-200 p-4 dark:border-slate-700"><div class="text-xs text-slate-500">供应商实际扣费</div><div class="mt-1 whitespace-nowrap font-semibold">{{ procurementStats?.actual_cost_count ? money(procurementStats.actual_cost_total) : '未核实' }}</div><p class="mt-1 text-xs text-slate-500">已核实 {{ procurementStats?.actual_cost_count ?? 0 }} 条</p></div>
        <div class="min-w-0 rounded-xl border border-slate-200 p-4 dark:border-slate-700"><div class="text-xs text-slate-500">待采购对账</div><div class="mt-1 text-xl font-semibold text-amber-600">{{ procurementStats?.pending_count ?? stats?.unknown_upstream_cost_count ?? 0 }}</div><p class="mt-1 text-xs text-slate-500">未提供实际扣费不按零成本计算</p></div>
        <div class="min-w-0 rounded-xl border border-slate-200 p-4 dark:border-slate-700"><div class="text-xs text-slate-500">已对账任务毛利率</div><div class="mt-1 text-xl font-semibold">{{ procurementStats?.confirmed_margin_percent == null ? '待核算' : `${procurementStats.confirmed_margin_percent.toFixed(1)}%` }}</div><p class="mt-1 text-xs text-slate-500">仅基于已核实实际扣费</p></div>
      </div>
      <div class="flex flex-wrap gap-2" role="group" aria-label="任务视图">
        <button class="btn" :class="!exceptionFilter ? 'btn-primary' : 'btn-secondary'" data-testid="video-all-tab" @click="exceptionFilter = ''">全部任务</button>
        <button class="btn" :class="exceptionFilter ? 'btn-primary' : 'btn-secondary'" data-testid="video-exceptions-tab" @click="exceptionFilter = 'any'">异常任务处理中心</button>
      </div>
      <div v-if="exceptionFilter" class="flex flex-wrap gap-2 rounded-lg bg-amber-50 p-3 dark:bg-amber-950/30">
        <button v-for="(label, code) in exceptionLabels" :key="code" class="rounded border px-2 py-1 text-xs" :class="exceptionFilter === code ? 'border-amber-600 text-amber-800 dark:text-amber-200' : 'border-transparent text-slate-600 dark:text-slate-300'" @click="exceptionFilter = code">{{ label }} {{ exceptionCounts[code] ?? 0 }}</button>
        <p class="w-full text-xs text-slate-500">同一任务可有多种异常。计数遵循当前筛选条件。</p>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <select v-model="providerFilter" aria-label="视频供应商" data-testid="video-provider-filter" class="input min-w-0 max-w-full w-auto"><option value="">全部供应商</option><option value="minimax">MiniMax H3</option><option value="seedance">Seedance / BytePlus</option></select>
        <input v-model="userId" type="number" min="1" aria-label="用户 ID" placeholder="用户 ID" class="input w-28" @keyup.enter="query" />
        <input v-model="groupId" type="number" min="1" aria-label="分组 ID" placeholder="分组 ID" class="input w-28" @keyup.enter="query" />
        <input v-model="accountId" type="number" min="1" aria-label="账号 ID" placeholder="账号 ID" class="input w-28" @keyup.enter="query" />
        <select v-model="statusFilter" aria-label="任务状态" class="input min-w-0 max-w-full w-auto"><option value="">全部状态</option><option v-for="(label, value) in statusLabel" :key="value" :value="value">{{ label }}</option></select>
        <select v-model="modelFilter" aria-label="模型" class="input min-w-0 max-w-full w-auto"><option value="">全部模型</option><option v-for="model in modelOptions" :key="model" :value="model">{{ model }}</option></select>
        <select v-model="reconciliationFilter" aria-label="采购对账状态" class="input min-w-0 max-w-full w-auto"><option value="">全部对账状态</option><option value="pending">待采购对账</option><option value="reconciled">已核实实际扣费</option></select>
        <input v-model="search" aria-label="任务搜索" placeholder="任务 ID / 提示词" class="input min-w-[180px] flex-1" @keyup.enter="query" />
        <label class="flex items-center gap-1 text-xs text-slate-500"><input v-model="includeDeleted" type="checkbox" /> 含已删除</label>
        <button class="btn btn-secondary" @click="query">查询</button>
      </div>
      <div class="min-w-0 w-full overflow-hidden rounded-xl border border-slate-200 dark:border-slate-700">
        <div class="min-w-0 max-w-full overflow-x-auto" data-testid="video-table-scroll"><table class="w-full min-w-[1100px] text-sm">
          <thead class="bg-slate-50 text-xs text-slate-500 dark:bg-slate-800"><tr><th class="px-3 py-3 text-left">任务 / 用户</th><th class="px-3 py-3 text-left">分组 / 账号</th><th class="px-3 py-3 text-left">模型 / 用量</th><th class="px-3 py-3 text-left">状态 / 异常</th><th class="px-3 py-3 text-right">采购成本</th><th class="px-3 py-3 text-right">用户实收</th><th class="px-3 py-3 text-left">创建</th><th class="px-3 py-3 text-right">操作</th></tr></thead>
          <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
            <tr v-for="task in tasks" :key="task.task_id" class="hover:bg-slate-50 dark:hover:bg-slate-800/40">
              <td class="px-3 py-3"><button class="font-mono text-xs text-indigo-600" @click="openDetail(task)">{{ shortId(task.task_id) }}</button><div class="mt-1 text-xs text-slate-500">{{ task.user_email || task.user_id }}</div></td>
              <td class="px-3 py-3 text-xs">{{ task.group_name || task.group_id }} / {{ task.account_name || task.account_id }}</td>
              <td class="px-3 py-3 text-xs"><span class="font-medium">{{ task.model }}</span><div class="mt-1 text-slate-500">{{ videoProviderLabel(task.provider) }}</div><div class="mt-1 text-slate-500">{{ videoUsageLabel(task) }}</div></td>
              <td class="px-3 py-3"><span class="rounded-full px-2 py-0.5 text-xs" :class="statusMeta[task.status]">{{ statusLabel[task.status] }}</span><div v-for="code in task.exception_codes" :key="code" class="mt-1 text-xs text-amber-700 dark:text-amber-300">{{ exceptionLabels[code] || code }}</div><div v-if="task.deleted_at" class="mt-1 text-xs text-slate-400">已删除</div></td>
              <td class="px-3 py-3 text-right text-xs"><div>估算 {{ estimatedMoney(task.procurement?.estimated_cost) }}</div><div class="mt-1" :class="task.procurement?.actual_cost == null ? 'text-amber-700 dark:text-amber-300' : ''">实际 {{ knownMoney(task.procurement?.actual_cost) }}</div><div class="mt-1 text-slate-500">接口报告 {{ videoUpstreamCostLabel(task) }}</div></td>
              <td class="px-3 py-3 text-right text-xs">{{ videoChargeLabel(task) }}<div class="mt-1 text-slate-500">{{ videoBillingStatusLabel(task) }}</div></td>
              <td class="px-3 py-3 text-xs text-slate-500">{{ fmt(task.created_at) }}</td>
              <td class="px-3 py-3 text-right"><div class="flex min-w-[150px] flex-wrap justify-end gap-1">
                <button class="rounded px-2 py-1 text-xs text-indigo-600 disabled:opacity-40" :disabled="actionBusy" @click="openReconciliation(task)">采购对账</button>
                <button class="rounded px-2 py-1 text-xs text-indigo-600 disabled:opacity-40" :disabled="actionBusy || !canRetry(task)" :title="retryReason(task)" @click="retry(task)">恢复处理</button>
                <button class="rounded px-2 py-1 text-xs text-amber-600 disabled:opacity-40" :disabled="actionBusy || task.status === 'done' || task.status === 'error'" @click="forceFail(task)">强制失败</button>
                <button class="rounded px-2 py-1 text-xs text-rose-600 disabled:opacity-40" :disabled="actionBusy || !videoTaskCanDelete(task)" :title="task.billing_status === 'pending' ? '待结算任务不能删除' : undefined" @click="remove(task)">删除</button>
              </div></td>
            </tr>
            <tr v-if="!tasks.length && !loading"><td colspan="8" class="px-4 py-16 text-center text-slate-500">暂无符合条件的视频任务</td></tr>
          </tbody>
        </table></div>
        <div class="flex min-w-0 flex-col gap-2 border-t border-slate-100 p-3 dark:border-slate-800"><span class="text-xs text-slate-500">共 {{ total }} 条</span><Pagination class="min-w-0 w-full" v-model:page="page" v-model:page-size="pageSize" :total="total" /></div>
      </div>
    </div>
    <div v-if="detail" class="fixed inset-0 z-40 flex justify-end bg-black/30" @click.self="closeDetail">
      <section role="dialog" aria-modal="true" aria-label="任务详情" class="h-full w-full max-w-xl overflow-y-auto bg-white p-5 shadow-xl dark:bg-slate-900">
        <div class="flex items-center justify-between"><h2 class="font-semibold">任务详情</h2><button aria-label="关闭任务详情" @click="closeDetail">✕</button></div>
        <dl class="mt-4 space-y-2 text-sm">
          <div class="flex justify-between gap-3"><dt>任务 ID</dt><dd class="break-all text-right font-mono text-xs">{{ detail.task_id }}</dd></div>
          <div class="flex justify-between gap-3"><dt>上游任务 ID</dt><dd class="break-all text-right font-mono text-xs">{{ detail.upstream_id || '未取得' }}</dd></div>
          <div class="flex justify-between"><dt>用户</dt><dd>{{ detail.user_email || detail.user_id }}</dd></div>
          <div class="flex justify-between"><dt>分组 / 账号</dt><dd>{{ detail.group_name || detail.group_id }} / {{ detail.account_name || detail.account_id }}</dd></div>
          <div class="flex justify-between"><dt>模型</dt><dd>{{ detail.model }}</dd></div>
          <div class="flex justify-between"><dt>供应商</dt><dd>{{ videoProviderLabel(detail.provider) }}</dd></div>
          <div class="flex justify-between gap-3"><dt>计费用量</dt><dd class="text-right">{{ videoUsageLabel(detail) }}</dd></div>
          <div class="flex justify-between"><dt>API Key</dt><dd>{{ detail.api_key_name || detail.api_key_id }}</dd></div>
          <div class="flex justify-between"><dt>任务状态</dt><dd>{{ statusLabel[detail.status] }}</dd></div>
          <div class="flex justify-between"><dt>用户实收</dt><dd>{{ videoChargeLabel(detail) }}</dd></div>
          <div class="flex justify-between"><dt>计费模式 / 结算</dt><dd>{{ videoBillingModeLabel(detail.billing_mode) }} / {{ videoBillingStatusLabel(detail) }}</dd></div>
          <div v-if="detail.settled_at" class="flex justify-between"><dt>结算时间</dt><dd>{{ fmt(detail.settled_at) }}</dd></div>
        </dl>
        <section class="mt-5 space-y-2 rounded-lg border border-slate-200 p-3 text-sm dark:border-slate-700">
          <h3 class="font-medium">采购成本对账</h3>
          <div class="flex justify-between"><span>估算采购成本</span><span>{{ estimatedMoney(detail.procurement?.estimated_cost) }}</span></div>
          <div class="flex justify-between"><span>供应商实际扣费</span><span>{{ knownMoney(detail.procurement?.actual_cost) }}</span></div>
          <div class="flex justify-between"><span>接口报告金额（参考）</span><span>{{ videoUpstreamCostLabel(detail) }}</span></div>
          <p v-if="detail.procurement?.reference" class="break-all text-xs">凭证：{{ detail.procurement.reference }}</p>
          <p v-if="detail.procurement?.note" class="whitespace-pre-wrap break-words text-xs">备注：{{ detail.procurement.note }}</p>
          <p class="text-xs text-slate-500">对账版本 {{ detail.procurement?.revision ?? 0 }} · {{ fmt(detail.procurement?.updated_at) }}</p>
          <button class="btn btn-secondary" :disabled="actionBusy" @click="openReconciliation(detail)">采购对账</button>
        </section>
        <section v-if="detail.execution || detail.exception_codes?.length" class="mt-5 space-y-2 rounded-lg bg-amber-50 p-3 text-sm dark:bg-amber-950/30">
          <h3 class="font-medium">异常与恢复</h3><p>{{ detail.exception_codes?.map(code => exceptionLabels[code] || code).join('、') || '暂无异常' }}</p>
          <template v-if="detail.execution"><p>连续请求失败：{{ detail.execution.failure_count ?? 0 }} 次</p><p>自动处理截止：{{ fmt(detail.execution.deadline_at) }}</p><p>下次重试：{{ fmt(detail.execution.next_retry_at) }}</p><p v-if="detail.execution.paused_reason">暂停原因：{{ pauseLabels[detail.execution.paused_reason] || detail.execution.paused_reason }}</p><p v-if="detail.execution.last_error" class="break-words text-xs">{{ detail.execution.last_error }}</p></template>
          <button class="btn btn-secondary" :disabled="actionBusy || !canRetry(detail)" @click="retry(detail)">恢复处理</button><p v-if="detail.retry_unavailable_reason" class="text-xs">{{ retryReason(detail) }}</p>
        </section>
        <div v-if="detail.error_message" class="mt-4 rounded bg-rose-50 p-3 text-xs text-rose-700 dark:bg-rose-950 dark:text-rose-300">{{ detail.error_message }}</div>
        <div class="mt-4 text-sm"><h3 class="font-medium">提示词</h3><p class="mt-1 whitespace-pre-wrap break-words rounded bg-slate-50 p-2 text-xs dark:bg-slate-800">{{ detail.prompt }}</p></div>
        <section class="mt-5"><h3 class="text-sm font-medium">处理记录</h3><p v-if="!detail.operations_history?.length" class="mt-2 text-xs text-slate-500">暂无人工处理记录</p>
          <ol class="mt-2 space-y-3"><li v-for="entry in detail.operations_history" :key="entry.id" class="rounded border border-slate-200 p-3 text-xs dark:border-slate-700"><div>{{ operationLabels[entry.action] || entry.action }} · 管理员 {{ entry.actor_id }} · {{ fmt(entry.created_at) }}</div><template v-if="entry.action === 'reconcile'"><p class="mt-1">估算：{{ estimatedMoney(entry.before.estimated_cost) }} → {{ estimatedMoney(entry.after.estimated_cost) }}</p><p class="mt-1">实际：{{ knownMoney(entry.before.actual_cost) }} → {{ knownMoney(entry.after.actual_cost) }}</p><p v-if="entry.after.reference" class="mt-1 break-all">凭证：{{ entry.after.reference }}</p></template><p class="mt-1 whitespace-pre-wrap break-words">{{ entry.note }}</p></li></ol>
        </section>
      </section>
    </div>
    <div v-if="reconciliation" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" @click.self="!actionBusy && (reconciliation = null)">
      <form role="dialog" aria-modal="true" aria-label="采购成本对账" class="max-h-[90vh] w-full max-w-lg space-y-4 overflow-y-auto rounded-xl bg-white p-5 shadow-xl dark:bg-slate-900" @submit.prevent="saveReconciliation">
        <h2 class="font-semibold">采购成本对账</h2><p class="break-all font-mono text-xs text-slate-500">{{ reconciliation.task_id }}</p>
        <p class="text-xs text-slate-600 dark:text-slate-300">金额以站内额度记录，不做汇率换算。实际扣费必须有供应商账单依据；未知请留空，确认未扣费才填 0。保存仅更新采购台账，不改变用户实收。</p>
        <label class="block text-sm">估算采购成本<input v-model="estimateInput" type="number" min="0" max="1000000000" step="any" aria-label="估算采购成本" class="input mt-1" placeholder="未知留空" /></label>
        <label class="block text-sm">供应商实际扣费<input v-model="actualInput" type="number" min="0" max="1000000000" step="any" aria-label="供应商实际扣费" class="input mt-1" placeholder="未核实留空" /></label>
        <label class="block text-sm">供应商账单或扣费凭证<input v-model="referenceInput" :required="String(actualInput).trim() !== ''" maxlength="500" aria-label="供应商账单或扣费凭证" class="input mt-1" placeholder="账单编号、供应商任务 ID 或对账记录编号" /></label>
        <label class="block text-sm">处理备注<textarea v-model="noteInput" required maxlength="1000" aria-label="处理备注" class="input mt-1" rows="3" placeholder="记录金额来源、调整原因与核对结果" /></label>
        <p class="text-xs text-slate-500">当前对账版本 {{ reconciliation.procurement?.revision ?? 0 }}。清空已录入金额将恢复为未知，并保留修改历史。</p>
        <p v-if="formError" role="alert" class="text-sm text-rose-600">{{ formError }}</p>
        <div class="flex justify-end gap-2"><button type="button" class="btn btn-secondary" :disabled="actionBusy" @click="reconciliation = null">取消</button><button type="submit" class="btn btn-primary" :disabled="actionBusy">{{ actionBusy ? '保存中…' : '保存对账' }}</button></div>
      </form>
    </div>
  </AppLayout>
</template>

<style scoped>
.video-admin th,
.video-admin td:nth-child(4),
.video-admin td:nth-child(5),
.video-admin td:nth-child(6),
.video-admin td:nth-child(7) {
  white-space: nowrap;
}
.video-admin td:nth-child(2),
.video-admin td:nth-child(3) {
  min-width: 140px;
  max-width: 190px;
  overflow-wrap: anywhere;
}
</style>

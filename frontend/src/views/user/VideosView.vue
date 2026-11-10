<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import { useAppStore } from '@/stores/app'
import { videosAPI, type VideoTask, type VideoTaskStats } from '@/api/videos'

const appStore = useAppStore()
const tasks = ref<VideoTask[]>([])
let loadSequence = 0
const stats = ref<VideoTaskStats | null>(null)
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const statusFilter = ref('')
const modelFilter = ref('')
const search = ref('')
const activeTab = ref<'tasks' | 'billing'>('tasks')
const selected = ref<VideoTask | null>(null)
const downloading = ref<string>('')
const copied = ref<string>('')
const showExamples = ref(true)

let pollTimer: number | undefined
let disposed = false

const statusMeta: Record<string, { label: string; cls: string; dot: string }> = {
  queued: { label: '排队中', cls: 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300', dot: 'bg-slate-400' },
  processing: { label: '生成中', cls: 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300', dot: 'bg-amber-500 animate-pulse' },
  done: { label: '已完成', cls: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300', dot: 'bg-emerald-500' },
  error: { label: '失败', cls: 'bg-rose-50 text-rose-700 dark:bg-rose-900/30 dark:text-rose-300', dot: 'bg-rose-500' },
}

const modelOptions = computed(() => [...new Set(tasks.value.map((t) => t.model))].sort())
const hasRunning = computed(() => tasks.value.some((t) => t.status === 'queued' || t.status === 'processing'))
const billingTasks = computed(() => tasks.value.filter((t) => t.status === 'done' || t.status === 'error'))

function fmtTime(v?: string | null) {
  if (!v) return '—'
  const d = new Date(v)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}
function fmtMoney(v: number) {
  return `$${(v ?? 0).toFixed(4)}`
}
function shortId(id: string) {
  return id.length > 14 ? `${id.slice(0, 14)}…` : id
}
function statusInfo(s: string) {
  return statusMeta[s] ?? statusMeta.queued
}

async function load() {
  const sequence = ++loadSequence
  loading.value = true
  try {
    const [list, st] = await Promise.all([
      videosAPI.list({
        page: page.value,
        page_size: pageSize.value,
        status: statusFilter.value || undefined,
        model: modelFilter.value || undefined,
        search: search.value || undefined,
      }),
      videosAPI.stats(modelFilter.value ? { model: modelFilter.value } : {}),
    ])
    if (sequence !== loadSequence) return
    tasks.value = list.items ?? []
    total.value = list.total ?? 0
    stats.value = st
    if (selected.value) selected.value = tasks.value.find(t => t.task_id === selected.value?.task_id) ?? selected.value
  } catch {
    if (sequence === loadSequence) appStore.showError('加载视频任务失败，请重试')
  } finally {
    if (sequence === loadSequence) loading.value = false
  }
}

function schedulePoll() {
  if (pollTimer) window.clearTimeout(pollTimer)
  if (disposed || !hasRunning.value) return
  pollTimer = window.setTimeout(() => {
    load().finally(schedulePoll)
  }, 5000)
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = text
    window.setTimeout(() => (copied.value = ''), 800)
  } catch {
    /* clipboard unavailable */
  }
}

async function download(task: VideoTask) {
  downloading.value = task.task_id
  try {
    const blob = await videosAPI.download(task.task_id)
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${task.model}-${task.task_id.slice(4, 12)}.mp4`
    a.click()
    URL.revokeObjectURL(url)
  } catch {
    appStore.showError('下载视频失败，请稍后重试')
  } finally {
    downloading.value = ''
  }
}

async function remove(task: VideoTask) {
  if (!window.confirm('删除后不可恢复，历史账单仍会保留。确认删除？')) return
  try {
    await videosAPI.remove(task.task_id)
    if (selected.value?.task_id === task.task_id) selected.value = null
    await load()
  } catch { appStore.showError('删除任务失败') }
}

async function openDetail(task: VideoTask) {
  try { selected.value = await videosAPI.get(task.task_id) } catch { appStore.showError('加载任务详情失败') }
}

watch([statusFilter, modelFilter], () => {
  page.value = 1
  load().finally(schedulePoll)
})
watch([page, pageSize], () => load().finally(schedulePoll))

onMounted(() => {
  load().finally(schedulePoll)
})
onUnmounted(() => {
  disposed = true
  loadSequence++
  if (pollTimer) window.clearTimeout(pollTimer)
})

type ApiSample = {
  key: string
  label: string
  title: string
  endpoint: string
  code: string
  params: { name: string; type: string; required: string; desc: string }[]
}

const activeSample = ref('minimax_t2v')

const apiSamples: ApiSample[] = [
  {
    key: 'minimax_t2v',
    label: 'MiniMax-H3 文生',
    title: 'MiniMax-H3 · 文生视频（/v1/videos/generations）',
    endpoint: 'POST /v1/videos/generations',
    code: `# MiniMax-H3 文生视频（异步：立即返回 task id）
curl -X POST "$BASE/v1/videos/generations" \\
  -H "Authorization: Bearer $API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "MiniMax-H3",
    "prompt": "a cat running on grass, cinematic lighting",
    "duration": 5,
    "resolution": "768p"
  }'

# → {"id":"mmv_73663385...","status":"queued","model":"MiniMax-H3"}

# 轮询
curl "$BASE/v1/videos/mmv_73663385..." -H "Authorization: Bearer $API_KEY"

# 下载
curl -L "$BASE/v1/videos/mmv_73663385.../content" \\
  -H "Authorization: Bearer $API_KEY" -o video.mp4`,
    params: [
      { name: 'model', type: 'string', required: '是', desc: '固定 MiniMax-H3' },
      { name: 'prompt', type: 'string', required: '是', desc: '提示词' },
      { name: 'duration', type: 'int', required: '否', desc: '秒数，默认 5' },
      { name: 'resolution', type: 'string', required: '否', desc: 'MiniMax-H3 仅支持 768p，默认 768p' },
    ],
  },
  {
    key: 'minimax_i2v',
    label: 'MiniMax-H3 图生',
    title: 'MiniMax-H3 · 图生视频（首帧驱动）',
    endpoint: 'POST /v1/videos/generations',
    code: `# 图生视频：多传 image_url（首帧图），其余参数与文生一致
curl -X POST "$BASE/v1/videos/generations" \\
  -H "Authorization: Bearer $API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "MiniMax-H3",
    "prompt": "the cat starts running, camera pans right",
    "image_url": "https://example.com/first-frame.png",
    "duration": 5
  }'

# 也支持 OpenAI 多模态写法：
# {"content":[{"type":"text","text":"..."},{"type":"image_url","image_url":{"url":"..."}}]}`,
    params: [
      { name: 'image_url', type: 'string', required: '是', desc: '首帧图 URL，支持多张（数组）' },
      { name: 'prompt', type: 'string', required: '否', desc: '动作描述；仅传图时按图续写' },
      { name: 'duration', type: 'int', required: '否', desc: '秒数，默认 5' },
    ],
  },
  {
    key: 'seedance_t2v',
    label: 'Seedance 文生',
    title: 'Seedance（国际版）· 文生视频（原生 Ark 协议）',
    endpoint: 'POST /api/v3/contents/generations/tasks',
    code: `# Seedance 走火山 / BytePlus 原生任务协议，与 /v1/videos 完全不同
curl -X POST "$BASE/api/v3/contents/generations/tasks" \\
  -H "Authorization: Bearer $API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "seedance-1-0-pro-250528",
    "content": [
      { "type": "text", "text": "a cat running on grass --resolution 720p --duration 5" }
    ]
  }'

# → {"id":"cgt-2026..."}  查询：
curl "$BASE/api/v3/contents/generations/tasks/cgt-2026..." \\
  -H "Authorization: Bearer $API_KEY"`,
    params: [
      { name: 'model', type: 'string', required: '是', desc: 'seedance-1-0-pro / -lite-t2v' },
      { name: 'content[].type', type: 'string', required: '是', desc: 'text 或 image_url' },
      { name: 'content[].text', type: 'string', required: '是', desc: '提示词，可用 --resolution/--duration 追加参数' },
    ],
  },
  {
    key: 'seedance_i2v',
    label: 'Seedance 图生',
    title: 'Seedance（国际版）· 图生视频（首帧 + 文本）',
    endpoint: 'POST /api/v3/contents/generations/tasks',
    code: `# 图生视频：content 里同时放 image_url 与 text
curl -X POST "$BASE/api/v3/contents/generations/tasks" \\
  -H "Authorization: Bearer $API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "seedance-1-0-lite-i2v-250428",
    "content": [
      { "type": "image_url", "image_url": { "url": "https://example.com/frame.png" } },
      { "type": "text", "text": "the cat starts running --resolution 720p" }
    ]
  }'`,
    params: [
      { name: 'model', type: 'string', required: '是', desc: 'seedance-1-0-lite-i2v-250428' },
      { name: 'content[].image_url.url', type: 'string', required: '是', desc: '首帧图 URL' },
      { name: 'content[].text', type: 'string', required: '否', desc: '动作描述' },
    ],
  },
]

const currentSample = computed(() => apiSamples.find((s) => s.key === activeSample.value) ?? apiSamples[0])

</script>

<template>
  <AppLayout>
    <div class="mx-auto w-full max-w-7xl space-y-5 px-4 py-6">
      <!-- 标题 -->
      <div class="flex items-center justify-between">
        <div>
          <h1 class="text-xl font-semibold text-slate-900 dark:text-slate-100">视频任务</h1>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
            MiniMax H3 的生成任务与计费记录；Seedance 通过下方原生 API 查询，扣费见用量记录
          </p>
        </div>
        <button
          class="rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-600 hover:bg-slate-50 dark:border-slate-700 dark:text-slate-300 dark:hover:bg-slate-800"
          :disabled="loading"
          @click="load()"
        >
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>

      <!-- 概览卡 -->
      <div class="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500 dark:text-slate-400">今日生成</div>
          <div class="mt-2 text-2xl font-semibold text-slate-900 dark:text-slate-100">{{ stats?.today ?? 0 }}</div>
        </div>
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500 dark:text-slate-400">生成中</div>
          <div class="mt-2 text-2xl font-semibold text-amber-600 dark:text-amber-400">
            {{ (stats?.queued ?? 0) + (stats?.processing ?? 0) }}
          </div>
        </div>
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500 dark:text-slate-400">累计消费</div>
          <div class="mt-2 text-2xl font-semibold text-slate-900 dark:text-slate-100">
            {{ fmtMoney(stats?.billed_cost_usd ?? 0) }}
          </div>
        </div>
        <div class="rounded-2xl border border-slate-200/70 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900">
          <div class="text-xs text-slate-500 dark:text-slate-400">成功率</div>
          <div class="mt-2 text-2xl font-semibold text-emerald-600 dark:text-emerald-400">
            {{ (stats?.success_rate ?? 0).toFixed(1) }}%
          </div>
        </div>
      </div>

      <!-- Tab -->
      <div class="flex gap-1 rounded-xl bg-slate-100 p-1 dark:bg-slate-800/60">
        <button
          v-for="tab in [{ k: 'tasks', l: '任务列表' }, { k: 'billing', l: '扣费记录' }]"
          :key="tab.k"
          class="flex-1 rounded-lg px-3 py-2 text-sm font-medium transition"
          :class="activeTab === tab.k
            ? 'bg-white text-slate-900 shadow-sm dark:bg-slate-900 dark:text-slate-100'
            : 'text-slate-500 hover:text-slate-700 dark:text-slate-400'"
          @click="activeTab = tab.k as 'tasks' | 'billing'"
        >
          {{ tab.l }}
        </button>
      </div>

      <!-- 筛选 -->
      <div class="flex flex-wrap items-center gap-2">
        <select
          v-model="statusFilter"
          class="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900"
        >
          <option value="">全部状态</option>
          <option value="queued">排队中</option>
          <option value="processing">生成中</option>
          <option value="done">已完成</option>
          <option value="error">失败</option>
        </select>
        <select
          v-model="modelFilter"
          class="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900"
        >
          <option value="">全部模型</option>
          <option v-for="m in modelOptions" :key="m" :value="m">{{ m }}</option>
        </select>
        <input
          v-model="search"
          placeholder="搜索任务 ID 或提示词"
          class="min-w-[220px] flex-1 rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900"
          @keyup.enter="page = 1; load()"
        />
      </div>

      <!-- 任务表 -->
      <div v-if="activeTab === 'tasks'" class="overflow-hidden rounded-2xl border border-slate-200/70 bg-white shadow-sm dark:border-slate-800 dark:bg-slate-900">
        <table class="w-full text-sm">
          <thead class="bg-slate-50 text-xs uppercase text-slate-500 dark:bg-slate-800/60 dark:text-slate-400">
            <tr>
              <th class="px-4 py-3 text-left">任务</th>
              <th class="px-4 py-3 text-left">模型</th>
              <th class="hidden px-4 py-3 text-left xl:table-cell">提示词</th>
              <th class="px-4 py-3 text-left">状态</th>
              <th class="px-4 py-3 text-left">规格</th>
              <th class="px-4 py-3 text-right">费用</th>
              <th class="px-4 py-3 text-left">创建时间</th>
              <th class="px-4 py-3 text-right">操作</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
            <tr v-for="t in tasks" :key="t.task_id" class="hover:bg-slate-50/70 dark:hover:bg-slate-800/40">
              <td class="px-4 py-3">
                <button class="font-mono text-xs text-slate-700 hover:text-indigo-600 dark:text-slate-300" @click="openDetail(t)">
                  {{ shortId(t.task_id) }}
                </button>
                <button class="ml-2 text-xs text-slate-400 hover:text-slate-600" @click="copy(t.task_id)">
                  {{ copied === t.task_id ? '已复制' : '复制' }}
                </button>
              </td>
              <td class="px-4 py-3">
                <span class="rounded-md bg-indigo-50 px-2 py-0.5 text-xs text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-300">
                  {{ t.model }}
                </span>
              </td>
              <td class="hidden max-w-[240px] truncate px-4 py-3 text-slate-600 xl:table-cell dark:text-slate-400" :title="t.prompt">
                {{ t.prompt }}
              </td>
              <td class="px-4 py-3">
                <span class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs" :class="statusInfo(t.status).cls">
                  <span class="h-1.5 w-1.5 rounded-full" :class="statusInfo(t.status).dot" />
                  {{ statusInfo(t.status).label }}
                </span>
              </td>
              <td class="px-4 py-3 text-slate-600 dark:text-slate-400">
                {{ t.resolution || '—' }} · {{ t.duration_seconds || '—' }}s
              </td>
              <td class="px-4 py-3 text-right font-mono">
                <span v-if="t.status === 'done'">{{ fmtMoney(t.billed_cost_usd) }}</span>
                <span v-else-if="t.status === 'error'" class="text-xs text-slate-400">未扣费</span>
                <span v-else class="text-slate-300">—</span>
              </td>
              <td class="px-4 py-3 text-slate-500 dark:text-slate-400">{{ fmtTime(t.created_at) }}</td>
              <td class="px-4 py-3 text-right">
                <button
                  class="rounded-md px-2 py-1 text-xs text-indigo-600 hover:bg-indigo-50 disabled:text-slate-300 dark:hover:bg-indigo-900/30"
                  :disabled="t.status !== 'done' || downloading === t.task_id"
                  @click="download(t)"
                >
                  {{ downloading === t.task_id ? '下载中…' : '下载' }}
                </button>
                <button
                  class="rounded-md px-2 py-1 text-xs text-rose-600 hover:bg-rose-50 disabled:text-slate-300 dark:hover:bg-rose-900/30"
                  :disabled="t.status === 'queued' || t.status === 'processing'"
                  @click="remove(t)"
                >
                  删除
                </button>
              </td>
            </tr>
            <tr v-if="!tasks.length && !loading">
              <td colspan="8" class="px-4 py-16 text-center">
                <div class="text-sm text-slate-500 dark:text-slate-400">还没有视频任务</div>
                <div class="mt-1 text-xs text-slate-400">用 MiniMax-H3 生成你的第一段视频</div>
              </td>
            </tr>
          </tbody>
        </table>
        <div class="flex items-center justify-between border-t border-slate-100 px-4 py-3 dark:border-slate-800">
          <span class="text-xs text-slate-500 dark:text-slate-400">共 {{ total }} 条</span>
          <Pagination v-model:page="page" v-model:page-size="pageSize" :total="total" />
        </div>
      </div>

      <!-- 扣费记录 -->
      <div v-else class="overflow-hidden rounded-2xl border border-slate-200/70 bg-white shadow-sm dark:border-slate-800 dark:bg-slate-900">
        <table class="w-full text-sm">
          <thead class="bg-slate-50 text-xs uppercase text-slate-500 dark:bg-slate-800/60 dark:text-slate-400">
            <tr>
              <th class="px-4 py-3 text-left">时间</th>
              <th class="px-4 py-3 text-left">任务</th>
              <th class="px-4 py-3 text-left">模型</th>
              <th class="px-4 py-3 text-left">时长</th>
              <th class="px-4 py-3 text-right">上游成本</th>
              <th class="px-4 py-3 text-right">实扣</th>
              <th class="px-4 py-3 text-left">模式</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
            <tr v-for="t in billingTasks" :key="t.task_id">
              <td class="px-4 py-3 text-slate-500 dark:text-slate-400">{{ fmtTime(t.completed_at || t.created_at) }}</td>
              <td class="px-4 py-3 font-mono text-xs">{{ shortId(t.task_id) }}</td>
              <td class="px-4 py-3">{{ t.model }}</td>
              <td class="px-4 py-3">{{ t.duration_seconds }}s</td>
              <td class="px-4 py-3 text-right font-mono">{{ fmtMoney(t.upstream_cost_usd) }}</td>
              <td class="px-4 py-3 text-right font-mono">{{ fmtMoney(t.billed_cost_usd) }}</td>
              <td class="px-4 py-3 text-xs text-slate-500">
                {{ t.billing_mode === 'upstream_cost' ? '跟随上游' : '按单价' }}
              </td>
            </tr>
            <tr v-if="!billingTasks.length">
              <td colspan="7" class="px-4 py-16 text-center text-sm text-slate-500 dark:text-slate-400">暂无扣费记录</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- API 示例 -->
      <div class="rounded-2xl border border-slate-200/70 bg-white shadow-sm dark:border-slate-800 dark:bg-slate-900">
        <button
          class="flex w-full items-center justify-between px-4 py-3 text-left"
          @click="showExamples = !showExamples"
        >
          <span class="text-sm font-medium text-slate-900 dark:text-slate-100">API 调用示例</span>
          <span class="text-xs text-slate-400">{{ showExamples ? '收起' : '展开' }}</span>
        </button>
        <div v-if="showExamples" class="space-y-4 border-t border-slate-100 px-4 py-4 dark:border-slate-800">
          <!-- 协议切换 -->
          <div class="flex flex-wrap gap-1 rounded-xl bg-slate-100 p-1 dark:bg-slate-800/60">
            <button
              v-for="smp in apiSamples"
              :key="smp.key"
              class="rounded-lg px-3 py-1.5 text-xs font-medium transition"
              :class="activeSample === smp.key
                ? 'bg-white text-slate-900 shadow-sm dark:bg-slate-900 dark:text-slate-100'
                : 'text-slate-500 hover:text-slate-700 dark:text-slate-400'"
              @click="activeSample = smp.key"
            >
              {{ smp.label }}
            </button>
          </div>

          <div>
            <div class="mb-2 flex items-center justify-between">
              <span class="text-xs font-medium text-slate-600 dark:text-slate-300">{{ currentSample.title }}</span>
              <button class="text-xs text-indigo-600 hover:underline" @click="copy(currentSample.code)">
                {{ copied === currentSample.code ? '已复制' : '复制' }}
              </button>
            </div>
            <div class="mb-2 rounded-md bg-slate-50 px-2 py-1 font-mono text-[11px] text-slate-600 dark:bg-slate-800 dark:text-slate-300">
              {{ currentSample.endpoint }}
            </div>
            <pre class="max-h-[420px] overflow-auto rounded-xl bg-slate-900 p-4 text-xs leading-relaxed text-slate-100"><code>{{ currentSample.code }}</code></pre>
          </div>

          <!-- 参数表 -->
          <div class="overflow-hidden rounded-xl border border-slate-200 dark:border-slate-700">
            <table class="w-full text-xs">
              <thead class="bg-slate-50 text-slate-500 dark:bg-slate-800/60 dark:text-slate-400">
                <tr>
                  <th class="px-3 py-2 text-left">参数</th>
                  <th class="px-3 py-2 text-left">类型</th>
                  <th class="px-3 py-2 text-left">必填</th>
                  <th class="px-3 py-2 text-left">说明</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
                <tr v-for="p in currentSample.params" :key="p.name">
                  <td class="px-3 py-2 font-mono text-slate-700 dark:text-slate-300">{{ p.name }}</td>
                  <td class="px-3 py-2 text-slate-500">{{ p.type }}</td>
                  <td class="px-3 py-2" :class="p.required === '是' ? 'text-rose-600' : 'text-slate-400'">{{ p.required }}</td>
                  <td class="px-3 py-2 text-slate-600 dark:text-slate-400">{{ p.desc }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="grid gap-3 text-xs text-slate-600 sm:grid-cols-2 dark:text-slate-400">
            <div class="rounded-xl border border-slate-200 p-3 dark:border-slate-700">
              <div class="font-medium text-slate-700 dark:text-slate-200">响应字段</div>
              <ul class="mt-1 space-y-1">
                <li><code>id</code> 任务 ID，用于轮询与下载</li>
                <li><code>status</code> queued / processing / done / error</li>
                <li><code>output_resolution</code> 实际分辨率</li>
                <li><code>usage.upstream_cost_usd</code> 上游真实成本</li>
              </ul>
            </div>
            <div class="rounded-xl border border-slate-200 p-3 dark:border-slate-700">
              <div class="font-medium text-slate-700 dark:text-slate-200">错误码</div>
              <ul class="mt-1 space-y-1">
                <li><code>403</code> 分组未开启视频权限</li>
                <li><code>404</code> 任务不存在或不属于你</li>
                <li><code>409</code> 视频未完成，不可下载</li>
                <li><code>503</code> 分组内无可用视频账号</li>
              </ul>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 详情抽屉 -->
    <div v-if="selected" class="fixed inset-0 z-40 flex justify-end bg-black/30" @click.self="selected = null">
      <div class="h-full w-full max-w-lg overflow-y-auto bg-white p-5 shadow-xl dark:bg-slate-900">
        <div class="flex items-center justify-between">
          <h2 class="text-base font-semibold text-slate-900 dark:text-slate-100">任务详情</h2>
          <button class="text-slate-400 hover:text-slate-600" @click="selected = null">✕</button>
        </div>

        <div class="mt-4 space-y-3 text-sm">
          <div class="flex justify-between gap-4">
            <span class="text-slate-500 dark:text-slate-400">状态</span>
            <span class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs" :class="statusInfo(selected.status).cls">
              {{ statusInfo(selected.status).label }}
            </span>
          </div>
          <div class="flex justify-between gap-4">
            <span class="text-slate-500 dark:text-slate-400">任务 ID</span>
            <span class="font-mono text-xs break-all text-right">{{ selected.task_id }}</span>
          </div>
          <div v-if="selected.upstream_id" class="flex justify-between gap-4">
            <span class="text-slate-500 dark:text-slate-400">上游 ID</span>
            <span class="font-mono text-xs break-all text-right">{{ selected.upstream_id }}</span>
          </div>
          <div class="flex justify-between gap-4">
            <span class="text-slate-500 dark:text-slate-400">模型</span>
            <span>{{ selected.model }}</span>
          </div>
          <div class="flex justify-between gap-4">
            <span class="text-slate-500 dark:text-slate-400">分辨率 / 时长</span>
            <span>{{ selected.resolution || '—' }} / {{ selected.duration_seconds }}s</span>
          </div>
          <div>
            <div class="text-slate-500 dark:text-slate-400">提示词</div>
            <div class="mt-1 rounded-lg bg-slate-50 p-2 text-xs leading-relaxed dark:bg-slate-800">{{ selected.prompt }}</div>
          </div>
          <div v-if="selected.error_message" class="rounded-lg bg-rose-50 p-2 text-xs text-rose-700 dark:bg-rose-900/30 dark:text-rose-300">
            {{ selected.error_message }}
          </div>
        </div>

        <div class="mt-5 rounded-xl border border-slate-200 p-3 dark:border-slate-700">
          <div class="mb-2 text-xs font-medium text-slate-600 dark:text-slate-300">计费明细</div>
          <div class="space-y-1 text-sm">
            <div class="flex justify-between"><span class="text-slate-500">上游成本</span><span class="font-mono">{{ fmtMoney(selected.upstream_cost_usd) }}</span></div>
            <div class="flex justify-between"><span class="text-slate-500">计费模式</span><span>{{ selected.billing_mode === 'upstream_cost' ? '跟随上游成本' : '按秒单价' }}</span></div>
            <div class="flex justify-between font-medium"><span>实扣</span><span class="font-mono">{{ fmtMoney(selected.billed_cost_usd) }}</span></div>
          </div>
        </div>

        <div class="mt-5 flex gap-2">
          <button
            class="flex-1 rounded-lg bg-indigo-600 px-3 py-2 text-sm text-white hover:bg-indigo-700 disabled:bg-slate-300"
            :disabled="selected.status !== 'done'"
            @click="download(selected)"
          >
            {{ downloading === selected.task_id ? '下载中…' : '下载视频' }}
          </button>
          <button
            class="rounded-lg border border-rose-200 px-3 py-2 text-sm text-rose-600 hover:bg-rose-50 dark:border-rose-800"
            :disabled="selected.status === 'queued' || selected.status === 'processing'"
            @click="remove(selected)"
          >
            删除
          </button>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

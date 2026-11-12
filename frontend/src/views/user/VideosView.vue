<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import { useAppStore } from '@/stores/app'
import { videosAPI, type VideoTask, type VideoTaskStats } from '@/api/videos'
import { videoProviderLabel, videoBillingStatusLabel, videoChargeLabel, videoUsageLabel, videoTaskCanDelete, videoMoneyLabel } from '../videoTaskPresentation'

const appStore = useAppStore()
const tasks = ref<VideoTask[]>([])
let loadSequence = 0
const stats = ref<VideoTaskStats | null>(null)
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const statusFilter = ref('')
const providerFilter = ref('')
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

const modelOptions = computed(() => [...new Set([modelFilter.value, 'MiniMax-H3', 'seedance-1-0-pro-250528', 'seedance-1-0-lite-t2v-250428', 'seedance-1-0-lite-i2v-250428', ...tasks.value.map((t) => t.model)])].filter(Boolean).sort())
const hasRunning = computed(() => tasks.value.some((t) => t.status === 'queued' || t.status === 'processing' || t.billing_status === 'pending') || (stats.value?.queued ?? 0) + (stats.value?.processing ?? 0) > 0)

function fmtTime(v?: string | null) {
  if (!v) return '—'
  const d = new Date(v)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}
const fmtMoney = videoMoneyLabel
function customerBillingModeLabel(mode: string) {
  return ({ usage_tokens: '按实际 Token', local_price: '按秒计费', upstream_cost: '按请求计价' } as Record<string, string>)[mode] ?? '按分组定价'
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
        provider: providerFilter.value || undefined,
        billing_only: activeTab.value === 'billing' || undefined,
        model: modelFilter.value || undefined,
        search: search.value || undefined,
      }),
      videosAPI.stats({ model: modelFilter.value || undefined, provider: providerFilter.value || undefined }),
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
  if (downloading.value === task.task_id) return
  downloading.value = task.task_id
  try {
    const blob = await videosAPI.download(task.task_id)
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${task.model}-${task.task_id.slice(4, 12)}.mp4`
    document.body.appendChild(a)
    a.click()
    a.remove()
    window.setTimeout(() => URL.revokeObjectURL(url), 1000)
  } catch (error) {
    const status = (error as { status?: number } | null)?.status
    appStore.showError(status === 410
      ? '视频文件已过期，无法再次下载。生成记录和扣费记录仍保留。'
      : status === 502
        ? '视频文件暂时无法获取，请稍后重试或联系客服。'
        : '下载视频失败，请稍后重试')
  } finally {
    downloading.value = ''
  }
}

async function remove(task: VideoTask) {
  if (!videoTaskCanDelete(task)) return
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

watch([statusFilter, modelFilter, providerFilter, activeTab], () => {
  if (activeTab.value === 'billing' && ['queued', 'processing'].includes(statusFilter.value)) statusFilter.value = ''
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
  notes?: string[]
}

const activeSample = ref('minimax_t2v')

const h3CommonParams = [
  { name: 'model', type: 'string', required: '是', desc: '固定为 MiniMax-H3，大小写与示例一致' },
  { name: 'duration', type: 'integer', required: '否', desc: '整数 1–15 秒，默认 5；实际文件时长可能略长，按名义输出秒数计费' },
  { name: 'resolution', type: 'string', required: '否', desc: '仅 768P（也接受 768p），默认 768p；不支持 480P、720P、1080P 或 2K' },
  { name: 'ratio', type: 'string', required: '否', desc: '默认 16:9；支持 21:9、16:9、4:3、1:1、3:4、9:16。adaptive 必须带图片或视频' },
]

const h3CanvasSizes = [
  { ratio: '21:9', size: '1536 × 672' }, { ratio: '16:9', size: '1344 × 768' },
  { ratio: '4:3', size: '1024 × 768' }, { ratio: '1:1', size: '768 × 768' },
  { ratio: '3:4', size: '768 × 1024' }, { ratio: '9:16', size: '768 × 1344' },
]

function createSample(endpoint: string, body: Record<string, unknown>) {
  return `curl --fail-with-body --request POST "$BASE${endpoint}" \\\n  --header "Authorization: Bearer $API_KEY" \\\n  --header "Content-Type: application/json" \\\n  --data '${JSON.stringify(body, null, 2)}'`
}

const apiSamples: ApiSample[] = [
  {
    key: 'minimax_t2v',
    label: 'MiniMax-H3 文生',
    title: 'MiniMax-H3 · 文生视频（/v1/videos/generations）',
    endpoint: 'POST /v1/videos/generations',
    code: createSample('/v1/videos/generations', {
      model: 'MiniMax-H3', prompt: '晨光中的湖面泛起涟漪，固定镜头，自然光，无字幕。',
      duration: 5, resolution: '768P', ratio: '16:9',
    }),
    params: [
      ...h3CommonParams,
      { name: 'prompt', type: 'string', required: '是', desc: '1–7000 个 Unicode 字符；描述主体、动作、环境、运镜和声音' },
    ],
    notes: ['创建响应示意：{"id":"mmv_...","status":"queued","model":"MiniMax-H3"}。成功只表示已受理，请保存完整 id，然后切换“查询与下载”示例继续。', '也可改用 content: [{"type":"text","text":"提示词"}]。content 与顶层 prompt / image_url 不能同时传入。'],
  },
  {
    key: 'minimax_i2v',
    label: 'MiniMax-H3 图生',
    title: 'MiniMax-H3 · 图生视频（首帧驱动）',
    endpoint: 'POST /v1/videos/generations',
    code: createSample('/v1/videos/generations', {
      model: 'MiniMax-H3', prompt: '保持画面中的人物和场景，人物慢慢转头看向镜头。',
      image_url: 'https://example.com/first-frame.png', duration: 5, resolution: '768P', ratio: 'adaptive',
    }),
    params: [
      ...h3CommonParams,
      { name: 'prompt', type: 'string', required: '是', desc: '动作描述，1–7000 字；图生视频也必须提供文本' },
      { name: 'image_url', type: 'string', required: '是', desc: '一张首帧图片的公网 HTTPS 直链，请替换示例占位地址' },
    ],
    notes: ['一张 image_url 按首帧处理；图片数组会按参考图片处理，不代表首尾帧。需要首尾帧请使用下一个示例中的显式 role。', 'adaptive 根据输入视觉素材确定画幅；纯文本或纯音频请求不能使用 adaptive。'],
  },
  {
    key: 'minimax_keyframes', label: 'MiniMax-H3 首尾帧', title: 'MiniMax-H3 · 指定首帧与尾帧', endpoint: 'POST /v1/videos/generations',
    code: createSample('/v1/videos/generations', {
      model: 'MiniMax-H3', content: [
        { type: 'text', text: '镜头从首帧缓慢推进，人物自然移动到尾帧的位置，保持人物和场景一致。' },
        { type: 'image_url', image_url: { url: 'https://example.com/first-frame.png' }, role: 'first_frame' },
        { type: 'image_url', image_url: { url: 'https://example.com/last-frame.png' }, role: 'last_frame' },
      ], duration: 5, resolution: '768P', ratio: '9:16',
    }),
    params: [
      ...h3CommonParams,
      { name: 'content', type: 'array', required: '是', desc: '恰好一个 text 项；本示例再添加两张有角色的图片' },
      { name: 'content[].text', type: 'string', required: '是', desc: 'text 项的提示词，1–7000 字' },
      { name: 'content[].image_url.url', type: 'string', required: '是', desc: '公网 HTTPS 图片直链；两张图的宽高比差异不超过 2%' },
      { name: 'content[].role', type: 'string', required: '是', desc: 'first_frame / last_frame，各最多一张；可只传首帧或只传尾帧' },
    ],
    notes: ['首尾帧不能与 reference_image、reference_video 或 reference_audio 混用。', '使用 content 时，不要再传顶层 prompt 或 image_url。'],
  },
  {
    key: 'minimax_reference', label: 'MiniMax-H3 多参考', title: 'MiniMax-H3 · 图片、视频与音频参考', endpoint: 'POST /v1/videos/generations',
    code: createSample('/v1/videos/generations', {
      model: 'MiniMax-H3', content: [
        { type: 'text', text: '让 <Picture 1> 中的人物沿用 <Video 1> 的动作，以 <Audio 1> 的节奏自然表演。' },
        { type: 'image_url', image_url: { url: 'https://example.com/character.png' }, role: 'reference_image' },
        { type: 'video_url', video_url: { url: 'https://example.com/motion.mp4' }, role: 'reference_video' },
        { type: 'audio_url', audio_url: { url: 'https://example.com/music.mp3' }, role: 'reference_audio' },
      ], duration: 5, resolution: '768P', ratio: '16:9',
    }),
    params: [
      ...h3CommonParams,
      { name: 'content', type: 'array', required: '是', desc: '恰好一个 text 项，最多 12 个媒体项；不可再传顶层 prompt / image_url' },
      { name: 'image_url / reference_image', type: 'media item', required: '否', desc: '最多 9 张；用 <Picture 1>、<Picture 2> 引用' },
      { name: 'video_url / reference_video', type: 'media item', required: '否', desc: '最多 3 段；用 <Video 1> 引用；裁切后累计不超过 15 秒' },
      { name: 'audio_url / reference_audio', type: 'media item', required: '否', desc: '最多 3 段；用 <Audio 1> 引用；裁切后累计不超过 15 秒' },
    ],
    notes: ['编号按同一媒体类型在 content 中出现的顺序分别从 1 开始；不要按整个数组的序号引用。', '可以只用部分参考类型。纯音频参考仍需一个文本项，并使用固定画幅。参考素材不会延长请求的输出秒数。'],
  },
  {
    key: 'minimax_query', label: 'MiniMax-H3 查询与下载', title: 'MiniMax-H3 · 保存任务 ID、轮询状态、下载 MP4', endpoint: 'GET /v1/videos/{id} · GET /v1/videos/{id}/content',
    code: `# 把创建响应中的完整 id 原样填入；不要使用示例省略号
TASK_ID="mmv_替换为创建返回的完整ID"

# 每隔 5–10 秒查询一次；status=done 才能下载
curl --fail-with-body "$BASE/v1/videos/$TASK_ID" \\
  --header "Authorization: Bearer $API_KEY"

# 成功响应示意（金额以实际记录为准）
# {"id":"mmv_...","status":"done","model":"MiniMax-H3",
#  "duration":5,"resolution":"768P","ratio":"16:9",
#  "video":{"url":"/v1/videos/mmv_.../content"},
#  "usage":{"output_seconds":5},
#  "billing":{"mode":"local_price","billed_cost_usd":0.30}}

# 保存到本地；--fail 阻止把 HTTP 错误正文当成视频保存
curl --fail --show-error --max-time 600 \\
  "$BASE/v1/videos/$TASK_ID/content" \\
  --header "Authorization: Bearer $API_KEY" \\
  --output video.mp4`,
    params: [
      { name: 'id', type: 'path string', required: '是', desc: '创建响应中的完整 mmv_ 任务 ID，查询和下载使用同一个 ID' },
      { name: 'Authorization', type: 'header', required: '是', desc: 'Bearer + 创建任务时使用的本站 API Key；其他 Key 不能访问此任务' },
    ],
    notes: ['创建接口返回 queued 不代表已完成；queued / processing 时继续查询，done 后下载，error 时查看 error.message。done 表示生成完成，结算状态请查看本页扣费记录。', '创建请求超时且没有拿到 ID 时，先在本页查找任务；不要盲目重复 POST，以免创建多个收费任务。本站当前使用轮询，不提供 callback_url 回调或 SSE。', '视频成功后请尽快下载并自行保存。结果临时保留，保留时间因模型和任务而异；重新查询不会延长保留时间，记录显示已完成也不代表文件永久可下载。'],
  },
  {
    key: 'seedance_t2v',
    label: 'Seedance 文生',
    title: 'Seedance（国际版）· 文生视频（原生 Ark 协议）',
    endpoint: 'POST /api/v3/contents/generations/tasks',
    code: `# 使用绑定 Seedance 分组的本站 API Key
curl --fail-with-body -X POST "$BASE/api/v3/contents/generations/tasks" \\
  -H "Authorization: Bearer $API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "seedance-1-0-pro-250528",
    "content": [
      { "type": "text", "text": "a cat running on grass" }
    ],
    "resolution": "720p",
    "duration": 5
  }'

# → {"id":"sdv_..."}，请保存实际返回的 id
TASK_ID="sdv_替换为创建返回的完整ID"

# 每隔 5–10 秒查询一次
curl --fail-with-body "$BASE/api/v3/contents/generations/tasks/$TASK_ID" \\
  -H "Authorization: Bearer $API_KEY"

# status=succeeded 后下载，仍需同一个 API Key
curl --fail --show-error --max-time 600 "$BASE/api/v3/contents/generations/tasks/$TASK_ID/content" \\
  -H "Authorization: Bearer $API_KEY" -o video.mp4`,
    params: [
      { name: 'model', type: 'string', required: '是', desc: '使用分组开放的完整模型 ID，例如 seedance-1-0-pro-250528' },
      { name: 'content[].type', type: 'string', required: '是', desc: 'text 或 image_url' },
      { name: 'content[].text', type: 'string', required: '是', desc: 'text 类型的提示词' },
      { name: 'resolution', type: 'string', required: '否', desc: '以所选模型支持的分辨率为准，本例使用 720p；不适用 H3 的固定 768P 规则' },
      { name: 'duration', type: 'int', required: '否', desc: '视频秒数，以所选模型支持范围为准' },
    ],
    notes: ['Seedance 使用 /api/v3/contents/generations/tasks，不能与 MiniMax H3 的请求体或路径混用。', 'queued / running 时继续查询，succeeded 后下载，failed 时读取 error.message。生成完成后请及时保存视频。'],
  },
  {
    key: 'seedance_i2v',
    label: 'Seedance 图生',
    title: 'Seedance（国际版）· 图生视频（首帧 + 文本）',
    endpoint: 'POST /api/v3/contents/generations/tasks',
    code: `# 图生视频：content 里同时放 image_url 与 text
curl --fail-with-body -X POST "$BASE/api/v3/contents/generations/tasks" \\
  -H "Authorization: Bearer $API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "seedance-1-0-lite-i2v-250428",
    "content": [
      { "type": "image_url", "image_url": { "url": "https://example.com/frame.png" }, "role": "first_frame" },
      { "type": "text", "text": "the cat starts running" }
    ],
    "resolution": "720p",
    "duration": 5
  }'`,
    params: [
      { name: 'model', type: 'string', required: '是', desc: 'seedance-1-0-lite-i2v-250428' },
      { name: 'content[].image_url.url', type: 'string', required: '是', desc: '无需登录即可读取的图片 HTTPS 直链，替换示例占位地址' },
      { name: 'content[].text', type: 'string', required: '否', desc: '动作描述' },
    ],
    notes: ['使用支持图生视频的模型；查询、下载和状态处理方式与“Seedance 文生”示例一致。'],
  },
]

const currentSample = computed(() => apiSamples.find((s) => s.key === activeSample.value) ?? apiSamples[0])
const isSeedanceSample = computed(() => currentSample.value.key.startsWith('seedance_'))

</script>

<template>
  <AppLayout>
    <div class="mx-auto w-full max-w-7xl space-y-5 px-4 py-6">
      <!-- 标题 -->
      <div class="flex items-center justify-between">
        <div>
          <h1 class="text-xl font-semibold text-slate-900 dark:text-slate-100">视频任务</h1>
          <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">
            MiniMax H3 与 Seedance 的生成任务、实际用量和结算记录
          </p>
          <a href="https://www.yunqiao.me/guide#video-start" target="_blank" rel="noopener noreferrer" class="mt-1 inline-block text-xs text-indigo-600 hover:underline">查看完整调用文档、计费说明与错误排查</a>
        </div>
        <button
          class="rounded-lg border border-slate-200 px-3 py-2 text-sm text-slate-600 hover:bg-slate-50 dark:border-slate-700 dark:text-slate-300 dark:hover:bg-slate-800"
          :disabled="loading"
          @click="load()"
        >
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>

      <p class="rounded-xl bg-amber-50 px-4 py-3 text-xs leading-relaxed text-amber-800 dark:bg-amber-900/20 dark:text-amber-200" data-testid="video-retention-notice">
        生成成功后请及时下载并自行保存。视频文件临时保留，过期后可能无法下载；历史生成记录和扣费记录仍会保留。
      </p>

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
        <select v-model="providerFilter" aria-label="视频模型系列" data-testid="video-provider-filter" class="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900">
          <option value="">全部模型系列</option>
          <option value="minimax">MiniMax H3</option>
          <option value="seedance">Seedance / BytePlus</option>
        </select>
        <select
          v-model="statusFilter"
          class="rounded-lg border border-slate-200 bg-white px-3 py-2 text-sm dark:border-slate-700 dark:bg-slate-900"
        >
          <option value="">全部状态</option>
          <option v-if="activeTab === 'tasks'" value="queued">排队中</option>
          <option v-if="activeTab === 'tasks'" value="processing">生成中</option>
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
      <div v-if="activeTab === 'tasks'" class="overflow-x-auto rounded-2xl border border-slate-200/70 bg-white shadow-sm dark:border-slate-800 dark:bg-slate-900">
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
                <div class="mt-1 text-[11px] text-slate-500">{{ videoProviderLabel(t.provider) }}</div>
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
                <span class="text-xs">{{ videoUsageLabel(t) }}</span>
              </td>
              <td class="px-4 py-3 text-right font-mono">
                <span>{{ videoChargeLabel(t) }}</span>
                <div v-if="t.status === 'done'" class="mt-1 text-[11px] text-slate-500">{{ videoBillingStatusLabel(t) }}</div>
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
                  :disabled="!videoTaskCanDelete(t)"
                  :title="t.billing_status === 'pending' ? '待结算任务不能删除' : undefined"
                  @click="remove(t)"
                >
                  删除
                </button>
              </td>
            </tr>
            <tr v-if="!tasks.length && !loading">
              <td colspan="8" class="px-4 py-16 text-center">
                <div class="text-sm text-slate-500 dark:text-slate-400">还没有视频任务</div>
                <div class="mt-1 text-xs text-slate-400">使用 MiniMax H3 或 Seedance 生成视频，任务与账单将自动显示在这里</div>
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
      <div v-else class="overflow-x-auto rounded-2xl border border-slate-200/70 bg-white shadow-sm dark:border-slate-800 dark:bg-slate-900">
        <table class="w-full text-sm">
          <thead class="bg-slate-50 text-xs uppercase text-slate-500 dark:bg-slate-800/60 dark:text-slate-400">
            <tr>
              <th class="px-4 py-3 text-left">时间</th>
              <th class="px-4 py-3 text-left">任务</th>
              <th class="px-4 py-3 text-left">模型</th>
              <th class="px-4 py-3 text-left">计费用量</th>
              <th class="px-4 py-3 text-right">实扣</th>
              <th class="px-4 py-3 text-left">模式</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-100 dark:divide-slate-800">
            <tr v-for="t in tasks" :key="t.task_id">
              <td class="px-4 py-3 text-slate-500 dark:text-slate-400">{{ fmtTime(t.completed_at || t.created_at) }}</td>
              <td class="px-4 py-3 font-mono text-xs">{{ shortId(t.task_id) }}</td>
              <td class="px-4 py-3">{{ t.model }}</td>
              <td class="px-4 py-3 text-xs">{{ videoUsageLabel(t) }}</td>
              <td class="px-4 py-3 text-right font-mono">{{ videoChargeLabel(t) }}<div class="mt-1 text-[11px] text-slate-500">{{ videoBillingStatusLabel(t) }}</div></td>
              <td class="px-4 py-3 text-xs text-slate-500">
                {{ customerBillingModeLabel(t.billing_mode) }}
              </td>
            </tr>
            <tr v-if="!tasks.length">
              <td colspan="6" class="px-4 py-16 text-center text-sm text-slate-500 dark:text-slate-400">暂无扣费记录</td>
            </tr>
          </tbody>
        </table>
        <div class="flex items-center justify-between border-t border-slate-100 px-4 py-3 dark:border-slate-800">
          <span class="text-xs text-slate-500">共 {{ total }} 条结算记录</span>
          <Pagination v-model:page="page" v-model:page-size="pageSize" :total="total" />
        </div>
      </div>

      <!-- API 示例 -->
      <div class="rounded-2xl border border-slate-200/70 bg-white shadow-sm dark:border-slate-800 dark:bg-slate-900">
        <button
          class="flex w-full items-center justify-between px-4 py-3 text-left"
          @click="showExamples = !showExamples"
        >
          <span class="text-sm font-medium text-slate-900 dark:text-slate-100">模型说明与 API 调用示例</span>
          <span class="text-xs text-slate-400">{{ showExamples ? '收起' : '展开' }}</span>
        </button>
        <div v-if="showExamples" class="space-y-4 border-t border-slate-100 px-4 py-4 dark:border-slate-800">
          <div class="rounded-xl bg-indigo-50 p-4 text-xs leading-relaxed text-indigo-950 dark:bg-indigo-950/30 dark:text-indigo-200">
            <p class="font-medium">开始前：设置本站 API 地址和 API Key</p>
            <p class="mt-1">以下为 Bash / zsh 示例；Windows 请在 WSL 或 Git Bash 中运行。BASE 填你正在使用的本站 API 根地址，不要带 /v1 或 /api/v3。API Key 必须绑定已开通对应视频模型的分组。</p>
            <pre class="mt-2 overflow-x-auto rounded-lg bg-slate-900 p-3 text-slate-100"><code>BASE="https://你的本站API域名"
API_KEY="替换为你的本站APIKey"</code></pre>
            <p class="mt-2">使用本站 API Key 完成创建、查询和下载。示例图片、视频、音频地址均为占位地址，调用前必须替换成可访问的真实素材。</p>
          </div>

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

          <section v-if="!isSeedanceSample" class="rounded-xl border border-slate-200 p-4 text-xs leading-relaxed text-slate-600 dark:border-slate-700 dark:text-slate-400" data-testid="h3-model-guide">
            <h2 class="font-semibold text-slate-900 dark:text-slate-100">MiniMax H3：文生、关键帧与多参考视频</h2>
            <p class="mt-2">仅支持 768P，输出 1–15 秒，默认 5 秒。可指定首帧、尾帧，或使用图片、视频、音频参考。输出为 MP4 / H.264，24 FPS，音频为 AAC / 32kHz / 双声道。不同画幅对应不同像素尺寸，均属于同一 768P 清晰度。</p>
            <div class="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-3">
              <div v-for="canvas in h3CanvasSizes" :key="canvas.ratio" class="rounded-lg bg-slate-50 px-3 py-2 dark:bg-slate-800">
                <span class="font-mono font-medium">{{ canvas.ratio }}</span><span class="ml-2">{{ canvas.size }}</span>
              </div>
            </div>
            <p class="mt-2">默认 16:9；带图片或视频时可用 adaptive。纯文本、纯音频参考必须使用固定画幅。按名义输出秒数计费：例如请求 1 秒，文件可能为 1.625 秒，计费用量仍为 1 秒。</p>
            <details class="mt-3 border-t border-slate-100 pt-3 dark:border-slate-800">
              <summary class="cursor-pointer font-medium text-slate-700 dark:text-slate-200">输入素材规格与角色限制</summary>
              <ul class="mt-2 list-disc space-y-2 pl-4">
                <li>content 恰好包含一个 text 项（1–7000 字）及最多 12 个媒体项；content 与顶层 prompt / image_url 互斥。快捷图生写法也必须提供 prompt。</li>
                <li>图片：静态 JPG/JPEG、PNG、WebP、HEIC/HEIF，单张不超过 30,000,000 字节；单边 256–5760 像素，总像素不超过 33,177,600，宽高比 0.4–2.5。不支持动态图片。</li>
                <li>关键帧：first_frame、last_frame 各最多一张，首尾帧宽高比差异不超过 2%；不能与任何 reference_* 角色混用。</li>
                <li>参考数量：reference_image 最多 9 张、reference_video 最多 3 段、reference_audio 最多 3 段，合计最多 12 个。每种类型单独编号，可在提示词中用 &lt;Picture 1&gt;、&lt;Video 1&gt;、&lt;Audio 1&gt; 引用。</li>
                <li>视频：MP4/MOV，H.264/H.265，单个不超过 50,000,000 字节；音频：WAV/PCM、MP3、M4A/AAC 或 ADTS AAC，单个不超过 15,000,000 字节。</li>
                <li>参考视频和音频每段至少 2 秒，超过 15 秒只取开头 15 秒；裁切后参考视频累计最多 15 秒，独立参考音频累计最多 15 秒。裁切素材不会改变请求的输出秒数。</li>
                <li>素材必须为服务端可直接下载的公网 HTTPS 地址；不支持 Base64、data URI、内网地址或需要登录的链接。创建受理后仍可能因素材格式、下载或内容审核失败，请继续查询任务状态。</li>
              </ul>
            </details>
          </section>
          <section v-else class="rounded-xl border border-slate-200 p-4 text-xs leading-relaxed text-slate-600 dark:border-slate-700 dark:text-slate-400" data-testid="seedance-model-guide">
            <h2 class="font-semibold text-slate-900 dark:text-slate-100">Seedance 国际版：选择文生或图生模型</h2>
            <p class="mt-2">使用完整的模型 ID 和 content 数组。文生可选分组开放的 Pro / Lite T2V；图生需选择支持图片输入的模型，例如 Lite I2V。具体时长、画幅和分辨率以该模型支持范围及本站模型列表为准，H3 的 768P 与 1–15 秒限制不适用于 Seedance。</p>
            <p class="mt-2">Seedance 查询状态为 queued / running / succeeded / failed；本页统一显示为排队中 / 生成中 / 已完成 / 失败。计费按所属分组的秒数或实际 Token 规则执行，以扣费记录为准。</p>
          </section>

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
            <pre class="max-h-[420px] overflow-auto rounded-xl bg-slate-900 p-4 text-xs leading-relaxed text-slate-100" data-testid="video-api-sample"><code>{{ currentSample.code }}</code></pre>
          </div>

          <ul v-if="currentSample.notes?.length" class="list-disc space-y-1 pl-4 text-xs leading-relaxed text-slate-600 dark:text-slate-400">
            <li v-for="note in currentSample.notes" :key="note">{{ note }}</li>
          </ul>

          <!-- 参数表 -->
          <div class="overflow-x-auto rounded-xl border border-slate-200 dark:border-slate-700">
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
              <ul v-if="isSeedanceSample" class="mt-1 space-y-1">
                <li><code>id</code> 网关任务 ID（sdv_），用于查询与下载</li>
                <li><code>status</code> queued / running / succeeded / failed</li>
                <li><code>content.video_url</code> 成功后的受保护下载地址</li>
                <li><code>usage.prompt_tokens / completion_tokens / total_tokens</code> 已报告的实际 Token 用量</li>
                <li><code>error.code / error.message</code> 任务失败时的错误信息</li>
              </ul>
              <ul v-else class="mt-1 space-y-1">
                <li><code>id / request_id</code> 本站 mmv_ 任务 ID，用于查询与下载</li>
                <li><code>status</code> queued / processing / done / error</li>
                <li><code>resolution / output_resolution</code> 输出清晰度；<code>ratio</code> 为画幅</li>
                <li><code>duration</code> 名义输出秒数；<code>usage.output_seconds</code> 为已报告的输出计费用量</li>
                <li><code>video.url</code> done 后返回的本站下载相对路径，拼接 BASE 并携带同一个 API Key</li>
                <li><code>created_at / completed_at</code> 创建及完成时间（完成时间仅在终态提供）</li>
                <li><code>error.message</code> 任务失败时的错误说明</li>
              </ul>
              <p class="mt-2"><code>billing.billed_cost_usd</code> 为本站已记录的用户扣费，未结算时可能为 0；完成不等于已结算，请以本页结算状态和扣费记录为准。重复查询不重复扣费。</p>
              <p class="mt-2">价格以 API Key 所属分组为准。页面 $ 和接口中的 usd 为站内额度显示，充值人民币 1 元到账 1 额度，不按美元汇率换算。</p>
            </div>
            <div class="rounded-xl border border-slate-200 p-3 dark:border-slate-700">
              <div class="font-medium text-slate-700 dark:text-slate-200">错误码</div>
              <ul class="mt-1 space-y-1">
                <li><code>400</code> 参数错误，例如非 768P、时长越界或混用输入格式</li>
                <li><code>401</code> API Key 无效或未携带鉴权</li>
                <li><code>403</code> 分组未开启视频权限</li>
                <li><code>404</code> 任务不存在，或查询 Key / 分组与创建时不一致</li>
                <li><code>409</code> 视频未完成，不可下载</li>
                <li><code>410</code> 视频文件已确认过期，生成和扣费记录仍保留</li>
                <li><code>429</code> 并发或限流限制，请按重试时间稍后重试</li>
                <li><code>502</code> 文件暂时无法获取，请稍后重试或联系客服</li>
                <li><code>503</code> 分组内无可用视频账号</li>
              </ul>
              <p class="mt-2">查询返回 HTTP 200 也可能是生成失败，必须检查 status 和 error。下载失败时先查看 HTTP 状态，不要把错误 JSON 当作 MP4。</p>
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
          <div class="flex justify-between gap-4">
            <span class="text-slate-500 dark:text-slate-400">模型</span>
            <span>{{ selected.model }}</span>
          </div>
          <div class="flex justify-between gap-4"><span class="text-slate-500">模型系列</span><span>{{ videoProviderLabel(selected.provider) }}</span></div>
          <div class="flex justify-between gap-4"><span class="text-slate-500">API Key / 分组</span><span>{{ selected.api_key_name || selected.api_key_id }} / {{ selected.group_name || selected.group_id }}</span></div>
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
            <div class="flex justify-between"><span class="text-slate-500">计费方式</span><span>{{ customerBillingModeLabel(selected.billing_mode) }}</span></div>
            <div v-if="selected.billing_mode === 'usage_tokens'" class="flex justify-between gap-3"><span class="text-slate-500">实际用量</span><span class="text-right text-xs">{{ videoUsageLabel(selected) }}</span></div>
            <div class="flex justify-between"><span class="text-slate-500">结算状态</span><span>{{ videoBillingStatusLabel(selected) }}</span></div>
            <div v-if="selected.settled_at" class="flex justify-between"><span class="text-slate-500">结算时间</span><span>{{ fmtTime(selected.settled_at) }}</span></div>
            <div class="flex justify-between font-medium"><span>实扣</span><span class="font-mono">{{ videoChargeLabel(selected) }}</span></div>
          </div>
        </div>

        <p class="mt-4 text-xs leading-relaxed text-slate-500">视频文件临时保留，请在生成成功后及时下载。文件过期不会删除生成和扣费记录。</p>

        <div class="mt-5 flex gap-2">
          <button
            class="flex-1 rounded-lg bg-indigo-600 px-3 py-2 text-sm text-white hover:bg-indigo-700 disabled:bg-slate-300"
            :disabled="selected.status !== 'done' || downloading === selected.task_id"
            @click="download(selected)"
          >
            {{ downloading === selected.task_id ? '下载中…' : '下载视频' }}
          </button>
          <button
            class="rounded-lg border border-rose-200 px-3 py-2 text-sm text-rose-600 hover:bg-rose-50 dark:border-rose-800"
            :disabled="!videoTaskCanDelete(selected)"
            :title="selected.billing_status === 'pending' ? '待结算任务不能删除' : undefined"
            @click="remove(selected)"
          >
            删除
          </button>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

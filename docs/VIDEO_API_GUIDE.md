# MiniMax H3 与 Seedance 海外版使用手册

本文面向使用本站 API 的用户与开发者，介绍 MiniMax H3 和 Seedance 海外版的调用、查询、下载与费用说明。可用模型及权限以你的控制台为准。

## 视频接入总览 {#video-start}

### 文档地址与 API 地址

| 地址 | 谁使用 | 用途 |
|---|---|---|
| `https://www.yunqiao.me/guide` | 用户、开发者 | 本使用文档，不是视频 API 路径 |
| `https://your-gateway.example` | 用户程序 | 你的分组 API Key 所属网关根地址；下文用 `GATEWAY` 表示 |

调用前请在控制台确认当前 API 地址、密钥和可用模型。API 地址与密钥必须匹配；本站密钥只用于本站网关。

### 两种模型的入口与流程

| 项目 | MiniMax H3 | Seedance / BytePlus 海外版 |
|---|---|---|
| 创建任务 | `POST /v1/videos/generations` | `POST /api/v3/contents/generations/tasks` |
| 查询任务 | `GET /v1/videos/{id}` | `GET /api/v3/contents/generations/tasks/{id}` |
| 下载视频 | `GET /v1/videos/{id}/content` | `GET /api/v3/contents/generations/tasks/{id}/content` |
| 新任务 ID | `mmv_...` | `sdv_...` |
| 请求主体 | `model`、`prompt`、`duration`、`resolution` 等 | Ark 原生 `model`、`content[]` 和视频参数 |
| 成功状态 | `done` | `succeeded` |
| 任务页面 | 统一视频任务页，可筛选供应商 | 同一页面，可筛选供应商 |
| 常见计费方式 | 按秒计费 | 按实际 Token 用量计费；具体以控制台价格为准 |

两者都是异步任务：创建返回 ID 后，任务会继续处理；用户程序通过 GET 获取进度。不要在每次等待时重新 POST 创建。

### 认证、请求格式和可用模型

每个 API 请求都带自己的网关密钥：

```http
Authorization: Bearer YOUR_GATEWAY_API_KEY
Content-Type: application/json
```

`Content-Type` 用于 POST JSON；GET 查询与下载只需要 `Authorization`。API Key 应绑定支持目标视频模型的分组，并满足余额、订阅、Key 额度和并发限制。下载链接同样需要认证，不能直接当作公开链接发送给他人。

下列 shell 示例使用 bash / zsh；Windows PowerShell 请使用 `curl.exe`，或直接运行后文 Python / Node.js 示例。

```bash
export GATEWAY="https://your-gateway.example"
export API_KEY="YOUR_GATEWAY_API_KEY"

curl --fail-with-body "$GATEWAY/v1/models" \
  -H "Authorization: Bearer $API_KEY"
```

模型列表用于确认当前密钥可见的模型。模型名区分大小写，请使用控制台展示的名称；模型不可用时，保存错误信息并联系支持。

## MiniMax H3 完整调用 {#minimax-h3}

### 适用协议与参数

这里的 `MiniMax-H3` 是本站接入的 H3 视频中转协议，采用 `/v1/videos/generations` JSON 请求。它不是 MiniMax 官方所有视频产品接口的统称，不能直接替换成其他厂商视频接口或上传文件接口。

| 参数 | 类型 | 必填 / 默认值 | 本网关行为 |
|---|---|---|---|
| `model` | string | 必填 | 示例 `MiniMax-H3`；使用控制台展示的模型名 |
| `prompt` | string | 纯文生视频必填 | 描述主体、动作、镜头、环境等 |
| `duration` | integer | 默认 5 | 秒数；网关接受整数，最终支持时长由上游模型决定，建议先使用已验证的 5 秒 |
| `resolution` | string | H3 默认 `768p` | `MiniMax-H3` 目前仅接受 `768p`；不能因为收费档叫 720p 就发送 `720p` |
| `image_url` | string 或 string[] | 可选 | 上游可访问的参考图片 URL；实际图片数量、尺寸、格式支持由供应商决定 |
| `content` | object[] | 可选替代形式 | 可包含 `text` 和 `image_url`；有 `prompt` 时以 `prompt` 为文本输入 |

兼容别名：`seconds`、`duration_seconds` 可作为时长字段，`size`、`output_resolution` 可作为分辨率字段。新接入建议只使用 `duration` 和 `resolution`；同一组别名只能传一个，即使值相同，同时传多个也会被拒绝。

本网关会将已支持的字段转换成供应商请求，不透传任意 H3 扩展字段。未支持的顶层字段在接单前返回 400。`ratio`、`seed`、音频参数、multipart 文件上传等未列出的 H3 功能不应传入，不能照搬 Seedance 请求。

### 文生视频：创建一次

```bash
curl --fail-with-body "$GATEWAY/v1/videos/generations" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MiniMax-H3",
    "prompt": "A calm blue ocean at sunset, gentle waves, slow cinematic camera movement",
    "duration": 5,
    "resolution": "768p"
  }'
```

创建成功响应示例，ID 和时间仅为示意：

```json
{
  "id": "mmv_example_task_id",
  "request_id": "mmv_example_task_id",
  "status": "queued",
  "model": "MiniMax-H3",
  "created_at": "2026-11-11T02:00:00Z"
}
```

| 返回字段 | 含义 |
|---|---|
| `id` | 本站任务 ID，后续查询和下载使用它 |
| `request_id` | 与 `id` 相同，便于兼容供应商形状 |
| `status` | `queued` 表示已接受，并不代表生成成功 |
| `model` | 你请求的公开模型名 |
| `created_at` | 创建时间，RFC3339 格式 |

创建响应目前使用 HTTP 200。是否完成应看 JSON `status`，不能把“HTTP 200”解释为“视频已经生成”。

### 图生视频：传参考图 URL

```bash
curl --fail-with-body "$GATEWAY/v1/videos/generations" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MiniMax-H3",
    "prompt": "The camera moves slowly around the product, preserving its appearance",
    "image_url": "https://your-public-assets.example/product.jpg",
    "duration": 5,
    "resolution": "768p"
  }'
```

也可把输入写成以下 `content` 形式；不要同时把同一张图片重复放在 `image_url` 和 `content` 中：

```json
{
  "model": "MiniMax-H3",
  "content": [
    {"type": "text", "text": "The camera slowly approaches the scene"},
    {"type": "image_url", "image_url": {"url": "https://your-public-assets.example/scene.jpg"}}
  ],
  "duration": 5,
  "resolution": "768p"
}
```

示例图片域名是占位符，需要替换成真实可访问的地址。需要登录、短期过期、只允许内网访问或禁止供应商下载的图片地址可能导致异步生成失败。

### 查询进度、读取错误与下载

```bash
export TASK_ID="mmv_replace_with_create_response_id"

curl --fail-with-body "$GATEWAY/v1/videos/$TASK_ID" \
  -H "Authorization: Bearer $API_KEY"
```

| `status` | 含义 | 客户端下一步 |
|---|---|---|
| `queued` | 本站已接单，尚未完成供应商执行 | 等待后查询 |
| `processing` | 正在生成 | 等待后查询 |
| `done` | 视频已生成 | 请求下载接口；在任务页查看结算状态 |
| `error` | 本次任务失败或中断 | 查看 `error.message`，不要无限轮询 |

完成响应可能包含以下字段；有条件的字段会省略：

```json
{
  "id": "mmv_example_task_id",
  "request_id": "mmv_example_task_id",
  "status": "done",
  "model": "MiniMax-H3",
  "created_at": "2026-11-11T02:00:00Z",
  "completed_at": "2026-11-11T02:02:20Z",
  "upstream_id": "provider_task_reference",
  "duration": 5,
  "resolution": "768p",
  "output_resolution": "768p",
  "video": {"url": "/v1/videos/mmv_example_task_id/content"},
  "usage": {"upstream_cost_usd": 0.375}
}
```

| 字段 | 含义 / 注意 |
|---|---|
| `upstream_id` | 供应商任务引用，用于排错；不要替代网关 `id` 去调用网关 |
| `duration` | 记录的视频秒数 |
| `resolution`、`output_resolution` | 输出规格；计费档位可能由分组映射 |
| `video.url` | 本站受保护的下载路径，通常是相对路径，需要相同 API Key |
| `usage.upstream_cost_usd` | 供应商报告的成本，不等于最终用户扣费；无可用成本时可能省略 |
| `error.message` | 失败说明；只在失败或异常状态存在 |
| `completed_at` | 完成/失败时间 |

```bash
curl --fail-with-body "$GATEWAY/v1/videos/$TASK_ID/content" \
  -H "Authorization: Bearer $API_KEY" \
  -o minimax-h3.mp4
```

下载未完成任务返回 409；下载不存在、已删除或不属于当前 Key 的任务返回 404。任务详情存储与视频文件保留是两件事，供应商文件过期后历史记录可能仍存在，应尽早下载保存。

H3 不提供原生 `DELETE /v1/videos/{id}`。如需隐藏历史记录，使用视频任务页面的“删除”；删除不会撤回已生成内容或退款，进行中的任务不能删除。

## Seedance 海外版完整调用 {#seedance-overseas}

### 海外版模型与协议

本文介绍本站提供的 Seedance 海外版接口。客户端使用本站 API 地址和 API Key，模型名称以控制台为准。示例模型包括：

| 模型名 | 用途说明 |
|---|---|
| `seedance-1-0-pro-250528` | Seedance 1.0 Pro 示例模型 |
| `seedance-1-0-lite-t2v-250428` | Seedance 1.0 Lite 文生视频示例模型 |
| `seedance-1-0-lite-i2v-250428` | Seedance 1.0 Lite 图生视频示例模型 |

不同模型支持的输入类型、参数和价格可能不同。切换模型前请确认该模型已对当前密钥开放，并查阅对应的参数说明。

### 请求参数：保留 Ark `content[]` 结构

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `model` | string | 是 | 本站公开模型名，须与当前密钥可用模型一致 |
| `content` | object[] | 是 | 非空输入数组；不是 OpenAI `messages` |
| `content[].type` | string | 是 | 常见 `text`、`image_url`；其他输入类型取决于实际模型 |
| `content[].text` | string | `type=text` 时 | 提示词文本 |
| `content[].image_url.url` | string | `type=image_url` 时 | 参考图 URL；还可使用供应商支持的其他素材形式 |
| `content[].role` | string | 可选 | 如 `first_frame`、`last_frame`、`reference_image`，须与模型能力匹配 |
| `duration` | integer | 推荐显式填写 | 生成秒数。按秒计费模式要求明确的正整数时长；模型支持范围由供应商决定 |
| `resolution` | string | 推荐显式填写 | 如 `720p`；按秒计费模式要求可定价的分辨率 |
| `ratio` | string | 可选 | 如 `16:9`，允许值取决于模型 |
| `seed` | integer | 可选 | 随机种子；不是本站任务 ID |
| `camera_fixed` | boolean | 可选 | 是否固定镜头，须模型支持 |
| `watermark` | boolean | 可选 | 是否按供应商规则添加水印，须模型支持 |
| `generate_audio` | boolean | 可选 | 是否生成音频；不是所有版本都支持 |
| `return_last_frame` | boolean | 当前不开放 true | 本站暂只提供鉴权视频下载，不提供受保护的末帧下载；传 true 会在接单前拒绝 |
| `frames` | integer | 可选 | 指定帧数的模型参数，勿与 `duration` 同时使用；按秒计费不适合未明确秒数的请求 |
| `callback_url` | string | 当前不开放 | 暂不支持回调通知；请轮询任务状态，传入此字段返回请求错误 |
| `service_tier` | string | 可选 | 供应商服务等级，是否支持取决于模型；不代表保证完成时间 |
| `execution_expires_after` | integer | 默认 172800 | 任务有效期，单位为秒；本站接受 3600–259200，默认 48 小时 |

除上表明确限制的参数外，Ark 扩展字段由网关保留并交给供应商验证。本站接受的 `content[].type` 为 `text`、`image_url`、`video_url`、`audio_url`；所选模型允许时，也可只提交音频/视频输入。素材须符合所选模型的输入要求和使用规则。供应商其他内容类型暂不开放。本站分辨率接受 480p、720p、1080p；暂不支持更高分辨率。支持字段透传不代表本站承诺该模型支持所有字段；任务查询以本文列出的返回字段为准。

优先在 JSON 顶层传 `duration`、`resolution` 等结构化参数。兼容旧提示词参数时，避免 `--duration` / `--dur`、`--resolution` / `--rs` 与顶层参数冲突。按秒计费不能使用无法事先确定价格的自适应时长；需要自适应时长时，请选择支持该参数且按 Token 计费的模型服务。

### 文生视频

```bash
curl --fail-with-body "$GATEWAY/api/v3/contents/generations/tasks" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "seedance-1-0-pro-250528",
    "content": [
      {"type": "text", "text": "A small sailboat on a calm sea at sunrise, gentle camera movement"}
    ],
    "duration": 5,
    "resolution": "720p",
    "ratio": "16:9"
  }'
```

创建成功后读取 `id`，例如：

```json
{"id": "sdv_example_task_id"}
```

`sdv_...` 是本站任务 ID，必须保存；它不是供应商返回的原始 ID。创建响应表示网关接受任务；上游鉴权、参数、模型权限错误也可能稍后作为任务失败返回。

### 图生视频与首帧示例

```bash
curl --fail-with-body "$GATEWAY/api/v3/contents/generations/tasks" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "seedance-1-0-lite-i2v-250428",
    "content": [
      {"type": "text", "text": "The camera gently pushes into the scene, natural motion"},
      {
        "type": "image_url",
        "image_url": {"url": "https://your-public-assets.example/first-frame.jpg"},
        "role": "first_frame"
      }
    ],
    "duration": 5,
    "resolution": "720p",
    "ratio": "16:9"
  }'
```

首尾帧、参考视频、参考音频、数字资产等功能须选择支持对应输入的模型，并遵循该版本的官方限制。不要把文生视频专用的 `t2v` 模型当作图生视频模型；不要把 `image_url` 写成图片文件本地路径。

### 查询与状态映射

```bash
export TASK_ID="sdv_replace_with_create_response_id"

curl --fail-with-body "$GATEWAY/api/v3/contents/generations/tasks/$TASK_ID" \
  -H "Authorization: Bearer $API_KEY"
```

| Seedance API 状态 | 视频任务页面状态 | 含义 |
|---|---|---|
| `queued` | 排队中 | 已接单或供应商排队 |
| `running` | 生成中 | 正在运行 |
| `succeeded` | 已完成 | 成功生成，可以下载 |
| `failed` | 失败 | 生成失败或本站确认的异常终态 |

供应商的 `cancelled`、`expired` 等失败终态在统一任务中按失败处理。客户端应读取错误说明，不依赖无限的未知状态轮询。

成功查询示例：

```json
{
  "id": "sdv_example_task_id",
  "model": "seedance-1-0-pro-250528",
  "status": "succeeded",
  "content": {
    "video_url": "/api/v3/contents/generations/tasks/sdv_example_task_id/content"
  },
  "duration": 5,
  "resolution": "720p",
  "usage": {
    "completion_tokens": 12345,
    "total_tokens": 12345
  }
}
```

示例 Token 数仅演示字段，不能用于预估所有 5 秒视频的费用。实际用量由供应商返回；不从时长、帧率或分辨率虚构 Token 数。

| 返回字段 | 说明 |
|---|---|
| `id` | 本站 `sdv_` 任务 ID |
| `model` | 任务对应模型名称 |
| `status` | 本站对外的 Ark 风格任务状态 |
| `content.video_url` | 本站鉴权下载路径，并非无认证的永久 CDN 地址 |
| `usage.prompt_tokens` | 供应商报告的输入 Token；BytePlus 常规视频为 0，未报告时不虚构 |
| `usage.completion_tokens` | 供应商报告的视频输出 Token；实际 Token 计费的依据 |
| `usage.total_tokens` | 供应商视频用量总数；BytePlus 当前视频协议说明输入 Token 为 0，因此通常等于 completion_tokens |
| `duration`、`resolution` | 已知输出规格；按实际可取得字段返回 |
| `error.code`、`error.message` | 失败分类与说明，视响应提供；错误对象存在时应展示并停止成功路径 |
| `created_at`、`updated_at` | Unix 时间戳，秒；不要用时间差替代输出时长 |
| `upstream_id` | 已知的供应商任务引用；查询本站仍使用 `id` |
| `billing.mode`、`billing.billed_cost_usd` | 接单计费方式和已记录扣费金额；未结算时的 0 不代表免费，结合任务页结算状态判断 |

BytePlus 某些模型有最低计费用量，供应商返回的 `completion_tokens` 可能已经包含最低计费规则。本站按该实际返回值计价，不自行减去所谓“填充 Token”。

### 下载与删除

```bash
curl --fail-with-body "$GATEWAY/api/v3/contents/generations/tasks/$TASK_ID/content" \
  -H "Authorization: Bearer $API_KEY" \
  -o seedance.mp4

# 仅对已经完成或失败的本站任务使用。
curl --fail-with-body -X DELETE \
  "$GATEWAY/api/v3/contents/generations/tasks/$TASK_ID" \
  -H "Authorization: Bearer $API_KEY"
```

DELETE 用于删除本站可见记录，不能取消正在生成的任务。排队/生成中或成功但尚未完成结算时返回 409；成功删除返回 HTTP 204、无响应体，不退款、不撤销已有扣费日志。

视频文件有保留期限，具体依模型变化。历史任务记录不代表视频文件永久可下载，请在生成后及时保存。

历史任务无法查询时，请携带原任务 ID 和创建时间联系支持。

### Python：创建、轮询、下载完整示例

安装依赖：`python -m pip install requests`。设置 `GATEWAY`、`API_KEY` 和 `VIDEO_PROVIDER`；后者取 `seedance` 或 `minimax`。代码只提交一次，GET 临时失败会重试；轮询超时不会重新创建任务。

```python
import os
import time
from urllib.parse import urlsplit

import requests

base = os.environ["GATEWAY"].rstrip("/")
api_key = os.environ["API_KEY"]
provider = os.getenv("VIDEO_PROVIDER", "seedance")
if provider not in {"seedance", "minimax"}:
    raise ValueError("VIDEO_PROVIDER must be seedance or minimax")
if not urlsplit(base).netloc:
    raise ValueError("GATEWAY must be an absolute URL")

session = requests.Session()
session.headers["Authorization"] = "Bearer " + api_key
if provider == "seedance":
    create_path = "/api/v3/contents/generations/tasks"
    body = {
        "model": "seedance-1-0-pro-250528",
        "content": [{"type": "text", "text": "Gentle ocean waves at sunrise"}],
        "duration": 5,
        "resolution": "720p",
        "ratio": "16:9",
    }
    success, pending = "succeeded", {"queued", "running"}
else:
    create_path = "/v1/videos/generations"
    body = {
        "model": "MiniMax-H3",
        "prompt": "Gentle ocean waves at sunrise",
        "duration": 5,
        "resolution": "768p",
    }
    success, pending = "done", {"queued", "processing"}

created = session.post(base + create_path, json=body, timeout=(10, 60))
created.raise_for_status()
task_id = created.json()["id"]
print("Save this task ID:", task_id)
task_path = (create_path if provider == "seedance" else "/v1/videos") + "/" + task_id
deadline = time.monotonic() + 30 * 60
while time.monotonic() < deadline:
    try:
        response = session.get(base + task_path, timeout=(10, 30))
    except requests.RequestException:
        time.sleep(10)
        continue
    if response.status_code == 429 or response.status_code >= 500:
        time.sleep(10)
        continue
    response.raise_for_status()
    task = response.json()
    status = task.get("status")
    print(task_id, status)
    if status == success:
        break
    if status not in pending:
        raise RuntimeError(f"Task stopped: {task.get('error', task)}")
    time.sleep(5)
else:
    raise TimeoutError(f"Stop local polling only; keep task ID and query later: {task_id}")

# Always download from the gateway's known path. Do not forward this API key
# to an arbitrary external URL returned by another service.
with session.get(base + task_path + "/content", stream=True,
                 timeout=(10, 120), allow_redirects=False) as download:
    if download.status_code != 200:
        raise RuntimeError(f"Download failed: HTTP {download.status_code}")
    with open(provider + ".mp4", "wb") as output:
        for chunk in download.iter_content(chunk_size=1024 * 1024):
            if chunk:
                output.write(chunk)
print("Saved", provider + ".mp4")
```

把最后的 30 分钟理解为客户端的等待预算，不是供应商任务有效期。退出程序或关闭网页后，已接受的新任务仍由网关后台处理；稍后凭原 ID 查询即可。

### Node.js：同一套程序调用两种视频接口

使用 Node.js 20 或更新版本。环境变量与 Python 示例一致；保存成 `video.mjs`，运行 `node video.mjs`。

```javascript
import { createWriteStream } from 'node:fs';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';

const base = process.env.GATEWAY?.replace(/\/+$/, '');
const key = process.env.API_KEY;
const provider = process.env.VIDEO_PROVIDER || 'seedance';
if (!base || !key || !['seedance', 'minimax'].includes(provider)) {
  throw new Error('Set GATEWAY, API_KEY, and VIDEO_PROVIDER=seedance|minimax');
}
const seedance = provider === 'seedance';
const createPath = seedance ? '/api/v3/contents/generations/tasks' : '/v1/videos/generations';
const payload = seedance
  ? { model: 'seedance-1-0-pro-250528', content: [{ type: 'text', text: 'Gentle ocean waves at sunrise' }], duration: 5, resolution: '720p', ratio: '16:9' }
  : { model: 'MiniMax-H3', prompt: 'Gentle ocean waves at sunrise', duration: 5, resolution: '768p' };
const headers = { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' };
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

// Do not automatically retry this POST after an ambiguous network failure.
const created = await fetch(base + createPath, {
  method: 'POST', headers, body: JSON.stringify(payload),
  signal: AbortSignal.timeout(60000), redirect: 'error'
});
if (!created.ok) throw new Error(`Create HTTP ${created.status}: ${await created.text()}`);
const { id } = await created.json();
if (!id) throw new Error('Create response has no task id');
console.log('Save this task ID:', id);
const taskPath = (seedance ? createPath : '/v1/videos') + '/' + encodeURIComponent(id);
const pending = new Set(seedance ? ['queued', 'running'] : ['queued', 'processing']);
const deadline = Date.now() + 30 * 60 * 1000;
let finished = false;
while (Date.now() < deadline) {
  let response;
  try {
    response = await fetch(base + taskPath, {
      headers, signal: AbortSignal.timeout(30000), redirect: 'error'
    });
  } catch {
    await sleep(10000);
    continue;
  }
  if (response.status === 429 || response.status >= 500) {
    await sleep(10000);
    continue;
  }
  if (!response.ok) throw new Error(`Poll HTTP ${response.status}: ${await response.text()}`);
  const task = await response.json();
  console.log(id, task.status);
  if (task.status === (seedance ? 'succeeded' : 'done')) {
    finished = true;
    break;
  }
  if (!pending.has(task.status)) throw new Error(JSON.stringify(task.error || task));
  await sleep(5000);
}
if (!finished) throw new Error(`Polling stopped locally; query this ID later: ${id}`);
const download = await fetch(base + taskPath + '/content', {
  headers: { Authorization: `Bearer ${key}` },
  signal: AbortSignal.timeout(120000), redirect: 'error'
});
if (!download.ok || !download.body) throw new Error(`Download HTTP ${download.status}`);
await pipeline(Readable.fromWeb(download.body), createWriteStream(provider + '.mp4'));
console.log('Saved', provider + '.mp4');
```

浏览器前端不应内置长期有效的 API Key。正式产品建议由自己的服务端调用，再向授权用户提供结果。

## 计费、任务查询与日志 {#video-billing}

### 三种计费模式

计费方式和单价以控制台显示为准，不能通过请求参数自行修改。以下金额只用于解释公式，不是当前报价。

| 模式 | 用途 | 计算方式 |
|---|---|---|
| `local_price` | 明确按视频秒数定价 | 模型/分辨率的每秒单价 × 输出秒数 × 有效倍率 |
| `upstream_cost` | 按供应商实际报告成本计价 | 供应商实际报告的美元成本 × 有效倍率；必须确实能取得成本 |
| `usage_tokens` | Seedance 实际用量计费 | 供应商实际 Token 数 × 接单时的 Token 单价 × 有效倍率 |

MiniMax H3 不使用 `usage_tokens`。Seedance 的 `usage.completion_tokens` 是用量，不是美元金额；最终扣费请查看“扣费记录”。如果接口中的成本字段缺失，或任务显示“待结算”，都不能据此判断本次免费。

按百万 Token 定价时，要注意单位换算。例如输出价 **2 美元 / 100 万 Token**、实际输出 **100,000 Token**、倍率 **1**，则金额为 `100000 / 1000000 × 2 × 1 = 0.20 美元`。这只是算式示例，并非 Seedance 官方报价。

H3 示例：分组 720p 档秒价 **0.10 美元**，768p 映射到 720p 档，实际时长 5 秒、倍率 1，则用户费用为 **0.50 美元**。供应商成本即使是 0.375 美元，也不会在本地定价模式下自动替换用户费用。

任务按接单时适用的价格计费，之后调价不会重新计算已接受的任务。是否免费以控制台明确标注和最终结算结果为准。

费用字段以美元表示，最多保留 8 位小数，页面可能为阅读方便显示更少小数；小额 Token 费用不要只凭页面四舍五入后的显示判断为零。

### 何时扣款、失败与重复查询

- 创建任务前，请确认权限、余额或订阅额度，以及 API Key 的额度和并发限制。未完成的任务会占用可用额度或并发名额。
- 按 Token 或上游成本计费时，最终金额需要等待实际用量返回，可能高于预估。请保留足够余额并控制同时创建的任务数量。
- 生成成功后结算。一个任务使用余额或订阅中的一种付款来源，不会同时扣两边。
- 重复查询或刷新页面不会对同一任务重复扣款。
- 生成失败不扣成功生成费用。创建响应超时或丢失时，请先到任务页核对，不要通过重复 POST 找回原任务。
- 客户端不查询进度，也不会停止已接受任务的生成与结算。
- “已完成”表示视频生成成功；“待结算”表示费用尚未确认。实际扣款以扣费记录和余额变化为准。
- 删除历史任务只隐藏记录，不会退款。

Seedance 默认任务有效期为 48 小时。客户端停止等待不等于任务取消；遇到“结果未知”或持续查询异常时，请保留任务 ID 并联系支持核对，避免重复创建。

### 在控制台查询两种视频记录

用户打开“视频任务”，可按供应商、状态、模型和任务 ID / 提示词筛选。MiniMax H3 与 Seedance 新任务都会显示在这里；切换“扣费记录”查看已结算记录。使用日志中同时保留模型、上游模型、Token 用量或视频时长、费用和请求关联信息。

| 页面字段 | 如何理解 |
|---|---|
| 供应商 / 模型 | `minimax` 或 `seedance`，以及公开模型名 |
| 任务 ID | `mmv_` / `sdv_` 标识，排错时优先提供这个 ID |
| 状态 | 排队、生成中、完成、失败；与原生状态映射见上文 |
| 规格 | 已知分辨率、视频秒数 |
| Token | Seedance 实际返回的输入/输出用量；没有用量时不能据此推定免费 |
| 费用 / 结算状态 | 已扣金额、免费、未扣费或待结算；零与未结算不是同义词 |
| 上游成本 | 仅在供应商实际报告时展示为已知成本；未知不能当作零成本 |
| 下载 | 成功且文件仍有效时可用，需要当前用户登录 |
| 删除 | 隐藏终态历史记录，不能取消正在运行的任务 |

### 面板接口与 API Key 接口不是同一种认证

下列 `/api/v1/videos` 是已登录控制台的用户接口，使用面板会话 JWT；普通网关 API Key 不能当成 JWT：

| 方法与路径 | 作用 |
|---|---|
| `GET /api/v1/videos` | 当前用户任务列表 |
| `GET /api/v1/videos/stats` | 当前筛选条件的统计 |
| `GET /api/v1/videos/{id}` | 当前用户的单个任务 |
| `GET /api/v1/videos/{id}/content` | 当前用户下载 |
| `DELETE /api/v1/videos/{id}` | 删除终态任务的用户可见记录 |

分页参数 `page` 从 1 开始，`page_size` 默认 20、最大 100；可用筛选包括 `provider`、`status`、`model`、`search`、`group_id`、RFC3339 时间范围 `from` / `to`。`billing_only=true` 只列结算记录，仍由服务端分页。列表采用 `{ "code": 0, "data": { "items": [], "total": 0, "page": 1, "page_size": 20 } }` 包装；原生视频创建/查询接口没有这一层面板包装。

面板任务字段 `billing_status` 区分 `settled`（已结算）、`pending`（未完成结算）、`not_charged`（失败未扣费）、`legacy`（历史记录）。`settled_at` 是结算完成时间，`input_tokens` / `output_tokens` 是实际用量；`upstream_cost_reported=false` 表示供应商没有提供可信成本值。历史记录不会因此自动追加扣费。

公开 Key 查询要求原创建任务的用户、API Key 和分组；同一用户另一把 Key 不等于原 Key，改变 Key 分组后也不应当作原任务的访问凭证。面板会话则按当前登录用户查看自己的任务。不存在的任务和不属于当前身份的任务都会返回 404，不能用它判断别人的任务是否存在。

## 错误处理、重试与常见问题 {#video-errors}

### HTTP 错误与异步失败是两层结果

创建阶段的请求错误通常使用 OpenAI 风格错误对象，例如：

```json
{
  "error": {
    "message": "MiniMax-H3 supports only 768p resolution",
    "type": "invalid_request_error"
  }
}
```

不要只依赖错误消息的英文原文，优先判断 HTTP 状态及错误类型。上游错误也可能在创建已成功之后变成任务 `error` / `failed`，此时查询 HTTP 仍可为 200。

| HTTP 状态 | 常见原因 | 处理方式 |
|---|---|---|
| 400 | JSON、模型、时长、分辨率、内容或计费参数不合法 | 修正参数；H3 使用 768p；按秒模式明确时长与档位 |
| 401 | API Key / 面板登录无效、缺少认证 | 核对环境与认证类型；不要将 Key 放在 URL 参数中 |
| 402 / 403 | 余额、订阅、额度、分组权限或安全审核不允许 | 查看具体错误；补足额度或联系支持核对权限 |
| 404 | 路径错误，或任务不存在/已删除/归属不符 | 使用创建响应 ID 和原 API Key；不要用供应商 ID 替代 |
| 409 | 任务未完成却下载，或删除仍在运行的任务 | 等待终态；不要把删除当作上游取消 |
| 415 | POST Content-Type 不是 JSON | 使用 `Content-Type: application/json` |
| 429 | 用户、Key 或账号并发/速率/额度限制 | 按 Retry-After（若有）等待，降低并发 |
| 500 | 服务暂时异常 | 保存任务 ID 和时间；创建结果不明确时先核对任务页 |
| 502 | 供应商返回异常，或成片下载失败/过期 | 查询任务与供应商错误；不要无条件新建任务 |
| 503 | 当前模型服务暂不可用 | 稍后重试；持续出现时，向支持提供模型名、请求时间和错误信息 |

错误明细可能因网关策略和供应商响应而变化，表格不是固定消息文本列表。

### 推荐重试规则

- GET 状态查询可每 5–10 秒一次，网络错误、429 或 5xx 时延迟重试并设置总体等待预算。
- POST 创建没有对客户端承诺通用幂等键去重。相同请求重复提交会产生不同任务并可能分别收费。
- POST 超时或连接中断不代表没有接单。先检查视频任务页面、请求日志及已有 ID，再决定是否人工重试。
- 下载可以重试，但确认状态完成且文件仍在供应商保留期内；下载重试不应触发重新生成。
- 程序退出、客户端等待超时、网页关闭，都不会自动取消已接受任务。

### 常见疑问

**为什么 H3 请求 768p，账单写 720p 档？** 输出规格和价格档位是两件事。768p 默认按分组 720p 价格档计费，不会把输出改成 720p。

**为什么同样 5 秒的 Seedance 视频费用不一样？** 实际 Token 模式依据供应商用量，模型、分辨率、输出和供应商最低计费规则都可能影响用量；5 秒不能直接换算成固定 Token。

**为什么生成完成但费用暂时显示待结算？** 视频已生成，但费用尚未确认。请稍后查看扣费记录；不要通过删除或重复创建处理待结算任务。

**为什么另一把 API Key 查询不到我的任务？** 原生视频接口同时校验用户与创建 Key；使用原 Key，或登录同一用户的控制台查看。

**为什么视频列表有记录却下载失败？** 历史任务不会自动保存供应商视频文件副本。视频链接可能过期、被供应商回收，或下载地址不可达。

**为什么官方 Seedance 文档里的字段在本站状态里没有？** 本站采用 Ark 风格请求，但查询返回字段以本文为准，未列出的供应商扩展字段不保证返回。

**可以把查询响应的 video_url 直接放进网页 video 标签吗？** 该 URL 是鉴权接口。需要由已登录面板下载，或由自己的服务端带 Key 取回后提供授权访问；不要把长期 Key 拼进公开页面。

**如何提交有效的问题报告？** 提供任务 ID、模型、创建时间、HTTP 状态、错误对象和大致参数即可。不要把 API Key 或不必要的私人素材发到公开渠道。

### 协议参考与适用边界

- [BytePlus 创建视频任务官方文档](https://docs.byteplus.com/en/docs/modelark/create-video-generation-task-api)：模型输入、视频参数和供应商特定限制。
- [BytePlus 查询视频任务官方文档](https://docs.byteplus.com/en/docs/modelark/get-video-generation-task-api)：原生状态、输出和实际 Token 用量。
- 本站查询/下载/删除和计费语义以本文及控制台实际开放能力为准；特别是 `sdv_` ID、后台结算与删除记录语义，属于本网关提供的能力。

文档参数核对日期：2026-11-11。供应商会更新模型和限制，实际开通能力以对应模型官方说明、你的可用模型及对应服务说明为准。

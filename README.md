## usage.csv

`usage_file`（默认 `usage.csv`）为**单进程本地追加文件**：

- 每个 ai-proxy 进程必须使用独立路径；
- 多副本/多实例**不要**挂载同一 `usage.csv`（否则可能行交错、schema 轮转竞态、备份覆盖）；
- 需要集中统计时请用 Prometheus `/metrics` 或外部采集，而不是共享 CSV。

## SLO webhook

可选配置 `slo_violation_webhook`：SLO **状态变化**时异步 POST JSON。

载荷 `SLOWebhookPayload`：

```json
{
  "at": "...",
  "instance_id": "a1b2c3d4e5f607081122334455667788",
  "seq": 1,
  "entered": [{"provider":"openai","rule":"upstream_error_rate_max","generation":1,"event_id":"a1b2c3d4e5f607081122334455667788|openai|upstream_error_rate_max|1|entered", "...": "..."}],
  "resolved": []
}
```

- **仅状态变化通知**：持续违规不会每个巡检周期重复投递；恢复时发送 `resolved`
- **评估层 vs 投递层**：`active` 只驱动本地 listener 一次；webhook 失败可延期重投，不重放 listener
- **顺序与幂等（重启安全）**：
  - `instance_id`：evaluator **启动时随机生成**，进程重启后变化；**仅在同一 instance 内**比较 `seq`
  - `seq`：实际投递序号，本 instance 内每次（重）入队递增；对账后重投会 reseq
  - 每条 violation 的 `event_id`：`instance|provider|rule|generation|state`，**单条状态变化幂等键**；对账裁剪 batch 后剩余条目 ID 不变
  - 每条 violation 含 `generation`（生命周期代次）；重投前按 generation 对账
  - **消费者应**：按 `event_id` 幂等去重；按 `(instance_id, seq)` 拒绝倒序（跨 instance 不要比较 seq）
  - `CheckNow` 经互斥串行；单 worker 串行投递
  - **listener 禁止重入** `CheckNow`（同步回调，重入会死锁）
- 本地 listener：`entered` → WARN `slo violation`；`resolved` → INFO `slo recovered`
- 有界队列（64）+ 单 worker；队列满时丢弃并计数
- 单次超时 3s；网络/408/425/429/5xx 可重试（最多 3 次 attempt）；429 优先 `Retry-After`（秒或 HTTP-date，上限 30s）
- 失败不在 worker 内长 sleep：写入 `undelivered`+`NextRetry`，下轮 `CheckNow` flush（上限 32 批）
- 其他 4xx 视为永久失败；禁用自动重定向；失败写日志（host 脱敏）
- **shutdown**：`Close()` 取消共享 context，中断在途 HTTP；剩余队列与 `undelivered` **计入** `dropped` 并清空（`queue_length=0`）
- Prometheus（需挂接 evaluator，进程默认已挂）：
  - `ai_proxy_slo_webhook_dropped_total`
  - `ai_proxy_slo_webhook_queue_length`（内存队列 + undelivered）
  - `ai_proxy_slo_webhook_requests_total{result="ok|error|non_2xx|canceled"}`

## 请求 outcome 枚举

流式首包 HTTP 200 后中途失败时，HTTP 状态无法改写；以 `outcome` 区分真实结果（CSV / Prometheus / metadata 一致）：

| outcome | 含义 | 计入 upstream 错误率 |
|---------|------|---------------------|
| `success` | 正常完成 | 否 |
| `client_canceled` | 客户端取消 | 否 |
| `idle_timeout` | 流空闲超时 | 是 |
| `limit_exceeded` | 本地体/流大小限制 | 否 |
| `upstream_truncated` | 上游中途断流/无终止事件 | 是 |
| `upstream_failed` | 上游显式失败（如 `response.failed`） | 是 |
| `incomplete` | 上游未完成（如 `response.incomplete`） | 否 |
| `client_write` | 写客户端失败 | 否 |
| `conversion` | 协议转换不支持的能力 | 否 |
| `protocol` | 上游 SSE/JSON 协议损坏 | 是 |
| `error` | 其他错误（含非流式 4xx/5xx） | 否（非流式已按状态码计数） |

Prometheus: `ai_proxy_requests_total{...,status,outcome}`；`/stats` 含 `requests.by_outcome`。

# ai-proxy

轻量级本地 LLM API 网关。客户端只访问统一标准入站 path（OpenAI 与 Anthropic），代理**仅按请求 `model`** 匹配上游 provider，并在需要时做基础协议转换。请求结束后把模型、耗时、token 用量和缓存命中统计追加到 `usage.csv`，同时按轮次归档请求和响应内容。

## 功能

- 标准入站白名单：
  - OpenAI：`POST /v1/chat/completions`、`/v1/responses`、`/v1/completions`、`/v1/embeddings`，以及 `GET/POST /v1/models`
  - Anthropic：`POST /v1/messages`
  - 其它 `/v1/*` 返回 404
- **纯 model 路由**：只根据 body 中的 `model` 与各 provider 的 `models` 规则匹配；已禁用 `X-AI-Provider`、`?provider=`、`provider/model` 前缀选择
- **模型能力目录**：全局 `model_catalog` 配置上下文窗口与 `operations`；`GET /v1/models` 本地返回 `contextWindowTokens` / `maxOutputTokens` / `operations`（不透传上游）
- 双向基础协议转换：
  - OpenAI 客户端 → Anthropic 上游：`POST /v1/chat/completions` 命中 `protocol: anthropic` 时转换
  - Anthropic 客户端 → OpenAI 上游：`POST /v1/messages` 命中 `protocol: openai` 时转换
- 支持流式 SSE 转发，客户端可以边收边显示
- 非流式/流式优先读取上游 `usage`；缺失时按字符数轻量估算
- 记录缓存使用量和缓存命中率（OpenAI cached_tokens / Anthropic cache_*_input_tokens）
- 上游 4xx/5xx 错误状态码和错误体透传
- CSV 追加写入并发安全；每轮交互归档到 `interactions/{round_id}/`
- 无 provider fallback：model 必须在 `model_catalog` 唯一路由，未匹配直接 typed 4xx

## 配置

复制示例配置：

```bash
cp config.example.yaml config.yaml
```

配置文件中的 `api_key` 等字段可用 `${ENV}` 展开，但 **provider 本身必须写在 config.yaml**：

```bash
export OPENAI_API_KEY=sk-...   # 供 config 中 ${OPENAI_API_KEY} 展开
export AI_PROXY_LISTEN_ADDR=127.0.0.1:8080
cp config.example.yaml config.yaml   # 编辑 providers / model_catalog / endpoint_capabilities
make run
```

常用环境变量：

- `AI_PROXY_CONFIG`: 配置文件路径。
- `AI_PROXY_LISTEN_ADDR`: 完整监听地址，例如 `127.0.0.1:8080`。
- `AI_PROXY_PORT`: 仅修改端口，生成 `127.0.0.1:<port>`（不再绑定全部网卡；若需监听全网卡请显式设置 `AI_PROXY_LISTEN_ADDR=:8080` 并配置 `inbound_api_key`）。
- `AI_PROXY_INBOUND_API_KEY`: 入站 API Key。监听非 loopback 时**必须**配置；客户端通过 `Authorization: Bearer <key>` 或 `X-API-Key` 提交。
- `AI_PROXY_MAX_REQUEST_BODY_BYTES` / `AI_PROXY_MAX_UPSTREAM_RESPONSE_BYTES`: 请求体与上游响应大小上限。
- `AI_PROXY_MAX_STREAM_BYTES` / `AI_PROXY_MAX_SSE_LINE_BYTES`: 流式累计输出与单条 SSE 行上限。
- `AI_PROXY_ARCHIVE_FULL_CONTENT`: `true|false`，是否落盘完整请求/响应正文。
- `AI_PROXY_USAGE_FILE`: 用量 CSV 文件路径。
- `AI_PROXY_INTERACTION_DIR`: 交互归档目录，默认 `interactions`。
- `AI_PROXY_INTERACTION_RETENTION`: 保留的交互归档轮数，默认 `500`。
- `AI_PROXY_DEBUG_LOG`: 是否输出调试日志，默认 `true`。
- `AI_PROXY_LOG_FORMAT` / `LOG_FORMAT`: 日志格式，`json` 或 `text`；`text` 会按日志等级给 `level=` 字段着色。
- `AI_PROXY_REQUEST_TIMEOUT_SECONDS`: 非流式请求总超时、流式请求等待上游响应头的超时时间，默认 `300`。
- `AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS`: 流式响应读取空闲超时，默认 `300`；设为 `0` 可禁用。该值不是流式请求总时长限制，只在连续没有收到 SSE 数据时触发。
- Provider **仅**通过 `config.yaml` 的 `providers` 声明；**不支持**通过 env 注入/创建 provider。
- 配置值可用 `${ENV}` 展开（例如 `api_key: ${OPENAI_API_KEY}`），但 provider 条目本身必须写在配置文件中。
- 每个 enabled provider 必须显式配置 `endpoint_capabilities`（不得从 protocol 推断）。
- `AI_PROXY_METRICS_REMOTE_ACCESS`: 设为 `true` 开放 `/metrics` 与 `/stats` 端点的非 loopback 访问。
- `AI_PROXY_METRICS_ALLOWED_CIDRS`: 逗号分隔的 CIDR 白名单(预留,P0 阶段未启用)。

### Provider 路由（仅 model）

客户端应发送**裸模型名**，例如：

```json
{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}
```

路由规则：

1. 从请求 body 读取 exact `model`（与 `model_catalog` 展示 ID 原文匹配）
2. 在 `model_catalog` 中查找模型（启动时已保证唯一 RouteOwner）
3. 校验入站 path 对应 operation 是否在模型 `operations` 中
4. 校验 RouteOwner 的 `endpoint_capabilities`（含协议转换矩阵）是否支持该入站 path
5. 仅请求该唯一上游；**无 provider fallback / default_provider**
6. 无 model / 未登记 / operation 或 endpoint 不支持 → typed 4xx，不访问上游

**已废弃（忽略）：** `X-AI-Provider` 头、`?provider=` 查询参数、`provider/model` 前缀。

### 模型上下文查询（`GET /v1/models`）

在配置中用**全局** `model_catalog` 登记具体模型能力（各 provider 共用）。`model_catalog` 是容量、operations 与确定路由的权威：
- model id **case-fold 后全局唯一**（展示 ID 保留配置原文；`GPT-4o` 与 `gpt-4o` 不得并存）
- 每个 catalog model 必须**唯一匹配**一个 enabled provider，作为 `owned_by` / RouteOwner
- `operations` **必填**，仅允许 `chat_completions` / `embeddings`（可多选，逗号分隔）
- **operations 是执行合同**：请求在访问上游前按 exact model 与入站 path 对应 operation 校验

配置示例：

```yaml
model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
  claude-sonnet-4-20250514:
    context_window_tokens: 200000
    max_output_tokens: 8192
    operations: chat_completions
  text-embedding-3-large:
    context_window_tokens: 8192
    max_output_tokens: 8191
    operations: embeddings
```

`GET /v1/models`（`POST` 同样）由代理**本地合成** OpenAI-compatible 列表，不再转发上游。示例响应：

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-4o",
      "object": "model",
      "created": 0,
      "owned_by": "openai",
      "contextWindowTokens": 128000,
      "maxOutputTokens": 16384,
      "operations": ["chat_completions"]
    }
  ]
}
```

说明：

- 仅 catalog 中登记的模型会出现在列表中；仅有 `gpt-*` 通配不会自动展开
- `owned_by` 为启动校验写入的确定 RouteOwner（enabled provider 名）；不存在 `ai-proxy` 回退
- `contextWindowTokens` / `maxOutputTokens` 为扩展字段；值为 0 或未配置时省略
- `operations` **始终输出**为非空数组；取值仅 `chat_completions` / `embeddings`
- 请求前校验：model 必须在 catalog 中且声明了入站 path 对应 operation，否则返回 typed 400（`model_not_found` / `operation_unsupported` 等），**不访问上游**
- path → operation：`/v1/chat/completions|/v1/messages|/v1/responses|/v1/completions` → `chat_completions`；`/v1/embeddings` → `embeddings`

其它规则：

- **不支持** `default_provider`；配置中声明将启动失败（路由仅使用 model_catalog RouteOwner）
- `enabled: false` 的 provider 不参与 model 匹配
- **不支持** `fallbacks`；配置中声明 `fallbacks` 将启动失败
- 协议转换：
  | 入站 path | 命中 provider.protocol | 行为 |
  |---|---|---|
  | OpenAI 路径 | openai（已显式声明当前 path 的 endpoint capability） | 直通 |
  | `/v1/messages` | anthropic | 直通 |
  | `/v1/chat/completions` | anthropic | OpenAI→Anthropic 文本转换（provider 须声明 `messages`）；tools/多模态/tool_calls 返回 400 |
  | `/v1/messages` | openai | Anthropic→OpenAI 文本转换（provider 须声明 `chat_completions`）；tools/多模态/tool_use 返回 400 |
  | 其它 OpenAI 路径 | anthropic | 400（不支持该端点转换） |

如果 `base_url` 已包含 `/v1`（含嵌套如 `.../codex/v1`），代理会避免重复拼接版本路径。

上游错误透传；`usage.csv` / `metadata.json` 记录实际 RouteOwner provider。流式响应在写出首包 SSE 前会探测首字节以便识别断流，但**不会**切换其它 provider。

Provider 与 model_catalog 示例（`models` 互不重叠；catalog model 必须唯一命中）：

```yaml
providers:
  openai:
    enabled: true
    protocol: openai
    base_url: https://primary.example/v1
    api_key: ${OPENAI_API_KEY}
    endpoint_capabilities: chat_completions, responses, completions, embeddings
    models: gpt-*, chatgpt-*, o*, text-embedding-*
  backup-openai:
    enabled: true
    protocol: openai
    base_url: https://backup.example/v1
    api_key: ${BACKUP_OPENAI_API_KEY}
    endpoint_capabilities: chat_completions
    models: backup-gpt-*
  deepseek:
    enabled: true
    protocol: openai
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
    endpoint_capabilities: chat_completions
    models: deepseek*
  anthropic:
    enabled: true
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    endpoint_capabilities: messages
    models: claude*
```

## 运行

```bash
make run
```

也可以指定配置文件：

```bash
AI_PROXY_CONFIG=config.yaml make run
```

客户端接入：

```text
# OpenAI-compatible 客户端
API_BASE_URL=http://127.0.0.1:8080/v1

# Anthropic 客户端
ANTHROPIC_BASE_URL=http://127.0.0.1:8080
# 或 ANTHROPIC_API_URL=http://127.0.0.1:8080
```

只需发送裸模型名；代理按 `models` 规则选上游，必要时自动做 OpenAI ↔ Anthropic 基础协议转换。

健康检查：

```bash
curl http://127.0.0.1:8080/healthz
```

可观测性端点（默认仅 loopback 访问；可通过 `metrics_remote_access: true` 放开）：

```bash
# Prometheus 文本格式
curl http://127.0.0.1:8080/metrics

# 实时聚合 JSON（p50/p75/p90/p95/p99 延迟、cache 命中率、provider 错误分布等）
curl http://127.0.0.1:8080/stats
```

`/stats` 字段参考：

```json
{
  "uptime_seconds": 1234,
  "requests": {
    "total": 493,
    "by_provider": {"aiapi-Deepseek": 107, "deepseek-v4-flash": 119, ...},
    "by_status": {"2xx": 400, "5xx": 50, "4xx": 43}
  },
  "cache": {
    "by_provider": {
      "deepseek-v4-flash": {"hit": 102, "miss": 17, "hit_rate": 0.8571, "avg_cached_tokens": 11416}
    }
  },
  "latency_ms": {
    "openai/gpt-4": {"p50": 1234, "p75": 2000, "p90": 3500, "p95": 4567, "p99": 8901}
  },
  "errors": {
    "upstream_5xx": 50,
    "upstream_timeout": 12,
    "upstream_rate_limit": 8,
    "upstream_by_status_code": {"502": 30, "504": 12, "429": 8}
  }
}
```

`/metrics` 暴露的指标名（均以 `ai_proxy_` 为前缀）：

- `ai_proxy_requests_total{provider,model,route,status}` — 请求计数
- `ai_proxy_request_duration_seconds_{sum,count}{provider,model,route,status}` — 累计耗时
- `ai_proxy_input_tokens_total{provider,model}` / `ai_proxy_output_tokens_total{provider,model}` — token 用量
- `ai_proxy_cached_input_tokens_total{provider,model}` / `ai_proxy_cache_creation_input_tokens_total{provider,model}`
- `ai_proxy_cache_hit_rate{provider,model}` — 缓存命中率
- `ai_proxy_upstream_errors_total{provider,status_code}` — upstream 错误分布

## 构建单二进制

```bash
make build
```

`Makefile` 默认使用 `-buildvcs=false`，避免当前目录不是完整 Git worktree 时 `go build` 因 VCS stamping 失败。可通过 `BINARY` 覆盖输出文件名：

```bash
make build BINARY=bin/ai-proxy
```

## 开发检查

```bash
make check
```

`make check` 会依次运行 `go fmt ./...`、`go vet ./...` 和 `go test ./...`。

## CSV 字段

`usage.csv` 首次写入会生成表头：

```text
time,provider,model,input_tokens,output_tokens,total_tokens,duration_ms,stream,estimated,http_status,outcome,cached_input_tokens,cache_creation_input_tokens,cache_hit_rate
```

`cache_hit_rate` 按 `cached_input_tokens / input_tokens` 计算，CSV 中保留 4 位小数。没有上游 `usage` 时，`estimated=true` 表示 token 用量来自本地轻量估算；缓存字段无法估算时记为 `0`。

## 交互归档

每一轮 `POST /v1/*` 会创建一个递增序号目录。默认只保留最新 `500` 轮，可通过 `server.interaction_retention` 或 `AI_PROXY_INTERACTION_RETENTION` 调整：

```text
interactions/
  000001/
    request.json
    request.meta.json
    upstream_request.json
    upstream_response.json
    response.json
    metadata.json
  000002/
    request.json
    request.meta.json
    upstream_request.json
    upstream_response.json
    response.sse
    response.json
    metadata.json
```

非流式响应通常写入 `response.json`。流式响应始终写入原始 `response.sse`；其中 Chat Completions / Completions 与 Anthropic Messages 还会整理出完整 `response.json`（合并 delta / content_block）。**Responses API（`/v1/responses`）当前只保留原始 `response.sse`**，不生成整理后的 `response.json`（事件结构与 Chat Completions 不同）。`request.meta.json` 记录客户端方法、路径、查询参数、来源地址、User-Agent、Content-Length 和脱敏后的请求头；`upstream_request.json` 与 `upstream_response.json` 记录唯一 RouteOwner 的上游请求/响应；`metadata.json` 汇总最终 provider、model、耗时、HTTP 状态、token 统计、缓存读写 token 和缓存命中率，流式响应会额外记录 `full_response_path`。

调试日志默认输出到终端，包含每轮 round id、客户端请求摘要、provider/model 选择、上游请求、上游响应和最终 token 摘要。`Authorization`、`X-API-Key`、`Cookie` 等敏感头会显示为 `<redacted>`。
最终 token 摘要也会带 `round`，并在流式读取中断、客户端写入失败等场景附带 `error`；对应错误也会写入该轮 `metadata.json`，便于并发请求交错时追踪完整生命周期。
默认日志格式为 JSON，便于日志系统采集；在 `server.log_format` 中设置 `text`，或设置 `AI_PROXY_LOG_FORMAT=text` / `LOG_FORMAT=text` 后，会输出人类可读日志，并仅对 `level=DEBUG`/`INFO`/`WARN`/`ERROR` 字段按等级着色。

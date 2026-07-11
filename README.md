# ai-proxy

轻量级本地 LLM API 网关。它接收 OpenAI-compatible `/v1/*` 请求，按 provider/model 规则转发到配置的上游，并在请求结束后把模型、耗时、token 用量和缓存命中统计追加到 `usage.csv`，同时按轮次归档请求和响应内容。

## 功能

- `POST /v1/chat/completions` OpenAI-compatible 转发。
- 支持 OpenAI-compatible `/v1/responses` 端点直通转发。
- 支持流式 SSE 转发，客户端可以边收边显示。
- 非流式响应优先读取 `usage` 字段；缺失时按字符数做轻量估算。
- 流式响应优先读取结束前的 `usage` 事件；缺失时按输入/输出字符数估算。
- 记录缓存使用量和缓存命中率，支持 OpenAI-compatible `prompt_tokens_details.cached_tokens` / `input_tokens_details.cached_tokens`，以及 Anthropic `cache_read_input_tokens` / `cache_creation_input_tokens`。
- 上游 4xx/5xx 错误状态码和错误体透传给客户端。
- CSV 追加写入并发安全。
- 每轮 `/v1/*` 交互归档到独立序号目录，例如 `interactions/000001/`。
- 支持 OpenAI-compatible 上游，例如 OpenAI、DeepSeek。
- 支持 Anthropic Messages API 的基础协议转换。
- 支持同协议 provider fallback，适合主上游限流或 5xx 时切换备用上游。

## 配置

复制示例配置：

```bash
cp config.example.yaml config.yaml
```

也可以只用环境变量：

```bash
export OPENAI_API_KEY=sk-...
export AI_PROXY_PORT=8080
make run
```

常用环境变量：

- `AI_PROXY_CONFIG`: 配置文件路径。
- `AI_PROXY_PORT`: 监听端口。
- `AI_PROXY_LISTEN_ADDR`: 完整监听地址，例如 `127.0.0.1:8080`。
- `AI_PROXY_USAGE_FILE`: 用量 CSV 文件路径。
- `AI_PROXY_INTERACTION_DIR`: 交互归档目录，默认 `interactions`。
- `AI_PROXY_INTERACTION_RETENTION`: 保留的交互归档轮数，默认 `500`。
- `AI_PROXY_DEBUG_LOG`: 是否输出调试日志，默认 `true`。
- `AI_PROXY_LOG_FORMAT` / `LOG_FORMAT`: 日志格式，`json` 或 `text`；`text` 会按日志等级给 `level=` 字段着色。
- `AI_PROXY_REQUEST_TIMEOUT_SECONDS`: 非流式请求总超时、流式请求等待上游响应头的超时时间，默认 `300`。
- `AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS`: 流式响应读取空闲超时，默认 `300`；设为 `0` 可禁用。该值不是流式请求总时长限制，只在连续没有收到 SSE 数据时触发。
- `AI_PROXY_DEFAULT_PROVIDER`: 默认 provider 名称；当请求没有显式 provider、模型规则无法唯一匹配、或 `/v1/models` 这类请求没有模型时使用。
- `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`, `ANTHROPIC_API_KEY`: provider API Key。
- `AI_PROXY_<PROVIDER>_API_KEY`, `<PROVIDER>_API_KEY`: 设置内置 provider API Key，例如 `AI_PROXY_OPENAI_API_KEY`、`DEEPSEEK_API_KEY`。
- `AI_PROXY_<PROVIDER>_BASE_URL`, `<PROVIDER>_BASE_URL`: 覆盖内置 provider Base URL。
- `AI_PROXY_<PROVIDER>_MODELS`, `<PROVIDER>_MODELS`: 覆盖内置 provider 模型匹配规则，例如 `deepseek*,gpt-*`。
- `AI_PROXY_<PROVIDER>_FALLBACKS`, `<PROVIDER>_FALLBACKS`: 覆盖内置 provider fallback 列表。
- `AI_PROXY_<PROVIDER>_ENABLED`, `<PROVIDER>_ENABLED`: 启用或禁用内置 provider。
- `AI_PROXY_METRICS_REMOTE_ACCESS`: 设为 `true` 开放 `/metrics` 与 `/stats` 端点的非 loopback 访问。
- `AI_PROXY_METRICS_ALLOWED_CIDRS`: 逗号分隔的 CIDR 白名单(预留,P0 阶段未启用)。
- `API_KEY`, `API_BASE_URL`: 创建名为 `custom` 的通用 provider；当只配置这一个 provider 时会自动使用。

请求时可以用 `X-AI-Provider` 头、`?provider=deepseek` 查询参数，或 `model` 前缀选择 provider：

```json
{"model":"deepseek/deepseek-chat","messages":[{"role":"user","content":"hi"}]}
```

转发到上游前会把模型名改写为 `deepseek-chat`。

代理会按请求解析 provider：

- 显式选择优先：`X-AI-Provider`、`?provider=`、`provider/model`。
- 请求体带 `model` 时，会优先按 provider 的 `models` 模型匹配规则自动选择；支持精确模型名和后缀 `*` 前缀匹配，例如 `deepseek*`、`gpt-*`、`kimi-*`。
- `default_provider` 可配置兜底 provider；它必须指向一个已启用的 provider，否则启动时报配置错误。兜底只在无法通过显式选择或模型规则唯一推断时使用。
- provider 可通过 `enabled: false` 禁用；禁用后不会参与自动匹配，显式选择该 provider 会返回 400。
- provider 可通过 `fallbacks: provider-a, provider-b` 配置同协议备用上游；网络错误、408、429 和 5xx 会触发 fallback，400/401/403 等客户端或鉴权错误不会切换。禁用、缺失、跨协议或重复的 fallback provider 会被跳过。
- Anthropic 请求特征：`/v1/messages` 或 `Anthropic-*` 请求头，会选择唯一的 `protocol: anthropic` provider。
- OpenAI-compatible 请求特征：`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings`、`/v1/responses`，会选择唯一的 `protocol: openai` provider。
- 如果同一协议配置了多个 provider，且没有可用的 `default_provider` 或模型规则仍无法唯一匹配，代理返回 400，要求客户端指定 provider 或补充 `models` 规则。

如果 `base_url` 已包含 `/v1`，代理会避免重复拼接版本路径。例如 `base_url: https://onlycode.shop/v1` 收到 `/v1/messages?beta=true` 时，上游 URL 会是 `https://onlycode.shop/v1/messages?beta=true`。

fallback 尝试会归档到每轮交互目录的 `fallback_attempts.json`，最终 `usage.csv` 和 `metadata.json` 记录实际返回给客户端的 provider。流式响应只有在尚未向客户端写出响应前才会 fallback；一旦开始输出 SSE，就不会中途切换 provider。

一个带 fallback 的 provider 示例：

```yaml
providers:
  openai:
    enabled: true
    protocol: openai
    base_url: https://primary.example/v1
    api_key: ${OPENAI_API_KEY}
    models: gpt-*, chatgpt-*, o*, text-embedding-*
    fallbacks: backup-openai, deepseek
  backup-openai:
    enabled: true
    protocol: openai
    base_url: https://backup.example/v1
    api_key: ${BACKUP_OPENAI_API_KEY}
    models: gpt-*
```

## 运行

```bash
make run
```

也可以指定配置文件：

```bash
AI_PROXY_CONFIG=config.yaml make run
```

客户端把 OpenAI-compatible base URL 改成：

```text
http://127.0.0.1:8080/v1
```

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
    "fallback_triggered": 30,
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
- `ai_proxy_fallback_attempts_total{from_provider,to_provider,reason}` — fallback 触发计数

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
time,provider,model,input_tokens,output_tokens,total_tokens,duration_ms,stream,estimated,http_status,cached_input_tokens,cache_creation_input_tokens,cache_hit_rate
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
    fallback_attempts.json
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

非流式响应通常写入 `response.json`；流式响应会同时写入原始 `response.sse` 和整理后的完整 `response.json`。OpenAI-compatible SSE 会合并 `delta` 为完整 `chat.completion`；Anthropic SSE 会合并为完整 Messages 响应。`request.meta.json` 记录客户端方法、路径、查询参数、来源地址、User-Agent、Content-Length 和脱敏后的请求头；`upstream_request.json` 与 `upstream_response.json` 记录最终一次上游请求/响应；`fallback_attempts.json` 记录每次尝试的 provider、协议、状态码/错误、耗时和是否为 fallback；`metadata.json` 汇总最终 provider、model、耗时、HTTP 状态、token 统计、缓存读写 token 和缓存命中率，流式响应会额外记录 `full_response_path`。

调试日志默认输出到终端，包含每轮 round id、客户端请求摘要、provider/model 选择、上游请求、上游响应和最终 token 摘要。`Authorization`、`X-API-Key`、`Cookie` 等敏感头会显示为 `<redacted>`。
最终 token 摘要也会带 `round`，并在流式读取中断、客户端写入失败等场景附带 `error`；对应错误也会写入该轮 `metadata.json`，便于并发请求交错时追踪完整生命周期。
默认日志格式为 JSON，便于日志系统采集；在 `server.log_format` 中设置 `text`，或设置 `AI_PROXY_LOG_FORMAT=text` / `LOG_FORMAT=text` 后，会输出人类可读日志，并仅对 `level=DEBUG`/`INFO`/`WARN`/`ERROR` 字段按等级着色。

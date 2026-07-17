# 配置参考

本文描述当前 `ai-proxy` 的运行配置。完整可复制示例见仓库根目录的 [`config.example.yaml`](../config.example.yaml)。配置路径默认是 `config.yaml`，也可用 `-config` 或 `AI_PROXY_CONFIG` 指定。

配置值支持 `${ENV}` 展开，例如 `api_key: ${OPENAI_API_KEY}`；环境变量只能填充值，不能创建 Provider、模型或路由。

## 最小配置

```yaml
server:
  listen_addr: 127.0.0.1:8080

usage_store:
  path: usage.duckdb

providers:
  openai:
    enabled: true
    protocol: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
    endpoint_capabilities: chat_completions
    models: gpt-4o

model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions
```

`model_catalog` 是客户端可用模型、容量、operation 与唯一 RouteOwner 的权威；模型 ID exact 且严格区分大小写。完整的端点矩阵、转换限制与 typed error 见 [Provider Capability Contract](provider-capability-contract-design-2026-07-15.md)。

## Provider 与模型路由

每个 enabled Provider 必须显式设置：

- `protocol`：`openai` 或 `anthropic`。
- `base_url`：可带或不带 `/v1`，代理会避免重复拼接。
- `endpoint_capabilities`：上游直接支持的端点能力，不能由 protocol 自动推断。
- `models`：用于启动期将 catalog model 解析为唯一 RouteOwner 的 pattern。
- `api_key`：远程 Provider 必填；仅 loopback 上游可显式 `allow_unauthenticated: true`。

一个 catalog model 必须唯一匹配一个 enabled Provider；`default_provider`、`fallbacks`、`X-AI-Provider`、`?provider=` 与 `provider/model` 前缀均不支持。Provider 管理页保存时复用同一套启动期校验，因此不会写入破坏既有模型路由的配置。

## 客户端 API Key

`client_api_keys` 是调用方身份与用量归属的唯一配置 authority：

```yaml
client_api_keys:
  codex:
    api_key: ${CODEX_API_KEY}
    enabled: true
  batch:
    api_key: ${BATCH_API_KEY}
    enabled: false
```

- Key ID 需匹配 `[a-z0-9][a-z0-9._-]{0,63}`，`default` 为内置保留 ID。
- 未携带 Key 或空 Header 归入 `default`；未知、禁用、格式错误或两个身份 Header 冲突时返回 401。
- OpenAI 使用 `Authorization: Bearer <key>`，Anthropic 使用 `X-API-Key: <key>`；两种 Header 可兼容，但同时出现时必须为同一 Key。
- 原始客户端 Key 不写入日志、DuckDB、归档或管理 API，也不会转发给上游。
- `inbound_api_key`、`AI_PROXY_INBOUND_API_KEY`、`usage_file` 与 `AI_PROXY_USAGE_FILE` 已删除，配置中出现会启动失败。

客户端 Key 是归属机制，不是非 loopback 监听的访问控制。若监听 `0.0.0.0:8080` 或 `:8080`，请在防火墙、反向代理或私有网络层实施访问控制。

## Server 配置

| 配置或环境变量 | 说明 |
| --- | --- |
| `server.listen_addr` / `AI_PROXY_LISTEN_ADDR` | 完整监听地址，默认 `127.0.0.1:8080`。 |
| `AI_PROXY_PORT` | 仅替换端口，生成 `127.0.0.1:<port>`。 |
| `max_request_body_bytes` / `AI_PROXY_MAX_REQUEST_BODY_BYTES` | 客户端请求体上限。 |
| `max_upstream_response_bytes` / `AI_PROXY_MAX_UPSTREAM_RESPONSE_BYTES` | 非流式上游响应上限。 |
| `max_stream_bytes`、`max_sse_line_bytes` | 流式累计输出与单条 SSE 行上限。 |
| `request_timeout_seconds` | 非流式总超时及流式等待响应头超时。 |
| `stream_idle_timeout_seconds` | 连续未收到 SSE 数据的超时；`0` 禁用。 |
| `archive_full_content` | 是否落盘完整请求/响应正文。 |
| `interaction_dir`、`interaction_retention` | 归档目录与保留轮数。 |
| `debug_log`、`log_format` | 调试日志和 `json`/`text` 格式。 |
| `metrics_remote_access`、`metrics_allowed_cidrs` | `/metrics`、`/stats` 的远程访问控制。 |

环境变量与配置键的完整默认值、上限和校验以 [`config.example.yaml`](../config.example.yaml) 与 `internal/config` 为准。

## DuckDB 用量存储

```yaml
usage_store:
  path: usage.duckdb
  memory_limit: 256MB
  threads: 2
  query_cache_seconds: 15
```

`usage_store.path` 是单进程本地 DuckDB 文件，也是唯一在线用量 authority；多个实例不得共享同一路径。数据库文件应由运行用户保护，且不应提交到版本库。

## 本地管理页

访问 `http://127.0.0.1:8080/admin/` 可管理 Provider、查看 API Key 用量、筛选事件和导出 CSV。

- `/admin` 及 `/admin/api/*` 永远限制 loopback，即使代理监听在非 loopback 地址。
- Provider API Key 只显示“已配置”，从不回显明文；保存时保留未修改的已有值或 `${ENV}` 表达式。
- 使用统计页查询 DuckDB，并支持按时间、API Key、Provider、Model、Outcome 和估算标记筛选。

Admin usage API 的筛选参数、导出边界与响应格式见 [API Key 用量与 DuckDB 收口方案](api-key-usage-duckdb-web-closure-plan-2026-07-17.md#17-admin-api)。

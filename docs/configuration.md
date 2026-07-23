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
  ci-agent:
    # 由本地 Admin 管理端创建的 Key 只保存 SHA-256 摘要。
    api_key_hash: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    enabled: true
  batch:
    api_key: ${BATCH_API_KEY}
    enabled: false
```

- Key ID 需匹配 `[a-z0-9][a-z0-9._-]{0,63}`，`default` 为历史用量保留 ID，不能配置。
- 每个数据请求必须携带 Key；缺失、空 Header、未知、禁用、格式错误或两个身份 Header 冲突时均返回 401，且不产生用量记录。
- OpenAI 使用 `Authorization: Bearer <key>`，Anthropic 使用 `X-API-Key: <key>`；两种 Header 可兼容，但同时出现时必须为同一 Key。
- 原始客户端 Key 不写入日志、DuckDB、归档或管理 API，也不会转发给上游。
- Admin 可创建、启停、轮换或删除客户端 Key。创建和轮换仅在成功响应中显示一次明文；托管 Key 的 YAML 使用 `api_key_hash`，不能与 `api_key` 同时配置。
- `inbound_api_key`、`AI_PROXY_INBOUND_API_KEY`、`usage_file` 与 `AI_PROXY_USAGE_FILE` 已删除，配置中出现会启动失败。

客户端 Key 是必需的应用层认证；若监听 `0.0.0.0:8080` 或 `:8080`，仍应在防火墙、反向代理或私有网络层实施额外访问控制。

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
| `admin_auth_enabled` / `AI_PROXY_ADMIN_AUTH_ENABLED` | Admin 登录开关，默认 `false`（保持 loopback-only）。 |
| `admin_base_path` / `AI_PROXY_ADMIN_BASE_PATH` | Admin 页面与 API 前缀，默认 `/admin`；启动期路由，变更需重启。 |
| `admin_username` / `AI_PROXY_ADMIN_USERNAME` | 单管理员账号（开启认证时必填，区分大小写）。 |
| `admin_password_hash` / `AI_PROXY_ADMIN_PASSWORD_HASH` | Argon2id PHC 哈希（开启认证时必填；禁止明文）。 |
| `admin_session_cookie_secure` / `AI_PROXY_ADMIN_SESSION_COOKIE_SECURE` | 会话 Cookie 是否仅随 HTTPS 请求发送，默认 `false`。 |
| `admin_session_ttl_seconds` / `AI_PROXY_ADMIN_SESSION_TTL_SECONDS` | 会话绝对有效期，默认 `28800`（8h），范围 `300~86400`。 |

环境变量与配置键的完整默认值、上限和校验以 [`config.example.yaml`](../config.example.yaml) 与 `internal/pkg/aiproxyconfig` 为准。

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

访问 `http://127.0.0.1:8080/admin/`（或自定义 `admin_base_path`）可管理 Provider、查看 API Key 用量、筛选事件和导出 CSV。

### 默认模式（`admin_auth_enabled: false`）

- `<admin_base_path>` 及 API **永远限制 loopback**，即使代理监听在非 loopback 地址。
- 写接口仍要求浏览器意图头 `X-AI-Proxy-Admin: 1`（不是身份凭据）。
- Provider API Key 只显示“已配置”，从不回显明文；保存时保留未修改的已有值或 `${ENV}` 表达式。
- 使用统计页查询 DuckDB，并支持按时间、API Key、Provider、Model、Outcome 和估算标记筛选。

### 安全登录模式（`admin_auth_enabled: true`）

开启后取消 Admin 的 loopback 限制；任意来源都必须先登录并持有有效会话。详见 [Admin 登录安全设计](admin-login-security-design-2026-07-23.md)。

```bash
# 交互式生成 Argon2id 密码哈希（仅 TTY；密码不进参数/日志）
ai-proxy admin password-hash
export AI_PROXY_ADMIN_PASSWORD_HASH='...'
```

```yaml
server:
  admin_auth_enabled: true
  admin_base_path: /ops/ai-proxy   # 可选；默认 /admin；变更需重启
  admin_username: ops-admin
  admin_password_hash: ${AI_PROXY_ADMIN_PASSWORD_HASH}
  admin_session_cookie_secure: true # 可选；开启后仅 HTTPS 可携带会话 Cookie
  admin_session_ttl_seconds: 28800
```

要点：

- 必须配置合法 Argon2id PHC（固定参数 `m=65536,t=3,p=1`）；缺失或非法哈希会使进程在监听前启动失败。
- 会话为进程内内存 Cookie（`HttpOnly` + `SameSite=Strict`）；`admin_session_cookie_secure=true` 时额外带 `Secure`，浏览器仅会在 HTTPS 请求中携带会话。HTTP 仅适用于受信网络，生产环境推荐 HTTPS。代理部署时应保留外部 `Host`；应用不信任 forwarded header。
- 状态变更请求需要会话 Cookie 与 `X-AI-Proxy-CSRF`；未登录 API 返回 JSON `401`，页面 `303` 到 `<basePath>/login`。
- 认证相关配置热更新成功后清空全部会话；`admin_base_path` 变更必须重启。
- 该模式不替代 TLS、主机账户隔离或配置文件权限保护。

Admin usage API 的筛选参数、导出边界与响应格式见 [API Key 用量与 DuckDB 收口方案](api-key-usage-duckdb-web-closure-plan-2026-07-17.md#17-admin-api)。

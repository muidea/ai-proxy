# 运行、观测与发布

## 启动与客户端接入

```bash
make run
AI_PROXY_CONFIG=/etc/ai-proxy/config.yaml make run
```

默认服务地址为 `127.0.0.1:8080`。客户端使用标准入站地址：

```text
OpenAI API base:    http://127.0.0.1:8080/v1
Anthropic API base: http://127.0.0.1:8080
```

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/metrics
curl http://127.0.0.1:8080/stats
```

`/metrics` 与 `/stats` 默认仅允许 loopback。若启用远程访问，应同时设置 `metrics_allowed_cidrs` 限制采集端来源。

## Admin 登录安全（可选）

默认 Admin 仅 loopback 可访问。需要远程运维时，启用账号密码登录：

```bash
# 1. 交互式生成哈希（密码不进入 argv / 环境变量 / 日志）
ai-proxy admin password-hash
export AI_PROXY_ADMIN_PASSWORD_HASH='...'

# 或直接创建/重置 Admin 登录凭据（自动启用 admin_auth_enabled）
ai-proxy admin set-credentials --username ops-admin --config config.yaml

# 2. 配置 server.admin_auth_enabled=true 与账号，或使用环境变量
#    AI_PROXY_ADMIN_AUTH_ENABLED / AI_PROXY_ADMIN_USERNAME / AI_PROXY_ADMIN_PASSWORD_HASH

# 3. 通过 HTTP 或 HTTPS 对外暴露 <admin_base_path>（默认 /admin）
#    生产环境推荐 HTTPS；若要浏览器仅在 HTTPS 携带会话，设置 admin_session_cookie_secure=true。
#    代理应保留外部 Host；应用不信任 X-Forwarded-*。
```

运维注意：

- 启用后任意来源都必须登录；不再保留 loopback 特权旁路。
- 修改密码哈希、账号或开关并成功热更新后，全部内存会话立即失效。
- `admin_base_path` 是启动期路由；变更后必须重启进程，并同步反向代理路径规则。
- 连续 5 次登录失败会按对端 IP 锁定 15 分钟（不信任 forwarded IP）。
- Provider Key、客户端 Key 哈希、Admin 密码哈希与 DuckDB 文件仍需主机权限保护。

设计细节见 [Admin 登录安全设计](admin-login-security-design-2026-07-23.md)。

## 指标与统计

Prometheus 指标均以 `ai_proxy_` 为前缀：

- `ai_proxy_requests_total{provider,model,route,status,outcome}`：请求完成数。
- `ai_proxy_request_duration_seconds_{sum,count}`：请求耗时。
- `ai_proxy_input_tokens_total`、`ai_proxy_output_tokens_total`、缓存 Token 与命中率：Provider/模型维度 Token 数据。
- `ai_proxy_client_requests_total{api_key_id}` 与 `ai_proxy_client_*_tokens_total{api_key_id}`：客户端 Key 维度累计数据。
- `ai_proxy_usage_store_*`：DuckDB 写入、查询、恢复、checkpoint 与健康状态。

`/stats` 返回进程统计、延迟分位数、缓存、上游错误与 all-time `usage` 视图。DuckDB 是用量最终 authority；Prometheus 与 `/stats` 的 Key 累计镜像在启动时由 DuckDB 初始化，并在成功结算请求后更新。

请求 outcome 用于表示流式首包写出后的真实结束态：

| outcome | 含义 |
| --- | --- |
| `success` | 正常完成。 |
| `client_canceled` | 客户端取消。 |
| `idle_timeout` | SSE 空闲超时。 |
| `limit_exceeded` | 本地体或流限制。 |
| `upstream_truncated`、`upstream_failed` | 上游中断或显式失败。 |
| `capability_drift` | Provider 声明的直连端点或模型能力与上游响应不一致。 |
| `incomplete` | 上游未完成。 |
| `client_write`、`protocol`、`conversion`、`error` | 客户端写入、协议、转换或其它错误。 |

完整统计口径与 Admin API 见 [API Key 用量与 DuckDB 收口方案](api-key-usage-duckdb-web-closure-plan-2026-07-17.md)。

## SLO webhook

配置 `slo_violation_webhook` 后，服务只在 SLO 状态变化时异步 POST `entered` / `resolved` 事件。事件带有 `instance_id`、递增 `seq`、`generation` 与稳定 `event_id`。

- 消费方应按 `event_id` 幂等，且只在同一 `instance_id` 内比较 `seq`。
- 投递为有界队列与单 worker；网络、408、425、429、5xx 最多重试三次，429 优先遵循 `Retry-After`。
- shutdown 会取消在途投递，并将剩余队列计入 `ai_proxy_slo_webhook_dropped_total`。

相关指标：`ai_proxy_slo_webhook_dropped_total`、`ai_proxy_slo_webhook_queue_length`、`ai_proxy_slo_webhook_requests_total{result}`。

## 用量、导出与归档

每个已接受请求会先写入 DuckDB `started` 事件，随后结算为 `completed`。管理页或 `<admin_base_path>/api/usage/export.csv`（默认 `/admin/api/usage/export.csv`）可导出安全元数据；单次导出最大范围为 31 天、最大 100,000 行。

旧 `usage.csv` 只可显式一次性导入：

```bash
go run ./cmd/ai-proxy-usage-import \
  -source usage.csv \
  -database usage.duckdb \
  -api-key-id default
```

交互归档位于 `interactions/{round_id}/`，包含脱敏请求元数据、上游请求/响应摘要、客户端响应与 `metadata.json`。`archive_full_content: false` 可禁止请求与响应正文落盘。归档中的敏感 Header 会脱敏，原始客户端/Provider Key 不会写入。

## 备份与维护

不要直接复制正在写入的 DuckDB 文件。建议流程：停止接收新请求、等待当前写入完成、执行 checkpoint、复制数据库文件、恢复服务。数据库恢复、保留策略与历史导入边界详见 [DuckDB 收口方案](api-key-usage-duckdb-web-closure-plan-2026-07-17.md#21-数据保留备份与维护)。

## Provider live probe

Probe 不会在服务启动时运行，可用于验证某个已配置 Provider 的 direct capability：

```bash
go run ./cmd/ai-proxy-probe -config config.yaml \
  -provider <route-owner> -capability chat_completions -model <exact-model-id>
```

输出会脱敏，结论为 `success`、`credential_issue`、`capability_drift` 或 `environment_undetermined`。现场审计记录放在 `docs/provider-capability-audit-*.md`。

Admin 的 Provider 页面还会显示配置启用状态之外的运行期可用性，并提供“检查”按钮。该按钮只对当前
Provider 执行一次最小非流式探测，记录结果但不会改写配置。状态含义如下：

| 状态 | 含义 |
| --- | --- |
| `disabled` | 配置已禁用。 |
| `unknown` | 尚无请求或手动检查结果。 |
| `healthy` | 最近一次记录为成功。 |
| `degraded` | 存在失败，但连续失败少于三次。 |
| `unavailable` | 连续失败至少三次。 |
| `credential_error` | 最近失败为 401 或 403。 |
| `capability_drift` | 最近探测表明端点或模型能力与上游不一致。 |

## 构建与发布

```bash
make check
make build
make release-package VERSION=v1.2.3
make release VERSION=v1.2.3
```

普通提交 CI 只执行 Linux amd64 的格式、依赖、vet、全量测试与构建。推送 `vX.Y.Z` tag 后，Release workflow 会统一验证源码一次，并在 Linux amd64/arm64、macOS arm64、Windows amd64 原生 runner 上打包 `.tar.gz` 与 SHA-256 文件，然后创建 GitHub Release。

不要从 amd64 强制交叉编译 Linux arm64：DuckDB Go bindings 需要相应的原生目标 runner。手动重跑 Release workflow 时，输入的版本必须是已有 tag。

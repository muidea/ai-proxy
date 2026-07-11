# 轻量级 LLM 本地代理网关

## 1. 项目背景
在本地使用各种 AI 客户端（如 NextChat, LobeChat, ChatBox 等）时，通常需要直接连接多个云端模型 API。本项目旨在构建一个运行在本地的轻量级代理服务（Gateway），统一管理 API 调用，并实时统计不同模型的 Token 消耗情况，以便用户直观掌握使用成本。

---

## 2. 项目目标 (Goals)

### 2.1 产品目标

*   **本地统一入口**：为 NextChat、LobeChat、ChatBox、IDE Agent 等客户端提供一个本地 `API_BASE_URL`，屏蔽不同上游 provider 的 Base URL、API Key 与协议差异。
*   **标准 path + 纯 model 路由**：客户端只访问 OpenAI/Anthropic 标准入站 path；代理仅按请求 `model` 与 provider `models` 规则匹配上游，不再使用 `X-AI-Provider` / `?provider=` / `provider/model`。
*   **双向协议兼容**：OpenAI chat ↔ Anthropic messages 提供基础协议转换；其余 OpenAI 端点对 openai provider 直通。
*   **用量与成本可见**：对每个经代理处理的标准 `/v1/*` 请求记录 provider、model、HTTP 状态、耗时、流式/非流式、输入/输出 token、缓存读写 token、缓存命中率；没有模型或 token 语义的请求保留空模型与 0 token，不能阻塞流水记录。
*   **问题可追踪**：每个经代理处理的请求生成独立交互归档，保留请求、上游请求、上游响应、最终响应、元数据、fallback 尝试、request_id 和请求指纹，便于复盘异常、并发交错和 prompt 漂移。
*   **多 provider 路由与韧性**：支持模型匹配规则、默认 provider 和同协议 fallback；在限流、超时、网络错误或 5xx 时可自动切换备用上游。
*   **轻量本地运维**：采用 Go 单二进制交付，不依赖数据库、中间件或长期驻留外部服务；配置、用量流水和交互归档均使用本地文件。
*   **可观测性内建**：提供 `/healthz`、`/metrics`、`/stats`、`/stats/stream`，让终端、Prometheus、TUI 或轻量 dashboard 能实时查看请求量、延迟分位数、错误率、fallback 和 cache 命中率。

### 2.2 范围边界

*   **Provider 配置**：通过 `config.yaml` 与环境变量配置端口、超时、usage 文件、交互归档目录、保留轮数、debug 日志、metrics 访问策略、SLO 阈值和 provider 列表。
*   **Provider 选择**：仅 `models` 匹配 + `default_provider` 兜底；多 provider 命中同一 model 时返回明确 400。
*   **转发能力**：标准 OpenAI path 与 Anthropic `/v1/messages`；非白名单 path 404；OpenAI chat ↔ Anthropic messages 基础双向转换。
*   **记录能力**：`usage.csv` 作为结构化流水账，`interactions/{round_id}/` 作为完整交互归档；归档需要脱敏敏感请求头。
*   **监控能力**：`/metrics` 输出 Prometheus text exposition format，`/stats` 输出 JSON 聚合快照，`/stats/stream` 输出 SSE 快照流；默认仅允许 loopback 访问，可通过 `metrics_remote_access` 显式开放远程访问。
*   **安全底线**：不在日志、CSV、metadata 或归档 meta 中明文输出 API Key、Authorization、Cookie 等敏感头。

### 2.3 可衡量目标

*   **代理延迟**：在本机 loopback mock 上游、非 TLS、固定小响应的验收环境下，代理层自身引入的首包额外延迟 P95 目标不超过 20ms。
*   **资源占用**：进程空闲 60s 后的 RSS 目标不超过 30MB；持续请求下不因归档、metrics 或 SSE 处理出现无界内存增长。
*   **并发安全**：本地多个客户端并发调用时，CSV 追加、交互目录序号、metrics 聚合和 fallback 记录不能错乱或数据竞争。
*   **用量准确性**：上游提供 usage 时优先使用精确值；缺失 usage 时允许估算，但必须在 CSV、metadata 和控制台摘要中标记。
*   **故障透明**：未触发 fallback 的上游错误透传原始状态码和响应体；触发 fallback 时归档每次失败原因，若备用上游成功则返回备用结果，若全部失败则返回最终一次上游错误或明确的代理错误。
*   **部署简单**：用户能通过 `make build` 得到单个可执行文件，并通过 `config.example.yaml` 或环境变量完成首次运行。

### 2.4 非目标 (Non-Goals)

*   不提供多用户账号、权限体系、计费账单或团队级成本分摊。
*   不长期保存完整历史数据；本地交互归档受 `interaction_retention` 控制。
*   不实现完整 OpenTelemetry 分布式追踪；当前以轻量 Prometheus、JSON stats、CSV 和本地归档为主。
*   不在本阶段验收 metrics CIDR 白名单策略；`metrics_allowed_cidrs` 作为预留配置字段，当前 DoD 只覆盖 loopback 默认保护与 `metrics_remote_access` 开关。
*   不承诺兼容所有上游私有扩展字段；优先保证 OpenAI-compatible 与 Anthropic Messages 的主路径。
*   不修改用户 prompt 或模型输出内容，除必要的协议适配外保持透明转发。

---

## 3. 完成定义 (Definition of Done, DoD)

### 3.1 接口与转发

- [ ] `GET /healthz` 返回 200 和 JSON 健康状态。
- [ ] `POST /v1/chat/completions` 能完成 OpenAI-compatible 非流式转发，客户端收到的状态码、主要响应头和 JSON body 与上游语义一致。
- [ ] `POST /v1/chat/completions` 在 `stream: true` 时以 SSE 方式转发，客户端能边生成边接收；代理不能等待完整流结束后才向客户端返回，归档层允许增量写入原始流并在流结束后生成整理版响应。
- [ ] 白名单 OpenAI 端点（`/v1/responses`、`/v1/completions`、`/v1/embeddings`、`/v1/models`）可直通 openai provider；非白名单 `/v1/*` 返回 404。
- [ ] Anthropic `POST /v1/messages` 可直通 anthropic provider，或在命中 openai provider 时做基础转换；OpenAI chat 命中 anthropic provider 时可做反向基础转换。
- [ ] 未触发 fallback 的上游 400、401、403、404、429、5xx 等错误能保留原始状态码和错误体透传给客户端；代理自身错误返回明确、可读的 4xx/5xx 信息。

### 3.2 Provider 路由与 fallback

- [ ] 仅按 `models` 与 `default_provider` 路由；`X-AI-Provider` / `?provider=` / `provider/model` 被忽略；行为有测试覆盖和文档说明。
- [ ] provider `enabled: false` 后不参与 model 匹配。
- [ ] 当多个 provider 的 models 同时命中同一 model 时返回 400，提示调整 models 规则。
- [ ] fallback 仅在网络错误、408、429 和 5xx 等可重试场景触发；401、403、400 等认证或请求错误不触发 fallback。
- [ ] fallback 只能切换到同协议、已启用、已配置的 provider；每次尝试记录 provider、状态码/错误、耗时和原因。
- [ ] fallback 成功时，客户端响应、`usage.csv` 和 `metadata.json` 记录实际返回的备用 provider；fallback 全部失败时，客户端收到最终一次上游错误响应，网络级失败则收到明确的代理 502。

### 3.3 Token、cache 与用量统计

- [ ] 非流式响应优先解析上游 `usage`，提取输入 token、输出 token、总 token、cache read token、cache creation token。
- [ ] 流式响应优先解析结束前或事件中的 usage；缺少 usage 时按本地估算逻辑补齐 token，并标记为估算。
- [ ] OpenAI-compatible 的 `prompt_tokens_details.cached_tokens` / `input_tokens_details.cached_tokens` 与 Anthropic 的 `cache_read_input_tokens` / `cache_creation_input_tokens` 均能进入统一统计字段。
- [ ] 每个经代理处理的 `/v1/*` 请求结束后追加一行 `usage.csv`，字段至少包括 `time,provider,model,input_tokens,output_tokens,total_tokens,duration_ms,stream,estimated,http_status,cached_input_tokens,cache_creation_input_tokens,cache_hit_rate`；没有模型或 token 语义的端点写入空模型与 0 token。
- [ ] CSV 写入并发安全；并发请求不会交叉写半行、重复表头或破坏 CSV 格式。
- [ ] 控制台摘要包含 round id、provider、model、状态码、耗时、token、cache 字段和错误摘要；敏感信息不出现在摘要中。

### 3.4 交互归档与可追踪性

- [ ] 每个经代理处理的 `/v1/*` 请求创建递增的 `interactions/{round_id}/` 目录，目录名稳定、并发安全。
- [ ] 归档至少包含客户端请求、脱敏请求元信息、最终上游请求、上游响应、最终客户端响应和 `metadata.json`；流式响应应增量保存原始 `response.sse`，并在流结束后保存整理后的完整响应。
- [ ] `metadata.json` 包含 round id、request_id、provider、model、HTTP 状态、耗时、token、cache 字段、stream、estimated、响应文件路径和错误信息。
- [ ] 请求入口生成或透传 `X-Request-ID`，响应头返回同一 request id，并写入归档 metadata。
- [ ] 请求指纹和 stable prefix hash 写入 metadata；连续 2 次出现不同 stable prefix hash 时记录 `stable_prefix_drift` 与 `stable_prefix_drift_count` 字段。本阶段该阈值为全局固定值，不要求运行时配置。
- [ ] `interaction_retention` 能限制本地归档保留轮数，并且不会删除正在写入的活跃轮次。

### 3.5 可观测性与 SLO

- [ ] `/metrics` 返回 Prometheus 兼容文本，至少包含请求数、请求耗时、输入/输出 token、cache token、cache hit rate、上游错误和 fallback 次数。
- [ ] `/stats` 返回 JSON 聚合快照，至少包含 uptime、按 provider/status 的请求量、cache 命中率、延迟 p50/p75/p90/p95/p99、上游错误分布和 fallback 次数。
- [ ] `/stats/stream` 以 SSE 周期推送 `/stats` 快照，客户端断开后资源能释放。
- [ ] `/metrics`、`/stats`、`/stats/stream` 默认仅允许 loopback 访问；开启 `metrics_remote_access` 后才允许远程访问。本阶段不验收 `metrics_allowed_cidrs` 白名单生效。
- [ ] SLO 配置支持 cache 命中率下限、上游错误率上限、p99 延迟上限和巡检周期；命中阈值时输出可定位 provider 与规则的事件。

### 3.6 配置、安全与部署

- [ ] `config.example.yaml` 覆盖 server、provider、fallback、metrics 和 SLO 的常用配置，并且可直接复制为 `config.yaml` 后运行。
- [ ] 支持通过环境变量覆盖监听地址、端口、usage 文件、交互目录、超时、默认 provider；内置 provider 支持 `AI_PROXY_<PROVIDER>_*` / `<PROVIDER>_*` 覆盖 API Key、Base URL、模型规则和 fallback，自定义 provider 通过 `config.yaml` 中的 `${ENV}` 注入。
- [ ] 配置加载会校验至少存在一个启用 provider，`default_provider` 必须指向已启用 provider。
- [ ] 日志、请求元信息、归档和错误输出会脱敏 `Authorization`、`X-API-Key`、`Cookie` 等敏感头。
- [ ] `make build` 能生成单二进制；目标机器无需安装 Go、数据库或中间件即可运行该二进制。
- [ ] 服务收到 SIGINT/SIGTERM 后能在超时时间内优雅关闭，不丢失已完成请求的 CSV 和归档记录。

### 3.7 性能、并发与质量门禁

- [ ] 在本机 loopback mock 上游、非 TLS、固定 1KB 非流式响应和固定首个 SSE chunk 立即返回的环境中，各执行 1000 次请求、并发度 1 与 10 各一轮；代理路径相对直接访问 mock 上游的首包额外延迟 P95 满足 20ms 目标。若环境不满足，需要在验收记录中说明瓶颈和实测值。
- [ ] 进程启动并空闲 60s 后 RSS 满足 30MB 目标；持续 1000 次请求后，metrics 样本、归档清理和流式处理没有无界增长。
- [ ] 并发测试覆盖 CSV 写入、交互归档序号、metrics 聚合、fallback 记录和流式转发。
- [ ] `make check` 通过，即 `go fmt ./...`、`go vet ./...`、`go test ./...` 均无失败。
- [ ] 关键路径测试覆盖配置加载、provider 选择、OpenAI-compatible 转发、Anthropic 适配、流式 usage、usage 估算、cache 字段、归档、metrics、SLO 和错误透传。
- [ ] README、`config.example.yaml` 与本 PRD 的端点、字段、环境变量和运行方式保持一致。

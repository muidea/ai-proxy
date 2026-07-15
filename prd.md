# 轻量级 LLM 本地代理网关

## 1. 项目背景
在本地使用各种 AI 客户端（如 NextChat, LobeChat, ChatBox 等）时，通常需要直接连接多个云端模型 API。本项目旨在构建一个运行在本地的轻量级代理服务（Gateway），统一管理 API 调用，并实时统计不同模型的 Token 消耗情况，以便用户直观掌握使用成本。

---

## 2. 项目目标 (Goals)

### 2.1 产品目标

*   **本地统一入口**：为 NextChat、LobeChat、ChatBox、IDE Agent 等客户端提供一个本地 `API_BASE_URL`，屏蔽不同上游 provider 的 Base URL、API Key 与协议差异。
*   **标准 path + 纯 model 路由**：客户端只访问 OpenAI/Anthropic 标准入站 path；请求期按 exact `model` 查找 `model_catalog` 已解析的唯一 RouteOwner，provider `models` 只在启动期验证归属，不再使用 `X-AI-Provider` / `?provider=` / `provider/model`。
*   **双向协议兼容**：OpenAI chat ↔ Anthropic messages 提供基础协议转换；其余 OpenAI 端点仅在 RouteOwner 显式声明对应 `endpoint_capabilities` 时对 openai provider 直通。
*   **用量与成本可见**：对每个经代理处理的标准 `/v1/*` 请求记录 provider、model、HTTP 状态、耗时、流式/非流式、输入/输出 token、缓存读写 token、缓存命中率；没有模型或 token 语义的请求保留空模型与 0 token，不能阻塞流水记录。
*   **问题可追踪**：每个经代理处理的请求生成独立交互归档，保留请求、上游请求、上游响应、最终响应、元数据、request_id 和请求指纹，便于复盘异常、并发交错和 prompt 漂移。
*   **多 provider 纯 model 路由**：`model_catalog` 唯一 RouteOwner；无 provider fallback；未匹配/operation 不支持直接 typed 4xx。
*   **轻量本地运维**：采用 Go 单二进制交付，不依赖数据库、中间件或长期驻留外部服务；配置、用量流水和交互归档均使用本地文件。
*   **可观测性内建**：提供 `/healthz`、`/metrics`、`/stats`、`/stats/stream`，实时查看请求量、延迟分位数、错误率和 cache 命中率。

### 2.2 范围边界

*   **Provider 配置**：通过 `config.yaml` 与环境变量配置端口、超时、usage 文件、交互归档目录、保留轮数、debug 日志、metrics 访问策略、SLO 阈值和 provider 列表。
*   **Provider 选择**：仅 `model_catalog` exact model → 唯一 RouteOwner；无 default_provider / fallback。
*   **Provider 模型范围**：每个 enabled provider 必须显式声明 `models`；不按 provider 名、protocol 或常见模型家族推导默认 pattern，pattern 也不自动生成 catalog。
*   **转发能力**：标准 OpenAI path 与 Anthropic `/v1/messages`；非白名单 path 404；OpenAI chat ↔ Anthropic messages 基础双向转换。
*   **记录能力**：`usage.csv` 作为结构化流水账，`interactions/{round_id}/` 作为完整交互归档；归档需要脱敏敏感请求头。
*   **监控能力**：`/metrics` 输出 Prometheus text exposition format，`/stats` 输出 JSON 聚合快照，`/stats/stream` 输出 SSE 快照流；默认仅允许 loopback 访问，可通过 `metrics_remote_access` 显式开放远程访问。
*   **安全底线**：不在日志、CSV、metadata 或归档 meta 中明文输出 API Key、Authorization、Cookie 等敏感头。

### 2.3 可衡量目标

*   **代理延迟**：在本机 loopback mock 上游、非 TLS、固定小响应的验收环境下，代理层自身引入的首包额外延迟 P95 目标不超过 20ms。
*   **资源占用**：进程空闲 60s 后的 RSS 目标不超过 30MB；持续请求下不因归档、metrics 或 SSE 处理出现无界内存增长。
*   **并发安全**：本地多个客户端并发调用时，CSV 追加、交互目录序号、metrics 聚合不能错乱或数据竞争。
*   **用量准确性**：上游提供 usage 时优先使用精确值；缺失 usage 时允许估算，但必须在 CSV、metadata 和控制台摘要中标记。
*   **故障透明**：上游错误透传原始状态码和响应体；代理自身错误返回明确 4xx/5xx typed 合同。
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
- [ ] 白名单 OpenAI 端点（`/v1/responses`、`/v1/completions`、`/v1/embeddings`、`/v1/models`）在 RouteOwner 显式声明对应 `endpoint_capabilities` 时可直通 openai provider；非白名单 `/v1/*` 返回 404。
- [ ] Anthropic `POST /v1/messages` 可直通 anthropic provider，或在命中 openai provider 时做基础转换；OpenAI chat 命中 anthropic provider 时可做反向基础转换。
- [x] 上游 400、401、403、404、429、5xx 等错误保留原始状态码和错误体透传；代理自身错误返回明确 typed 4xx/5xx。

### 3.2 Provider 路由（catalog authority）

- [x] 仅按 `model_catalog` exact model + RouteOwner 路由；`X-AI-Provider` / `?provider=` / `provider/model` 被忽略。
- [x] 无 provider fallback；无 model 未命中时的 default_provider 兜底。
- [x] provider `enabled: false` 后不参与 model 匹配。
- [x] 当多个 provider 的 models 同时命中同一 model 时配置加载启动失败，提示调整 `models` 规则。
- [x] enabled provider 的 `models` 必须显式配置；空 `models` 不按 provider 名或 protocol 自动补默认值。

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

- [x] `/metrics` 返回 Prometheus 兼容文本，至少包含请求数、请求耗时、输入/输出 token、cache token、cache hit rate、上游错误。
- [x] `/stats` 返回 JSON 聚合快照，至少包含 uptime、按 provider/status 的请求量、cache 命中率、延迟分位数、上游错误分布。
- [ ] `/stats/stream` 以 SSE 周期推送 `/stats` 快照，客户端断开后资源能释放。
- [ ] `/metrics`、`/stats`、`/stats/stream` 默认仅允许 loopback 访问；开启 `metrics_remote_access` 后才允许远程访问。本阶段不验收 `metrics_allowed_cidrs` 白名单生效。
- [ ] SLO 配置支持 cache 命中率下限、上游错误率上限、p99 延迟上限和巡检周期；命中阈值时输出可定位 provider 与规则的事件。

### 3.6 配置、安全与部署

- [x] `config.example.yaml` 覆盖 server、provider（含 endpoint_capabilities）、model_catalog、metrics 和 SLO，可复制为 `config.yaml` 后运行。
- [x] 支持通过环境变量覆盖监听地址、端口、usage 文件、交互目录、超时等 server 字段；**不支持** env 注入 provider；provider 与 api_key 在 config 中声明，可用 `${ENV}` 展开。
- [x] 配置加载校验至少存在一个启用 provider；不支持 `default_provider`。
- [ ] 日志、请求元信息、归档和错误输出会脱敏 `Authorization`、`X-API-Key`、`Cookie` 等敏感头。
- [ ] `make build` 能生成单二进制；目标机器无需安装 Go、数据库或中间件即可运行该二进制。
- [ ] 服务收到 SIGINT/SIGTERM 后能在超时时间内优雅关闭，不丢失已完成请求的 CSV 和归档记录。

### 3.7 性能、并发与质量门禁

- [ ] 在本机 loopback mock 上游、非 TLS、固定 1KB 非流式响应和固定首个 SSE chunk 立即返回的环境中，各执行 1000 次请求、并发度 1 与 10 各一轮；代理路径相对直接访问 mock 上游的首包额外延迟 P95 满足 20ms 目标。若环境不满足，需要在验收记录中说明瓶颈和实测值。
- [ ] 进程启动并空闲 60s 后 RSS 满足 30MB 目标；持续 1000 次请求后，metrics 样本、归档清理和流式处理没有无界增长。
- [x] 并发测试覆盖 CSV 写入、交互归档序号、metrics 聚合和流式转发。
- [ ] `make check` 通过，即 `go fmt ./...`、`go vet ./...`、`go test ./...` 均无失败。
- [ ] 关键路径测试覆盖配置加载、provider 选择、OpenAI-compatible 转发、Anthropic 适配、流式 usage、usage 估算、cache 字段、归档、metrics、SLO 和错误透传。
- [ ] README、`config.example.yaml` 与本 PRD 的端点、字段、环境变量和运行方式保持一致。

## model_catalog operation 合同（WorkOrch 对齐）

- [x] `model_catalog` 每项必填 `operations`（`chat_completions` / `embeddings`）；model id **严格区分大小写**且 exact 唯一。
- [x] 每个 catalog model 唯一匹配 enabled provider；`GET /v1/models` 使用具体 DTO，只发布模型业务能力，
  不暴露 RouteOwner。
- [x] 请求前按 exact model + path operation 校验；`operation_unsupported` / `model_not_found` 不访问上游。

### Provider endpoint_capabilities

- [x] enabled provider 必须显式配置 `endpoint_capabilities`（仅表示上游直连能力）。
- [x] 不支持 env 注入 provider；不支持 `fallbacks`。
- [x] 请求前校验 endpoint capability，失败返回 `endpoint_unsupported`。

### Provider Capability Contract（TransportPlan）

- [x] 启动期 `ResolvedModelRoute` 与请求期 `TransportPlan` 两阶段权威。
- [x] 单一入口 `ResolveTransportPlan` 应用固定转发矩阵（native / openai_to_anthropic / anthropic_to_openai）。
- [x] 转换前 feature preflight 返回 `conversion_unsupported`，不访问上游。
- [x] 转换模式下游错误保留 status，输出客户端协议兼容安全 envelope。
- [x] metadata 记录 operation、client endpoint/protocol、upstream protocol/path、conversion mode。
- [x] `go test ./...` 覆盖矩阵、typed error、conversion preflight、SDK 验收与错误 envelope。
- [x] 独立 `cmd/ai-proxy-probe` 运维入口与脱敏审计记录。

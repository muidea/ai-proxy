# ai-proxy PRD

> 以当前代码实现为准。更新时间：2026-07-16。

## 1. 产品目标

ai-proxy 是一个本地单二进制 LLM API 网关，为 OpenAI-compatible 与 Anthropic Messages 客户端提供统一入口。

产品应达成：

- 客户端只需配置一个本地 API Base URL，并使用裸模型名调用标准接口。
- 请求仅按 `model_catalog` 中严格匹配的模型 ID 路由到唯一 provider；不支持 fallback、默认 provider 或显式指定 provider。
- 支持 OpenAI chat 与 Anthropic Messages 的原生转发及基础双向转换；其他端点只允许原生转发。
- 每个处理过的模型请求可追溯、可统计、可观测，并且不泄露认证信息。
- 以本地文件和内存指标完成部署与运维，不依赖数据库或中间件。

## 2. 范围与边界

### 2.1 支持范围

- OpenAI：`/v1/chat/completions`、`/v1/responses`、`/v1/completions`、`/v1/embeddings`、`/v1/models`。
- Anthropic：`/v1/messages`。
- OpenAI chat 与 Anthropic Messages 的基础文本请求、非流式响应与 SSE 流式响应转换。
- 本地用量 CSV、交互归档、Prometheus metrics、JSON/SSE stats、SLO 巡检与可选 webhook。
- 入站 API Key、请求/响应/流大小限制、超时、默认 loopback 观测访问保护。

### 2.2 非目标

- 多用户、权限、计费、团队成本分摊或长期指标存储。
- provider fallback、负载均衡、重试后切换 provider、通过 header/query/provider 前缀选路。
- 所有上游私有扩展的完全兼容；tools、function calling、多模态、`response_format` 等不属于协议转换保证范围。
- 跨实例共享 `usage.csv`。

## 3. 核心产品合同

### 3.1 路由与能力

- `model_catalog` 是模型能力与路由的唯一权威；模型 ID exact match，严格区分大小写。
- 每个 catalog 模型在启动期必须唯一匹配一个 enabled provider 的 `models` 规则，否则服务拒绝启动。
- 每个模型必须声明 `chat_completions` 或 `embeddings` operation；请求在访问上游前校验模型和 operation。
- provider 必须显式配置 protocol、base URL、models、endpoint capabilities；远程上游必须配置 API Key。
- 不支持 `default_provider`、`fallbacks`、`X-AI-Provider`、`?provider=` 或 `provider/model` 选择路由。

### 3.2 转发矩阵

| 客户端路径 | 可用上游 | 行为 |
| --- | --- | --- |
| `/v1/chat/completions` | OpenAI chat | 原生转发 |
| `/v1/chat/completions` | Anthropic messages | OpenAI → Anthropic 基础转换 |
| `/v1/messages` | Anthropic messages | 原生转发 |
| `/v1/messages` | OpenAI chat | Anthropic → OpenAI 基础转换 |
| `/v1/responses`、`/v1/completions`、`/v1/embeddings` | 对应 OpenAI 直连能力 | 原生转发 |
| `/v1/models` | 本地 catalog | 本地合成，不访问上游 |

转换范围限于基础文本消息与常用生成参数；不支持的转换特性必须在访问上游前返回明确错误。

### 3.3 安全与可靠性

- 非 loopback 监听必须配置入站 API Key；支持 `Authorization: Bearer` 与 `X-API-Key`。
- 每个请求透传或生成 `X-Request-ID`，响应返回相同 ID；`/healthz` 不要求认证。
- 上游认证由代理使用 provider 配置重建；入站认证信息不会转发或写入可见审计信息。
- 所有请求、上游非流式响应与 SSE 流均受大小限制；流式请求同时受空闲超时和协议终止校验保护。
- 上游 4xx/5xx 保留状态与协议语义；代理自身错误使用 OpenAI 或 Anthropic 兼容的 typed error envelope。

## 4. Definition of Done

### 4.1 接入与转发

- [x] `GET /healthz` 返回 `200` 和健康 JSON。
- [x] OpenAI chat、responses、completions、embeddings 与 Anthropic Messages 路径受白名单保护；未知路径返回 `404`。
- [x] OpenAI chat ↔ Anthropic Messages 支持基础的双向非流式和 SSE 转换。
- [x] OpenAI 非 chat 端点只在 RouteOwner 声明对应直连 capability 时原生转发。
- [x] `/v1/models` 由本地 `model_catalog` 合成，且不暴露 provider、URL 或认证信息。
- [x] 上游路径正确处理带 `/v1` 的 base URL，避免重复版本路径。

### 4.2 路由合同

- [x] 请求仅按 body 中的 exact `model` 和 catalog 的唯一 RouteOwner 路由。
- [x] 配置加载拒绝未匹配、重复匹配、缺 operation、缺能力或缺 provider 必填项的模型配置。
- [x] `model_required`、`model_not_found`、`operation_unsupported`、`endpoint_unsupported`、`conversion_unsupported` 均在访问上游前返回。
- [x] 无 default provider、fallback 或失败切换；显式 provider 头、query 和模型前缀不影响路由。

### 4.3 认证、错误与资源保护

- [x] 非 loopback 监听强制入站 API Key；错误认证返回 `401 authentication_failed`。
- [x] 请求体、上游非流式响应、SSE 累计字节和单行长度均有可配置上限；请求体超限返回 `413 request_too_large`。
- [x] 流式响应即时转发并 flush；客户端取消、空闲超时、上游截断、协议损坏等会记录真实 outcome。
- [x] 请求与响应头处理不会泄露入站 API Key、Authorization、Cookie 等敏感信息。
- [x] 服务支持 SIGINT/SIGTERM 下的优雅关闭。

### 4.4 用量与审计

- [x] 每个处理过的 `/v1/*` 请求写入单进程并发安全的 `usage.csv`。
- [x] 用量记录包含路由、operation、token、cache、时延、流式、HTTP 状态、转换方式与 outcome。
- [x] 优先解析上游 usage；缺失时对适用响应估算 token 并显式标记。
- [x] 每个请求创建递增交互归档，保留请求/上游/响应的脱敏元信息、最终 metadata 与 request ID。
- [x] 可关闭完整正文归档；归档保留策略不会删除正在写入的轮次。
- [x] metadata 记录模型、provider、TransportPlan、状态、token、outcome、指纹和 stable-prefix drift 信息。

### 4.5 可观测性与 SLO

- [x] `/metrics` 提供请求、时延、token、cache、上游错误及 SLO webhook 指标。
- [x] `/stats` 提供按 provider/status/outcome 聚合的请求、cache、延迟分位数与上游错误快照。
- [x] `/stats/stream` 以 SSE 立即并按秒推送 stats 快照，客户端断开后释放资源。
- [x] metrics、stats 和 stats stream 默认仅 loopback 可访问；可按远程访问开关及 IP/CIDR 白名单开放。
- [x] SLO 支持 cache 命中率、上游错误率、上游 attempt p99 延迟阈值及状态变化 webhook。

### 4.6 交付与质量

- [x] `config.example.yaml` 覆盖 server、provider、model catalog、metrics 和 SLO 配置。
- [x] 支持环境变量覆盖 server 层配置及 `${ENV}` 展开敏感配置值；provider 本身只能在配置文件中声明。
- [x] 提供独立 `ai-proxy-probe`，可脱敏探测已声明 provider 的直连能力。
- [x] 自动化测试覆盖配置校验、路由矩阵、协议转换、流式处理、用量、归档、metrics、SLO webhook 与 probe。

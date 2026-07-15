# ai-proxy Provider Capability Contract 功能设计

Status: active

Type: feature-design

Last Updated: 2026-07-15

Review State: ai-proxy-code-closure-verification

## 0. 2026-07-15 代码评审与收口结论

本文是 ai-proxy 独立拥有的完整功能设计，不由 WorkOrch 或具体 SDK 反向推导。WorkOrch 后续按本合同单独
同步，本轮不把跨仓联调计入 ai-proxy 代码修改范围。

C01–C05 的实现收口与 C06 的 SDK/mock 验收已经落入代码；全量门禁和 provider direct capability live matrix
仍需按本文与 `docs/provider-capability-audit-2026-07-15.md` 复核。因此本文保持 `active`，不得把部分 chat live
结果表述为 C01–C08 全部完成。

用户补充合同：model ID **严格区分大小写**，`DeepSeek-V4-Flash` 与 `deepseek-v4-flash` 视为两个不同 model。

已确认完成：

- 已实现 `ResolvedModelRoute`、`TransportPlan`、`ResolveTransportPlan`、`ProviderHasDirectEndpoint`。
- 已实现 `conversion_unsupported` 与 APIError 上下文字段；转换 preflight 在访问上游前拒绝。
- chat / messages / raw OpenAI handler 统一消费 TransportPlan；转换错误输出客户端协议 envelope。
- metadata 记录 operation、client endpoint/protocol、upstream protocol/path、conversion mode。
- 响应转换对上游空 `tool_calls: []` / null `function_call` 视为无 feature，仅非空拒绝。
- 单元测试矩阵覆盖路由矩阵、typed error、conversion preflight 与错误 envelope。

本轮已处理的历史代码阻断：

1. model ID 已改为 exact 唯一且严格区分大小写；正式配置中的大小写不同 ID 可以共存。
2. 上游请求头仍为“全量复制后删除少数字段”，尚未按 upstream protocol 实现 allowlist。
3. enabled 远程 provider 的空 API Key 未 fail-fast；已知 provider 名仍会隐式补 protocol/base URL。
4. OpenAI→Anthropic 流式响应仍可能通过通用 flatten 将非文本 content 静默转成文本。
5. buffered 转换失败在 usage/metrics 中仍记录为 `error`，仅 metadata 会二次改为 `conversion`。
6. usage/metrics/SLO 尚未按本文定义消费同一 TransportPlan 观测上下文；README 对
   `usage.csv` 字段的描述与代码不一致。
7. invalid JSON、request too large、代理内部失败和上游网络失败仍有自由文本错误。
8. 已增加 OpenAI SDK / Anthropic SDK 验收测试和独立 live probe；probe 输出包含 provider、protocol、
   capability、exact model、path、stream、status、duration 与 conclusion，并做摘要脱敏。
9. WorkOrch 的合同同步留待其后续独立调整；ai-proxy 本身不实现 fallback。

最终收口必须重新通过以下代码门禁：

- `go test ./... -count=1`
- `go vet ./...`
- `gofmt -l .`（无输出）
- `git diff --check`

上述门禁通过只证明当前测试集没有回归；只有 direct capability live matrix 的已验证项也被准确归档后，
才能更新相应 live 状态。WorkOrch 联调状态必须单独记录，不得由 ai-proxy 单仓测试代替。

Related:

- [WorkOrch 模型目录与 Operation 合同收口计划](workorch-model-catalog-operation-closure-plan-2026-07-15.md)
- `/home/rangh/aispace/workorch/docs/70-roadmap/active/ai-proxy-primary-llm-provider-cutover-design-2026-07-15.md`
- `internal/config/config.go`
- `internal/proxy/route.go`
- `internal/proxy/handler.go`
- `internal/proxy/anthropic.go`

## 1. 功能定位

Provider Capability Contract 是 ai-proxy 自己拥有的路由与执行能力。它负责：

1. 对外发布 ai-proxy 支持的标准入站端点和模型业务能力。
2. 根据请求中的 exact model 解析唯一上游 provider。
3. 根据入站端点、模型 operation、上游协议和 provider 直连端点生成唯一转发计划。
4. 在客户端协议与上游协议不同时执行 ai-proxy 明确实现的协议转换。
5. 在访问上游前拒绝不支持的模型、operation、endpoint 或转换特性。
6. 为配置校验、模型目录、请求执行、归档、metrics 和 WorkOrch catalog refresh 提供同一份 authority。

OpenAI SDK、Anthropic SDK 和 WorkOrch 都是该功能合同的消费者。客户端请求形态不会反向定义 provider
能力，也不能通过 SDK 类型、header、query 或模型名称猜测 ai-proxy 的路由与转换能力。

## 2. 目标与非目标

### 2.1 目标

- 客户端可使用标准 OpenAI 或 Anthropic SDK 接入 ai-proxy。
- provider 及其上游协议、模型范围、直连 endpoint 和认证只由配置文件声明。
- 请求只按 exact model 路由到唯一 RouteOwner。
- OpenAI Chat Completions 与 Anthropic Messages 支持基础文本协议双向转换。
- `/v1/models` 发布 ai-proxy 已完成启动校验的模型 operation 和容量合同。
- 所有路由与转换拒绝在创建上游请求前完成，并返回稳定 typed error。
- 不存在 default provider、provider fallback 或失败后重新选择 provider。

### 2.2 非目标

- 不根据上游 `/v1/models` 自动创建本地 catalog 或 provider。
- 不根据 `protocol: openai` 自动假定 responses、completions 或 embeddings 可用。
- 不承诺跨协议转换 tools、function calling、图片、音频、文件、文档或 JSON schema。
- 不把 provider 私有扩展、模型别名或计费能力自动暴露为通用合同。
- 不在服务启动过程中执行会产生费用或受外部网络波动影响的 live probe。

## 3. 业务术语与权威边界

| 概念 | 权威数据 | 职责 | 不负责 |
| --- | --- | --- | --- |
| Client Protocol | 入站 endpoint 合同 | 标识客户端使用 OpenAI 或 Anthropic 请求格式 | provider 选择和上游协议 |
| Client Endpoint | HTTP method + path | 标识本次具体 API 操作 | 模型是否支持该 operation |
| Model Operation | `model_catalog.<model>.operations` | 标识模型的稳定业务能力 | 具体上游 endpoint 和协议 |
| Provider Route Profile | `providers.<name>` | 定义一个独立路由单元的地址、认证、模型范围、上游协议和直连端点 | 自动发现 catalog 模型 |
| Upstream Protocol | `providers.<name>.protocol` | 选择上游认证、请求/响应编码和转换器 | 客户端 SDK 类型和 endpoint 能力 |
| Direct Endpoint Capability | `providers.<name>.endpoint_capabilities` | 声明 RouteOwner 上游可直接接收的 endpoint | 协议转换派生的客户端能力 |
| Resolved Model Route | `config.Load` 解析结果 | 固定 exact model → RouteOwner 与 operation readiness | 本次请求的客户端 endpoint |
| Transport Plan | 请求期解析结果 | 固定本次请求的入站协议/path、上游协议/path和转换方式 | 第二次 provider 选择 |

字段语义固定为：

- `operations` 是业务合同。
- `protocol` 始终表示 upstream protocol。
- `endpoint_capabilities` 始终表示 upstream direct endpoint capabilities。
- provider route profile 名称是稳定 RouteOwner ID，不等同于必须唯一的厂商品牌名。

## 4. 对外客户端合同

### 4.1 入站端点白名单

| Method | Path | Client Protocol | Operation |
| --- | --- | --- | --- |
| POST | `/v1/chat/completions` | OpenAI | `chat_completions` |
| POST | `/v1/messages` | Anthropic | `chat_completions` |
| POST | `/v1/responses` | OpenAI | `chat_completions` |
| POST | `/v1/completions` | OpenAI | `chat_completions` |
| POST | `/v1/embeddings` | OpenAI | `embeddings` |
| GET/POST | `/v1/models` | OpenAI-compatible catalog | 不适用 |

其它 `/v1/*` 返回 404。Client Protocol 由 method+path 决定，不从 User-Agent、SDK header 或 body 推断。

### 4.2 模型字段

- 所有执行请求必须在 body 中提供非空 `model`。
- 客户端只发送 catalog 中的裸模型 ID，不使用 `provider/model` 前缀。
- `X-AI-Provider`、`?provider=` 和类似 provider override 不参与路由。
- model ID 按配置原文 exact 匹配并 exact 唯一，严格区分大小写；`DeepSeek-V4-Flash` 与
  `deepseek-v4-flash` 可以同时存在并路由到不同 RouteOwner。

### 4.3 入站认证

- 客户端使用 ai-proxy 的 `inbound_api_key`，而不是上游 provider API Key。
- ai-proxy 接受 `Authorization: Bearer <key>` 或 `X-API-Key: <key>`。
- 客户端认证头不得原样作为上游认证转发；上游认证由 RouteOwner 配置重新生成。

## 5. Model Operation 合同

### 5.1 `chat_completions`

`chat_completions` 表示模型可执行基础对话文本生成。canonical path 固定为
`POST /v1/chat/completions`。

该 operation 的跨协议最低保证范围为：

- `system`、`user`、`assistant` 基础文本角色。
- 字符串内容或仅包含 text block 的内容。
- 非流式和基础文本流式响应。
- `max_tokens`、`temperature`、`top_p` 和 stop/stop_sequences 的基础映射。
- 基础 input/output token usage 映射；上游未提供时允许本地估算并明确标记。

以下能力不属于 `chat_completions` 的跨协议保证：

- tools、tool calls、tool result、function calling。
- 图片、音频、文件、文档和其它多模态 content block。
- `response_format`、JSON schema 和 provider 私有结构化输出。
- provider 私有 reasoning、thinking、cache-control 或 prompt 扩展。

native 直通路径可以透明转发上游额外能力，但这些能力不是 `/v1/models.operations` 的稳定保证，客户端不得
仅因为 model 声明 `chat_completions` 就假定高级特性可用。未来若需要稳定发现高级能力，必须新增独立、
版本化的 feature 枚举，不能扩展现有 operation 的隐含语义。

### 5.2 `embeddings`

`embeddings` 表示模型可执行文本向量化。canonical path 固定为 `POST /v1/embeddings`。

- 只允许 RouteOwner 使用 OpenAI-compatible upstream protocol 且显式声明 `embeddings`。
- 不通过 Anthropic Messages 或 Chat Completions 做协议转换。
- 输入格式、向量维度和 provider 私有参数按 native upstream 合同透传。

### 5.3 Operation readiness

每个 catalog model 的 operation 必须能由唯一 RouteOwner 执行其 canonical path。启动校验失败的模型不得
出现在 `/v1/models`，也不得等到请求期才发现 canonical endpoint 不可用。

非 canonical endpoint，例如 `/v1/responses` 和 `/v1/completions`，不改变 operation readiness；它们还必须
通过本次请求的 endpoint 与 Transport Plan 校验。

## 6. Provider Route Profile 配置模型

### 6.1 Provider 配置

```yaml
providers:
  vendor-openai-chat:
    enabled: true
    protocol: openai
    base_url: https://example.invalid/v1
    api_key: ${VENDOR_API_KEY}
    allow_unauthenticated: false
    endpoint_capabilities: chat_completions, responses
    models: model-a-*, model-b

  vendor-anthropic:
    enabled: true
    protocol: anthropic
    base_url: https://example.invalid
    api_key: ${ANTHROPIC_API_KEY}
    allow_unauthenticated: false
    endpoint_capabilities: messages
    models: claude-*
```

字段规则：

- provider map key 是稳定 RouteOwner ID，用于内部 usage、metadata、metrics 和 SLO；不得通过 `/v1/models`
  暴露给客户端。
- `protocol` 必填，只允许 `openai` 或 `anthropic`；不得根据 provider 名补默认值。
- `endpoint_capabilities` 必填、去重、稳定排序，不允许未知枚举。
- `models` 是该 route profile 可拥有的模型 pattern；它不自动生成 catalog 条目。
- enabled provider 必须显式配置合法 HTTP(S) `base_url`；不得根据 provider 名补默认 URL。
- `allow_unauthenticated` 缺省为 `false`。值为 `false` 时 `api_key` 必须非空；值为 `true`
  时 `base_url` 必须为 loopback 主机，且 `api_key` 必须为空。不允许对远程上游隐式发送无认证请求。
- `protocol=openai` 的默认认证为 `Authorization: Bearer <api_key>`；`protocol=anthropic`
  的默认认证为 `X-API-Key: <api_key>`。
- provider 不能由环境变量动态创建；配置值可以使用 `${ENV}` 展开。

### 6.2 同一厂商的多 route profile

若同一厂商的不同模型支持不同 endpoint，必须拆成多个 models 不重叠的 provider route profile：

```yaml
providers:
  vendor-chat-only:
    protocol: openai
    base_url: https://example.invalid/v1
    api_key: ${VENDOR_API_KEY}
    endpoint_capabilities: chat_completions
    models: model-a-*

  vendor-responses:
    protocol: openai
    base_url: https://example.invalid/v1
    api_key: ${VENDOR_API_KEY}
    endpoint_capabilities: chat_completions, responses
    models: model-b-*
```

拆分后的 profile 被视为不同 RouteOwner，并在 catalog、metrics 和 SLO 中分别展示和聚合。这是明确合同，
不是实现副作用。models pattern 应按配置治理保持不重叠；加载器必须对每个具体 catalog model 做确定检查，
确保它只匹配一个 enabled profile。没有对应 catalog model 的抽象 pattern 交集不作为启动期可判定事实。

## 7. Model Catalog 配置模型

```yaml
model_catalog:
  model-a-fast:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions

  text-embedding-model:
    context_window_tokens: 8192
    max_output_tokens: 8191
    operations: embeddings
```

catalog 规则：

- catalog 只登记具体、可枚举、容量完整的 model ID。
- `operations` 必填，只允许已发布的业务枚举。
- model 必须唯一匹配一个 enabled provider profile，由此生成 RouteOwner。
- provider 的通配 pattern 不会自动展开为 catalog 模型。
- 上游 `/v1/models` 不自动合并进本地 catalog。
- catalog、请求路由和 `/v1/models` 输出共同使用启动期 resolved authority。

## 8. 上游协议与 Direct Endpoint Capability

### 8.1 Direct endpoint 枚举

| Capability | 上游 path | 允许的 upstream protocol |
| --- | --- | --- |
| `chat_completions` | `/v1/chat/completions` | `openai` |
| `messages` | `/v1/messages` | `anthropic` |
| `responses` | `/v1/responses` | `openai` |
| `completions` | `/v1/completions` | `openai` |
| `embeddings` | `/v1/embeddings` | `openai` |

`endpoint_capabilities` 只能描述上表中的直接上游能力。转换得到的客户端可服务 path 不写回
`endpoint_capabilities`，也不改变 provider 配置。

### 8.2 静态兼容规则

- OpenAI protocol 不允许声明 `messages`。
- Anthropic protocol 只允许声明 `messages`。
- protocol 不会自动补齐 endpoint capability。
- endpoint capability 必须适用于该 route profile 匹配的全部 catalog 模型；不满足时拆 profile。

## 9. Transport Plan 转发矩阵

| Client Endpoint | Client Protocol | Operation | Upstream Protocol | 要求的 direct capability | Upstream Endpoint | Mode |
| --- | --- | --- | --- | --- | --- | --- |
| `/v1/chat/completions` | OpenAI | `chat_completions` | OpenAI | `chat_completions` | `/v1/chat/completions` | native |
| `/v1/chat/completions` | OpenAI | `chat_completions` | Anthropic | `messages` | `/v1/messages` | `openai_to_anthropic` |
| `/v1/messages` | Anthropic | `chat_completions` | Anthropic | `messages` | `/v1/messages` | native |
| `/v1/messages` | Anthropic | `chat_completions` | OpenAI | `chat_completions` | `/v1/chat/completions` | `anthropic_to_openai` |
| `/v1/responses` | OpenAI | `chat_completions` | OpenAI | `responses` | `/v1/responses` | native |
| `/v1/completions` | OpenAI | `chat_completions` | OpenAI | `completions` | `/v1/completions` | native |
| `/v1/embeddings` | OpenAI | `embeddings` | OpenAI | `embeddings` | `/v1/embeddings` | native |

矩阵以外的组合统一为 `endpoint_unsupported`。responses、completions 和 embeddings 不允许通过
chat/messages 转换隐式获得。

## 10. 两阶段解析模型

### 10.1 启动期 `ResolvedModelRoute`

`config.Load` 在完成 normalize/validate 后生成等价的只读模型路由：

```go
type ResolvedModelRoute struct {
    ModelID     string
    Operations  []string
    RouteOwner  string
}
```

该结构回答“这个具体模型属于谁、具备哪些业务 operation”，供 `/v1/models` 和请求路由共同消费。

### 10.2 请求期 `TransportPlan`

```go
type TransportPlan struct {
    ModelID          string
    Operation        string
    ClientProtocol   string
    ClientEndpoint   string
    RouteOwner       string
    UpstreamProtocol string
    UpstreamEndpoint string
    Mode             string // native, openai_to_anthropic, anthropic_to_openai
}
```

TransportPlan 只在 ResolvedModelRoute 之上解析，不允许修改 RouteOwner。建议提供单一入口：

```go
ResolveTransportPlan(cfg, method, path, modelID) (TransportPlan, *APIError)
```

内部职责必须分离：

- `ProviderHasDirectEndpoint(provider, capability)` 只检查配置中的直连 endpoint。
- `ResolveTransportPlan(...)` 只应用固定转发矩阵。
- `ValidateConversionRequest(plan, body)` 只检查本次 payload 是否处于转换最低保证范围。

不得继续使用一个名称同时表示 direct endpoint 和转换后可服务 path。

## 11. 标准请求处理链

所有执行端点统一执行以下流程：

1. 校验 method+path 属于入站白名单，并解析 Client Protocol、Client Endpoint 和 Model Operation。
2. 限制并读取请求体，解析 JSON 和 exact model。
3. 从 ResolvedModelRoute authority 查找模型和唯一 RouteOwner。
4. 校验 model operation；失败返回 `operation_unsupported`。
5. 根据 RouteOwner protocol 和 direct endpoint capabilities 解析 TransportPlan。
6. 若 plan 为转换模式，执行转换 feature preflight；失败返回 `conversion_unsupported`。
7. 生成上游 URL、认证和请求体，只调用 TransportPlan 中的唯一 RouteOwner。
8. native 模式透明转发；转换模式按同一 plan 转换响应和 SSE 事件。
9. usage、metadata 和 request metrics 从同一 TransportPlan 记录 RouteOwner、model、client endpoint、
   upstream protocol、upstream endpoint 和 conversion mode；SLO 以同一 RouteOwner/attempt/outcome
   记录聚合，归档保留完整 TransportPlan 上下文。

步骤 1—6 任一失败均不得创建或发送上游请求。

## 12. 协议转换功能合同

### 12.1 OpenAI Chat → Anthropic Messages

请求映射：

- `model` 原样保留。
- system messages 合并为 Anthropic system 文本。
- user/assistant text messages 映射为 Anthropic messages。
- `max_tokens`、`temperature`、`top_p`、`stop` 映射到对应字段。
- OpenAI `stop` 为单个字符串时必须规范为 Anthropic `stop_sequences: [value]`；
  字符串数组保持数组语义，其它类型在访问上游前拒绝。
- `stream` 保持原语义。

响应映射：

- Anthropic text content 合并为 OpenAI assistant content。
- stop_reason 映射为 OpenAI finish_reason。
- input/output/cache usage 映射为 OpenAI usage 扩展。
- 流式事件映射为 OpenAI Chat Completions SSE，并正常输出终止事件。

### 12.2 Anthropic Messages → OpenAI Chat

请求映射：

- `model` 原样保留。
- Anthropic system 文本映射为 OpenAI system message。
- user/assistant text messages 映射为 OpenAI messages。
- `max_tokens`、`temperature`、`top_p`、`stop_sequences` 映射到对应字段。
- `stream` 保持原语义。

响应映射：

- OpenAI assistant text 映射为 Anthropic text content block。
- finish_reason 映射为 Anthropic stop_reason。
- usage 映射为 Anthropic usage。
- 流式 chunk 映射为 Anthropic message/content block 生命周期事件。
- 流式 `delta.content` 只允许字符串或明确 text block；不得使用通用 map/list flatten
  将未知或非文本结构拼接为文本。

### 12.3 转换前拒绝规则

以下内容在任何方向出现时均应在访问上游前返回 `conversion_unsupported`：

- tools、tool_choice、tool_calls、tool role。
- functions、function_call。
- response_format 或 JSON schema。
- image、image_url、input_image、audio、input_audio、document、file。
- tool_use、tool_result 和其它非 text content block。
- 无法映射的角色或 provider 私有必需字段。

若不支持的内容只在上游响应或流中出现，由于 HTTP 可能已经开始写出，应记录 `outcome=conversion`、归档
脱敏错误并终止流；不能切换 provider，也不能伪造正常终止事件。

buffered 响应在客户端响应头写出前发现不支持内容时，必须返回客户端协议兼容的
typed conversion error。usage、metadata 和 metrics 必须在首次记录时就使用 `outcome=conversion`，
不得仅在 metadata 后处理中修正。

### 12.4 转换模式的上游错误

转换模式下必须保留上游 HTTP status，但错误 body 应转换成客户端协议可解析的错误 envelope：

- Anthropic upstream → OpenAI client：输出 OpenAI-compatible `error` object。
- OpenAI upstream → Anthropic client：输出 Anthropic-compatible `error` object。
- 无法安全解析上游错误时，输出 ai-proxy typed upstream error，不能把另一种协议的原始错误结构直接交给
  客户端 SDK。
- 转换后的错误只保留安全摘要、status、RouteOwner 和 request ID；不得包含 Authorization、API Key、带凭据
  URL 或不受限的完整上游 body。

## 13. Typed Error 合同

| HTTP | Code | 触发条件 | 是否访问上游 |
| --- | --- | --- | --- |
| 400 | `model_required` | body 缺少 model | 否 |
| 400 | `model_not_found` | model 不在 resolved catalog | 否 |
| 400 | `operation_unsupported` | model 未声明入站 path 对应 operation | 否 |
| 400 | `endpoint_unsupported` | RouteOwner 无法生成该 endpoint 的 TransportPlan | 否 |
| 400 | `conversion_unsupported` | payload 超出转换保证范围 | 否 |
| 400 | `invalid_request` | JSON 非法、body 结构无法解析或字段类型错误 | 否 |
| 401 | `authentication_failed` | 入站 API Key 缺失或无效 | 否 |
| 413 | `request_too_large` | 请求体超过本地上限 | 否 |
| 500 | `route_contract_invalid` | resolved authority 在运行期不完整 | 否 |
| 500 | `proxy_internal_error` | 代理内部归档、编码或其它不可恢复错误 | 否 |
| 503 | `provider_unavailable` | RouteOwner 缺失、disabled 或不可构造请求 | 否 |
| 502 | `upstream_unavailable` | 唯一 RouteOwner 网络失败、响应读取/解压失败或未返回 HTTP 响应 | 是 |

上游已响应的 HTTP 状态码和响应体按 native/转换合同处理。错误中不得包含 API Key、Authorization、完整上游
敏感响应或带凭据 URL。

`APIError` 必须保留以下稳定字段：

```go
type APIError struct {
    Code             string `json:"code"`
    Message          string `json:"message"`
    Model            string `json:"model,omitempty"`
    Operation        string `json:"operation,omitempty"`
    ClientEndpoint   string `json:"client_endpoint,omitempty"`
    ClientProtocol   string `json:"client_protocol,omitempty"`
    UpstreamProtocol string `json:"upstream_protocol,omitempty"`
    Feature          string `json:"feature,omitempty"`
}
```

`APIError` 是内部稳定错误 authority，对外 envelope 必须根据 Client Protocol 编码：

- OpenAI endpoint：OpenAI-compatible `{"error":{"message","type","code",...}}`。
- Anthropic `/v1/messages`：Anthropic-compatible
  `{"type":"error","error":{"type","message"}}`；稳定 code 以前缀写入 `message`，因为 Anthropic 标准
  error object 不定义独立 `code` 字段。
- OpenAI envelope 的 `code` 保留上表稳定 code；Anthropic envelope 的 message 前缀保留同一 code；
  两者的 `type` 都映射到对应客户端 SDK 可识别的错误类型。
- 路由、preflight、JSON 解析、body 超限、代理内部失败和未得到 HTTP 响应的上游失败
  都必须走该 typed encoder；不得使用 `http.Error` 返回自由文本。

## 14. `/v1/models` 客户端发现合同

`/v1/models` 只发布 ai-proxy 自己已经校验的模型 authority：

- model ID、容量和 operations。
- 稳定排序、具体 DTO、GET/POST 一致。
- 不输出 `created`、RouteOwner/provider 名、provider API Key、BaseURL 或转换内部实现。

客户端只能用 `operations` 判断最低业务能力：

- `chat_completions` 表示第 5 节定义的基础文本合同。
- `embeddings` 表示 native embeddings 合同。

客户端不得从模型名称或使用的 SDK 推断高级 feature、上游协议或 direct endpoint。未来若需要工具、多模态或
结构化输出发现能力，应新增版本化的公开字段，而不是改变 operations 语义。

## 15. 配置加载与启动校验

`config.Load` 必须完成：

1. provider 名、显式 protocol、显式 base URL、显式 models、enabled 状态和 direct endpoint capabilities 校验；
   models 不按 provider 名、protocol 或常见模型家族推导。
2. `allow_unauthenticated=false` 时 API Key 必填；只有显式无认证的 loopback 上游允许空 Key。
3. protocol × direct endpoint compatibility 校验。
4. model ID、容量、operations、exact 唯一校验；大小写不同的 ID 不冲突。
5. exact catalog model 唯一匹配 enabled provider profile，并写入 RouteOwner。
6. model operation × canonical path × RouteOwner TransportPlan readiness 校验。
7. 对每个具体 catalog model 校验 provider profile 匹配数只能为一；pattern 交集通过配置评审治理。
8. resolved authority 生成后保持只读；Handler 不修补 catalog、operation、RouteOwner 或 endpoint capability。

空 catalog 可以保留为空，但不能从 provider pattern 自动合成模型。任何配置错误都在监听端口前启动失败。

代码收口时必须同时使用正式 `config.yaml` 执行一次加载验证。临时合规配置的 live
结果不能替代部署配置 readiness。模型目录排序固定使用 case-fold 主键与 exact ID 二级键，排序规则不得
被误解为 case-fold 唯一规则。

## 16. Provider 能力一致性治理

配置声明是运行时 authority，但必须有外部证据支撑。每个 provider profile 应维护以下审计信息：

- 上游协议和 endpoint 官方文档或内部合同引用。
- 已验证的 model pattern 或具体 model ID。
- context window、max output 和 operation 的来源。
- 验证日期、环境、验证人/任务和结果摘要。

live probe 是显式运维动作，不属于服务启动：

1. 为每个 provider+direct endpoint 选择已登记 catalog model。
2. 使用最小、非流式请求验证 endpoint；必要时独立验证 streaming。
3. 2xx 为成功；404、405、协议不匹配或明确 endpoint-not-supported 为能力漂移。
4. 401/403 表示凭据问题，408/429/5xx 表示环境暂不可判定，不能自动扩展或删除 capability。
5. probe 输出必须脱敏，不记录 API Key 和完整敏感上游响应。
6. provider 文档或模型清单变更时重新验证；定时验证结果用于发布门禁或告警，不在请求期动态改写配置。

当前 profile 的来源、版本、责任角色与复验规则登记在
[`provider-profile-contracts-2026-07-15.md`](provider-profile-contracts-2026-07-15.md)，live 结果登记在
[`provider-capability-audit-2026-07-15.md`](provider-capability-audit-2026-07-15.md)。

为了让验证可重复，代码收口必须提供一个独立运维入口，建议使用
`cmd/ai-proxy-probe`：

```text
go run ./cmd/ai-proxy-probe -config ./config.yaml -provider <route-owner> -capability <capability> -model <model>
```

该入口只读取并校验现有配置，不修改 provider/catalog，不由 ai-proxy server 启动链调用。
每次发布的审计结果归档到 `docs/provider-capability-audit-<date>.md`，至少包含：

- RouteOwner、protocol、direct capability、model、验证时间和环境。
- 脱敏后的 HTTP status、streaming 是否覆盖、结论与能力漂移处置。
- 执行人或 CI/运维任务 ID，以及对应 provider 官方文档或内部合同引用。

## 17. 安全、归档与可观测性

- 客户端认证和 provider 认证完全隔离。
- TransportPlan 按 upstream protocol 生成 header allowlist，删除客户端认证、provider override、
  hop-by-hop header 和不适用于上游协议的版本 header。
- 构造上游请求时不得先复制全部入站 header 再做 blocklist 删除。允许透传的通用语义
  header 仅包括 `Content-Type`、`Accept` 和经过验证的 `X-Request-ID`；其它 header 必须由 ai-proxy
  或 RouteOwner 配置重建。
- OpenAI upstream 必须删除 `Anthropic-Version`、`Anthropic-Beta`、`X-API-Key` 及 Anthropic SDK
  私有 header。Anthropic upstream 的 `Anthropic-Version` 由 ai-proxy 固定值或未来显式 provider
  配置生成，不直接信任客户端传入值。
- OpenAI upstream 使用 RouteOwner 的 Bearer/API Key 合同；Anthropic upstream 使用 RouteOwner 的 API Key 和
  Anthropic version 合同。不得根据客户端使用的 SDK 复用认证方式。
- 日志、归档和 typed error 对 Authorization、X-API-Key、Cookie 等敏感字段脱敏。
- metadata 至少记录 model、RouteOwner、client endpoint、operation、upstream protocol/path、conversion mode、
  HTTP status、outcome、usage 和 request ID。
- usage record 必须扩展 `operation`、`client_endpoint`、`upstream_protocol`、
  `upstream_endpoint`、`conversion_mode`；CSV schema 变更继续使用已有安全轮换机制。
- metrics 的 provider label 使用稳定 RouteOwner ID；request 指标同时使用有界枚举
  `route/client_endpoint`、`upstream_protocol`、`upstream_endpoint`、`conversion_mode`。不得把原始 URL
  或任意客户端输入放入 label。
- SLO 仍以 RouteOwner 为主聚合边界，不要求把全部 TransportPlan 字段扩展为 SLO label；
  但 SLO 使用的 provider ID、attempt 和 outcome 必须来自同一次 TransportPlan 执行记录。
- 流式首包前探测失败、网络错误、408、429 和 5xx 均只记录唯一 RouteOwner，不切换 provider。

## 18. 客户端使用指南

### 18.1 OpenAI SDK

- Base URL 指向 `http://<ai-proxy>/v1`。
- API Key 使用 ai-proxy inbound API Key。
- 使用 `/v1/models` 返回的裸 model ID。
- 可调用 chat completions、responses、completions、embeddings；是否可执行由 model operation 和
  TransportPlan 决定。

### 18.2 Anthropic SDK

- Base URL 指向 `http://<ai-proxy>`，由 SDK 调用 `/v1/messages`。
- API Key 使用 ai-proxy inbound API Key。
- 使用 `/v1/models` 返回的裸 model ID。
- RouteOwner 为 OpenAI protocol 时，ai-proxy 自动执行基础文本 Anthropic→OpenAI 转换。

### 18.3 客户端通用规则

- 客户端不选择 provider，不读取本地 provider 配置，也不推断上游协议。
- `operation_unsupported` 表示应更换模型或业务 operation。
- `endpoint_unsupported` 表示当前模型 RouteOwner 不提供该 SDK endpoint。
- `conversion_unsupported` 表示请求使用了跨协议保证范围之外的 feature；客户端应简化请求，或使用部署方
  明确发布了对应高级 feature 合同的模型/endpoint。通用 `/v1/models.operations` 不提供这类高级能力推断。
- 客户端不得在错误后自动改写 model 为另一个 provider 的模型来规避 catalog 合同。

## 19. 代码职责与落点

不新增 framework module，沿用现有 focused package：

- `internal/config/config.go`：配置解析、normalize、RouteOwner 和 canonical readiness。
- `internal/proxy/route.go`：Client Endpoint/Operation 解析和 TransportPlan 矩阵。
- `internal/proxy/handler.go`：公共请求生命周期、authority 查找、native 转发。
- `internal/proxy/anthropic.go`：OpenAI↔Anthropic 请求、响应和 SSE 转换器。
- `internal/proxy/api_error.go`：稳定 typed error DTO 和 code。
- `internal/proxy/models.go`：模型目录外部 DTO。
- `internal/stats/recorder.go`：usage record 与 CSV TransportPlan 字段、schema 轮换。
- `internal/metrics/registry.go`：有界 TransportPlan request label 与 RouteOwner SLO 聚合数据。
- `cmd/ai-proxy-probe`：独立运维 probe 入口；不参与 ai-proxy server 运行时路由。

Handler 只消费已解析 authority 和 TransportPlan。转换器不选择 provider，config 包不解析请求 body，models
handler 不构造 route。

usage、metadata 和 metrics 不得分别重新推导 protocol/path/mode；它们必须接收同一
TransportPlan 或由该 plan 一次性投影出的不可变 observation DTO。

## 20. 测试与验收矩阵

必须覆盖：

### 配置

- protocol、endpoint capability、operation 的未知值、重复值和排序。
- protocol/base URL 缺失时启动失败，不根据 provider 名补默认值。
- 远程 provider 空 API Key 启动失败；仅 `allow_unauthenticated=true` + loopback + 空 Key
  组合可通过。
- protocol × direct endpoint 不兼容。
- catalog model 零匹配、多匹配、disabled provider；model ID exact 唯一且区分大小写（DeepSeek-V4-Flash 与 deepseek-v4-flash 可并存）。
- operation canonical readiness 不成立时启动失败。
- 同厂商拆 profile 后，任一具体 catalog model 命中多个 profile 时必须启动失败。
- 正式 `config.yaml` 可被 `config.Load` 成功加载；model ID 严格区分大小写，仅 exact 重复失败。

### 路由与转换

- OpenAI Chat → OpenAI native。
- OpenAI Chat → Anthropic conversion。
- Anthropic Messages → Anthropic native。
- Anthropic Messages → OpenAI conversion。
- responses、completions、embeddings 仅允许矩阵中的 native 组合。
- stream/non-stream 基础文本双向转换。
- system/user/assistant、采样参数、stop 和 usage 映射。
- OpenAI 字符串 `stop` 规范为 Anthropic `stop_sequences` 数组。
- tools、function calling、多模态和 response_format 在转换前 typed 拒绝且不访问上游。
- 转换模式下上游错误 status 保留，并输出客户端协议兼容的安全错误 envelope。
- 响应期或流式转换出现不支持内容时记录 conversion outcome，不伪造成功终止。
- OpenAI 流式响应中的非文本 `delta.content` 必须终止转换，不得通过 flatten
  产生伪文本。
- buffered 响应转换失败在 usage、metadata、metrics 中均为 `outcome=conversion`。

### Authority 与错误

- `/v1/models` 与请求路由使用同一 ResolvedModelRoute。
- GET/POST models 整体 DTO 深度一致。
- 所有本地拒绝 typed error 不泄露 secret。
- invalid JSON、request too large、代理内部失败和上游网络失败不得返回
  `text/plain` 自由文本。
- OpenAI 与 Anthropic 入站端点的本地 typed error 都能被对应官方 SDK 解析。
- 网络错误、首事件失败、408、429、5xx 只访问一个 RouteOwner。
- usage、metadata、metrics 和 SLO 使用相同 RouteOwner ID。
- usage 与 metadata 包含相同 TransportPlan 字段；request metrics 的 protocol/endpoint/mode label
  值只来自有界枚举。
- Anthropic→OpenAI 时上游不得收到 `Anthropic-Version`、客户端 `X-API-Key` 或其它
  Anthropic SDK 私有 header；反向路径也必须验证协议头隔离。

### 客户端验收

- 使用官方 OpenAI SDK 对真实 ai-proxy HTTP server 执行 models、chat 和 embeddings 主路径；
  不能仅用手写 HTTP request 代替。
- 使用官方 Anthropic SDK 对真实 ai-proxy HTTP server 执行 messages 主路径，并验证
  native/conversion 两种 RouteOwner。
- 两类 SDK 使用相同裸 model ID 时均由 ai-proxy 决定 RouteOwner 和是否转换。
- 客户端不配置或传递 provider override。
- SDK 验收必须覆盖客户端可解析的本地 typed error 与转换模式上游错误。

## 21. 实施顺序

已完成的基础批次不重复实施：`ResolvedModelRoute`、`TransportPlan`、路由矩阵、
conversion preflight、基础双向转换和转换模式上游错误 envelope。剩余代码收口必须
按以下顺序执行：

1. **C01 部署配置与 provider 认证**
   - 验证正式 `config.yaml` 的 model ID exact 唯一；允许大小写不同 ID 共存。
   - 删除按 provider 名补 protocol/base URL 的隐式行为。
   - 实现 `allow_unauthenticated` 与远程空 Key fail-fast，补配置测试。
2. **C02 上游 header 与认证隔离**
   - 将 `newUpstreamRequestForPath` 改为协议 allowlist 构造。
   - 补 OpenAI↔Anthropic 双向版本头、认证头和 SDK 私有头隔离测试。
3. **C03 转换严格性与 outcome**
   - 规范 OpenAI 字符串 stop。
   - 删除转换流中的通用 content flatten，对非文本响应 typed/显式终止。
   - 使 buffered/stream 转换失败在 usage、metadata、metrics 统一记录 `conversion`。
4. **C04 typed error 外部合同**
   - 增加 `request_too_large`、`proxy_internal_error`、`upstream_unavailable`。
   - 删除执行端点的 `http.Error`，按 Client Protocol 输出 OpenAI/Anthropic SDK 可解析 envelope。
5. **C05 TransportPlan 可观测性**
   - 扩展 `stats.Record`/CSV schema 和 request metrics 有界 label。
   - 使 usage、metadata、metrics、SLO 共用同一请求期观测上下文，补一致性测试。
6. **C06 SDK 与运维验收**
   - 增加真实 OpenAI SDK / Anthropic SDK 兼容测试。
   - 增加独立 provider probe 入口，产生脱敏审计文档。
7. **C07 文档与跨仓合同**
   - 更新 README、PRD、config example、正式部署配置和 closure plan。
   - 本仓只记录 WorkOrch 后续需要同步的合同；WorkOrch 文档与 catalog refresh/live 联调由后续独立任务完成。
8. **C08 最终门禁**
   - 执行全量 Go 测试、vet、gofmt、diff check、正式 config 启动校验、SDK 验收和
     provider direct capability live 验证；WorkOrch live 由后续独立任务验收。

## 22. Definition of Done

- [x] 所有对外 endpoint、operation、provider direct capability 和协议转换语义在本文中有唯一解释。
- [x] 配置加载生成唯一 model route authority；请求期生成唯一 TransportPlan。
- [x] native 与 conversion 路径边界明确，转换前拒绝的主要 feature 有直接测试。
- [x] 正式 `config.yaml` 可加载并启动；model ID 严格区分大小写；provider protocol/base URL/认证均为显式合同。
- [x] 上游请求头使用协议 allowlist，客户端与 provider 认证/版本头完全隔离。
- [x] 转换的 stop 映射、非文本响应拒绝和 buffered/stream outcome 一致性均有直接测试。
- [x] 所有本地失败使用 typed error，并根据 OpenAI/Anthropic Client Protocol 输出 SDK 可解析 envelope。
- [x] usage、metadata、request metrics 和 SLO 与同一 TransportPlan/RouteOwner authority 一致。
- [x] 真实 OpenAI SDK 与 Anthropic SDK 不需了解 provider 配置或上游协议即可完成主路径与错误解析。
- [ ] 每个 enabled provider 的每个已声明 direct capability 都有文档来源和可审计 live 结论；streaming
  支持项单独记录。（当前仅部分 `chat_completions` 已 live）
- [x] 独立 probe 入口（cmd/ai-proxy-probe）输出完整合同字段并对摘要脱敏。
- [x] README、PRD、配置示例、部署配置、closure plan 与 ai-proxy 本仓合同一致；WorkOrch 标记为后续同步。
- [x] 当前代码重新通过 `go test ./...`、`go vet ./...`、`gofmt -l` 和 `git diff --check`；正式
  `config.yaml` 成功加载到监听阶段（沙箱 bind 被拒绝不属于配置错误）。
- [ ] 完成 ai-proxy 代码门禁与 direct capability live 审计后归档最终证据；WorkOrch 联调单独验收。

只有上述所有条目勾选后，才能将本文 `Status` 从 `active` 更新为 `completed`，
并删除 `Review State: code-closure-required`。

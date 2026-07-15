# WorkOrch 模型目录与 Operation 合同收口计划

Status: active

Type: closure-plan

Last Updated: 2026-07-15

## 1. 文档目的

本文定义 ai-proxy 为 WorkOrch 提供模型自动枚举、上下文容量和 operation readiness 时必须完成的
上游合同收口。对应 WorkOrch 设计文档为：

`/home/rangh/aispace/workorch/docs/70-roadmap/active/ai-proxy-primary-llm-provider-cutover-design-2026-07-15.md`

provider capability、模型 operation、协议转换及客户端使用的独立功能设计，见
[`provider-capability-contract-design-2026-07-15.md`](provider-capability-contract-design-2026-07-15.md)。

本次不保留缺少 operation 的旧 `model_catalog` 合同，也不通过模型名、默认 provider 或上游请求失败
猜测模型能力。完成后，ai-proxy 的 `model_catalog` 必须同时成为模型容量、operation 和确定路由的权威。

## 2. 当前核对结论

**2026-07-15 收口状态：代码、测试与文档侧已满足 live 验证前置条件。**

已完成：

1. Typed `/v1/models` DTO；无 `map[string]any`。
2. `config.Load`：operations 必填、exact 唯一（区分大小写）、容量完整、唯一 RouteOwner。
3. catalog operations × RouteOwner endpoint_capabilities 交叉校验。
4. Provider 仅配置文件声明；不支持 env 注入 provider；不支持 `fallbacks`。
5. **已删除 `default_provider` 配置/环境变量/Handler 兜底**；声明 `default_provider` 启动失败。
6. 显式 `endpoint_capabilities`；不从 protocol 推断全量 endpoint。
7. 请求前 exact model + operation + endpoint capability 校验；失败 typed 4xx，不访问上游。
8. `NewHandler.requireResolvedConfig` 全量 fail-fast（含 table-driven 畸形配置覆盖）。
9. GET/POST `/v1/models` `reflect.DeepEqual` 全 payload 一致；首事件失败、5xx/408/429/网络错误仅访问唯一 RouteOwner。
10. `api_error.go`、`handler_test_helpers.go`、probe 与本文档已纳入当前代码变更集。
11. ai-proxy 全量 Go 测试、vet、gofmt、diff check 已通过；本地 `config.yaml` 成功加载到监听阶段。

待 live 手工验证（§12）与跨仓 WorkOrch catalog refresh 联调后，可将本文 status 改为 `completed`。

## 3. 目标语义

收口完成后，ai-proxy 必须满足：

1. `/v1/models` 只返回规范化、可路由的具体模型。
2. 每个模型明确提供容量、operations 和确定 route owner。
3. 所有模型请求在访问上游前校验 exact model 与 requested operation。
4. operation 不支持时由 ai-proxy 本地返回稳定 4xx，不依赖上游错误发现能力。
5. catalog、路由和执行使用同一份规范化 `ModelInfo` authority。
6. WorkOrch 可以只依赖 `/v1/models` 判断新 Run 是否可使用某个 target/operation 的 canonical 执行路径。
7. HTTP Handler 只消费已经 normalize/validate 的只读 authority，不合成 model、不补默认容量、不补默认
   operations，也不重新猜测 RouteOwner。
8. provider 的 endpoint capability 必须来自显式配置或可审计的确定合同，不能仅因 protocol 名为 `openai`
   就假定其支持 embeddings、responses 或 completions。
9. 一个 catalog model 只有在其 RouteOwner 能服务该 model 每个 operation 的 canonical 入站 path 时才能通过
   启动校验并出现在 `/v1/models` 中。
10. 不存在 provider fallback 或 default provider 兜底；请求只访问 catalog 已解析的唯一 RouteOwner。

## 4. 统一 Operation 合同

只允许以下枚举：

```go
const (
    ModelOperationChatCompletions = "chat_completions"
    ModelOperationEmbeddings      = "embeddings"
)
```

入站 path 到 operation 的映射固定为：

| 入站 path | operation |
| --- | --- |
| `/v1/chat/completions` | `chat_completions` |
| `/v1/messages` | `chat_completions` |
| `/v1/responses` | `chat_completions` |
| `/v1/completions` | `chat_completions` |
| `/v1/embeddings` | `embeddings` |
| `/v1/models` | 不适用，仅返回能力目录 |

该映射是本次面向 WorkOrch 的粗粒度执行合同。若未来需要区分 responses、legacy completions 或其他能力，
必须新增明确枚举并同步 WorkOrch，不得在现有枚举中隐式扩展。

readiness 保证范围固定为：`chat_completions` 保证 canonical `/v1/chat/completions` 可执行，`embeddings` 保证
canonical `/v1/embeddings` 可执行。`/v1/messages`、`/v1/responses`、`/v1/completions` 虽映射到
`chat_completions` 做模型 operation 校验，但是否可执行仍取决于 RouteOwner 的具体 endpoint capability 和转换矩阵；
WorkOrch 若要使用这些非 canonical path，必须显式理解该 path 合同。

## 5. `/v1/models` 具体 DTO

禁止继续使用 `map[string]any` 或 `[]any` 构造模型目录。应定义具体外部协议 DTO：

```go
type ModelsListResponse struct {
    Object string        `json:"object"`
    Data   []ModelRecord `json:"data"`
}

type ModelRecord struct {
    ID                  string   `json:"id"`
    Object              string   `json:"object"`
    Operations          []string `json:"operations"`
    ContextWindowTokens int      `json:"contextWindowTokens,omitempty"`
    MaxOutputTokens     int      `json:"maxOutputTokens,omitempty"`
}
```

输出规则：

- `object` 固定为 `list`。
- `operations` 始终输出非 nil 数组，且至少包含一个已知 operation。
- operation 使用规范化稳定顺序。
- model 按 case-fold ID 主键、exact ID 二级键稳定排序；该排序不改变 exact 唯一语义。
- GET、POST `/v1/models` 返回相同业务合同。
- response 不返回 `created`、`owned_by`、provider 名称、secret、BaseURL 或内部 fallback 配置。

目标响应示例：

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-4o",
      "object": "model",
      "operations": ["chat_completions"],
      "contextWindowTokens": 128000,
      "maxOutputTokens": 16384
    }
  ]
}
```

## 6. Catalog 配置校验

每个 `model_catalog` 条目必须满足：

- ID trim 后非空。
- ID trim 后 exact 全局唯一，严格区分大小写。
- ID 不包含控制字符，并设置合理最大长度。
- operations 非空，只包含已知枚举，去重并稳定排序。
- `context_window_tokens >= 0`。
- `max_output_tokens >= 0`。
- 正值容量下，`max_output_tokens < context_window_tokens`。
- 若容量不完整，可选择启动失败；如果保留展示型 incomplete model，则必须在目录中明确不可运行，不能被
  WorkOrch 识别为 ready。首选方案是部署配置只登记容量完整的可运行模型。

配置 map key 与 `ModelInfo.ID` 的 exact ID 冲突时必须启动失败。`GPT-4o` 和 `gpt-4o` 是两个不同模型，
允许同时存在；WorkOrch 后续同步时也必须按 exact ID 建索引，不得 case-fold 合并或拒绝整表。

空 catalog 可以选择启动失败，也可以返回空 `/v1/models`；但无论选择哪一种，Handler 都不得根据 provider
pattern、常见模型名或测试 fixture 自动合成 catalog 条目。所有默认值和兼容 fixture 必须只存在于测试 helper。

## 7. Catalog 与 Provider 路由一致性

每个 catalog model 在启动配置校验阶段必须匹配且只匹配一个 enabled provider：

- 零匹配：启动失败，提示 model 无可用 route owner。
- 多匹配：启动失败，提示 provider model pattern 重叠。
- 唯一匹配：将该 provider 作为仅内部使用的确定 RouteOwner。

禁止通过公开字段掩盖零匹配或多匹配；RouteOwner 必须来自已经通过启动校验的确定路由结果，但不得出现在
`/v1/models` 响应中。

建议在 normalize/validate 后生成只读的 resolved model route table，HTTP catalog 和 provider request routing
共同读取该表，避免两条路径分别遍历 provider map 后产生漂移。

RouteOwner 解析后必须继续做 operation readiness 交叉校验：

| model operation | canonical 入站 path | RouteOwner 完成条件 |
| --- | --- | --- |
| `chat_completions` | `/v1/chat/completions` | provider 可直通该 path，或具备已实现的 messages→chat 转换能力 |
| `embeddings` | `/v1/embeddings` | OpenAI provider 显式声明 `embeddings` |

任一 operation 无 canonical path capability 时必须启动失败，禁止把该 model 输出到 `/v1/models` 后再在请求期
返回 `endpoint_unsupported`。responses、completions、messages 等具体 path 仍在请求前按 provider capability
和转换矩阵单独校验。

`NewHandler`、route helper 和 `/v1/models` handler 不得修补这张表。若生产调用方绕过 `config.Load` 传入未解析
Config，应在构造阶段 fail-fast，而不是自动补齐。

## 8. 请求前 Operation Enforcement

所有包含 model 的 LLM 请求在访问上游前必须执行：

1. 解析入站 path 对应 operation。
2. trim 并读取 exact model ID。
3. 从规范化 catalog/route table 查找 model。
4. 校验 model 声明 requested operation。
5. 获取已经确定的 route owner。
6. 校验 provider protocol 和入站 path 的转换能力。
7. 通过后才能创建上游请求。

失败语义：

- model 为空：`400 model_required`。
- model 不存在：`400 model_not_found`。
- operation 不支持：`400 operation_unsupported`。
- route owner 不具备当前入站 path 的直通或转换能力：`400 endpoint_unsupported`。
- model route 不完整：启动时失败；运行时若仍发生则返回 `500 route_contract_invalid` 并记录告警。
- provider 配置不可用：`503 provider_unavailable`。

错误响应必须是具体结构，不能只依赖自由文本：

```go
type APIErrorResponse struct {
    Error APIError `json:"error"`
}

type APIError struct {
    Code      string `json:"code"`
    Message   string `json:"message"`
    Model     string `json:"model,omitempty"`
    Operation string `json:"operation,omitempty"`
}
```

错误中不得包含 API Key、Authorization、完整上游响应体或带凭据 URL。

## 9. 唯一 RouteOwner 与 Provider Endpoint Capability 合同

本轮最终合同不支持 provider fallback：

- provider 配置出现 `fallbacks` / `fallback_providers` 时启动失败。
- model 不存在、operation 不支持、RouteOwner 不可用或 endpoint 不支持时直接返回 typed error。
- 网络错误、408、429、5xx 只由唯一 RouteOwner 返回，不切换其它 provider。
- `default_provider` 不参与请求路由；后续应删除该无效配置和环境变量，避免误导。
- usage/metadata 的 provider 始终是 catalog 已解析的 RouteOwner。

### 9.1 Endpoint Capability 枚举

本轮收口不再仅按 `provider.Protocol` 猜测端点能力。应为 provider 增加显式、可规范化校验的
`endpoint_capabilities` 配置，枚举固定为：

```text
chat_completions
messages
responses
completions
embeddings
```

配置示例：

```yaml
providers:
  openai:
    protocol: openai
    endpoint_capabilities: chat_completions, responses, completions, embeddings

  deepseek:
    protocol: openai
    endpoint_capabilities: chat_completions

  anthropic:
    protocol: anthropic
    endpoint_capabilities: messages
```

规范化和验证规则：

1. enabled provider 的 capability 非空，只允许已知枚举，去重并稳定排序。
2. 直通能力必须由 provider 显式声明；不得因为 `protocol: openai` 默认补齐所有 OpenAI endpoint。
3. 协议转换能力由 ai-proxy 的已实现转换矩阵派生：
   - OpenAI provider 声明 `chat_completions` 后，可通过转换服务 `/v1/messages`。
   - Anthropic provider 声明 `messages` 后，可通过转换服务 `/v1/chat/completions`。
   - responses、completions、embeddings 不允许通过上述转换隐式获得。
4. normalize/validate 后生成或提供确定的 provider inbound-path capability 查询。
5. RouteOwner 解析后，启动时按 §7 的 operation→canonical path 表逐项验证 model readiness。
6. 请求时按当前入站 path 校验唯一 RouteOwner；不满足时返回 typed `endpoint_unsupported`，不访问上游。
7. 不构建 fallback candidate，不因 retryable status 或网络错误切换 provider。

## 10. 配置和文档同步

必须同步更新：

- `config.example.yaml`。
- 实际部署使用的 `config.yaml` 或外部挂载配置。
- `README.md` 模型目录说明和响应示例。
- `prd.md` 对模型目录和 operation enforcement 的验收项。
- provider `endpoint_capabilities` 的配置说明、协议转换矩阵和唯一 RouteOwner 示例。
- 无 provider fallback、无 default provider 兜底、provider 不能由 env 创建的最终语义。

README 不得继续声明“代理本身不按 operations 拒绝 path”。最终说明应明确：

> operations 是 ai-proxy 执行合同；请求在访问上游前按 exact model 和入站 operation 校验。

配置示例：

```yaml
providers:
  openai:
    protocol: openai
    endpoint_capabilities: chat_completions, responses, completions, embeddings

model_catalog:
  gpt-4o:
    context_window_tokens: 128000
    max_output_tokens: 16384
    operations: chat_completions

  text-embedding-3-large:
    context_window_tokens: 8192
    max_output_tokens: 8191
    operations: embeddings
```

## 11. 逐项代码收口顺序

### AP01：Typed Catalog DTO

涉及：

- `internal/proxy/models.go`
- `internal/proxy/handler_test.go`

要求：

1. 删除 models response 中的 `map[string]any`、`[]any`。
2. 增加具体 response/record DTO。
3. 仅输出 operations、容量等客户端业务能力；不输出 RouteOwner、`owned_by` 或 `created`。
4. 保证 GET/POST 合同一致和稳定排序。

完成条件：模型目录响应可以直接解码为具体类型，不存在动态字段组装。

### AP02：Model Route Authority

涉及：

- `internal/config/config.go`
- `internal/proxy/models.go`
- provider routing helper

要求：

1. model ID exact 唯一且严格区分大小写。
2. 每个 catalog model 唯一匹配 enabled provider。
3. normalize 后生成 resolved route table 或等价确定结构。
4. catalog 与请求路由使用同一 authority。
5. 删除生产 `NewHandler` 中的 `materializeModelCatalog` 调用及常见测试模型注入。
6. Handler 不补 operations、容量或 RouteOwner。
7. `config.Load` 在 RouteOwner 解析后校验每个 model operation 的 canonical path capability。
8. 若保留公开 `NewHandler(Config, ...)`，其 fail-fast 检查必须覆盖已知 operation/capability、容量关系、
   exact 唯一（区分大小写）、RouteOwner 存在且匹配 model、operation readiness、provider protocol 合法性，以及
   operations/capabilities 无重复且顺序已规范化；也可以改为只接收已解析 authority。

完成条件：目录中不存在不可路由、路由歧义或 operation readiness 不成立的模型，且空 catalog 不会在 Handler
中变成合成模型目录。

### AP03：Operation Enforcement

涉及：

- `internal/proxy/handler.go`
- `internal/proxy/anthropic.go`
- `internal/proxy/route.go`
- operation/error helper

要求：

1. path 映射为 operation。
2. 上游请求前校验 model operation。
3. 不支持时返回稳定 typed 400。
4. chat、messages、responses、completions 和 embeddings 全部覆盖。
5. primary provider 不具备当前 path 时返回 typed `endpoint_unsupported`，且不创建上游请求。

完成条件：任何不符合 catalog operation 的请求都不会访问上游。

### AP04：Provider Endpoint Capability Validation

涉及：

- `internal/config/config.go`
- endpoint capability 解析、规范化和配置校验
- `config.example.yaml` / 部署 `config.yaml`

要求：

1. 增加并规范化 provider `endpoint_capabilities`。
2. enabled provider capability 非空，只允许已知枚举，去重并稳定排序。
3. protocol 与 capability 不兼容时启动失败。
4. catalog model operation 与 RouteOwner canonical path capability 不匹配时启动失败。
5. 请求期校验当前具体 path；失败返回 `endpoint_unsupported` 且不访问上游。
6. 不得把 `protocol: openai` 等价为支持所有 OpenAI endpoint。
7. 配置中声明 `fallbacks` 时启动失败，retryable status 不切换 provider。

完成条件：`/v1/models` 中每个 operation 都能由唯一 RouteOwner 执行；任何具体 path 不支持时由本地 typed
error 拒绝，不会访问错误 endpoint 或其它 provider。

### AP05：配置与文档刷新

要求：

1. 所有 model catalog fixture 和部署配置补齐 operations。
2. README 删除“只展示、不执行校验”的中间态说明。
3. config example 增加 chat/embedding 模型示例。
4. README 删除任何将 RouteOwner/`owned_by` 暴露给客户端的旧说明。
5. README、config example 和代码注释不再声明 catalog 与 provider 路由独立/解耦。
6. 文档补充 provider endpoint capability、canonical operation readiness 和无 fallback 语义。
7. 删除“仅用环境变量即可启动”、`AI_PROXY_DEFAULT_PROVIDER` 路由兜底、`API_KEY/API_BASE_URL` 创建 custom
   provider 等已失效说明。
8. README/PRD 中 provider 示例全部补齐 endpoint capabilities。
9. 删除 fallback 成功切换、fallback 指标和 fallback 归档用途等产品合同。
10. 删除 catalog 与 provider route “独立/解耦”的旧注释。
11. 从 `Config`、配置解析、环境变量、`config.example.yaml` 中删除不参与路由的 `default_provider`；如果因兼容
    暂时保留，必须明确 deprecated/ignored，且不能继续作为推荐配置出现。
12. 不记录或提交真实 API Key。

完成条件：按仓库文档复制配置即可启动，且语义与代码一致。

### AP06：测试与 Live 验证

完成以下测试后才允许标记完成：

- operation 配置解析、未知值拒绝、去重和稳定排序。
- exact duplicate model ID 拒绝；大小写不同 ID 可共存并保持确定排序。
- catalog model 零 provider 匹配拒绝。
- catalog model 多 provider 匹配拒绝。
- `/v1/models` typed DTO、operations 和稳定顺序。
- GET、POST `/v1/models` 解码为同一 typed DTO 且业务响应一致。
- chat-only model 调用 embeddings 被拒绝且不访问上游。
- embedding-only model 调用 chat/messages/responses/completions 被拒绝。
- 支持 operation 时正常转发。
- endpoint capability 缺失、未知值、去重、稳定排序和 protocol 不兼容启动失败场景。
- catalog model operation 与 RouteOwner capability 不匹配时启动失败。
- retryable status / 网络错误不会切换到其它 provider；配置声明 `fallbacks` 被拒绝。
- 错误体不包含密钥和上游敏感内容。
- 空 catalog 不合成模型；缺失 operations/容量/RouteOwner 的未解析 Config 不被 Handler 自动修复。
- `NewHandler` 对畸形 operation/capability、容量关系、RouteOwner 匹配和 exact duplicate fail-fast，或构造器
  已改为只接收不可构造的 resolved authority。
- `NewHandler` 对 unknown protocol、重复或未规范排序的 operations/capabilities fail-fast；空 catalog 不能绕过
  provider authority 校验。
- GET、POST `/v1/models` 的整个 typed payload 深度一致，不只比较部分字段。
- `internal/proxy/api_error.go`、`internal/proxy/handler_test_helpers.go` 已纳入变更集并通过 `gofmt`。

已补齐的直接测试包括：

1. `TestLoadRejectsOpenAIMessagesCapability` 与 `TestLoadRejectsAnthropicEmbeddingsCapability`：protocol/capability
   不兼容启动失败。
2. `TestRequireResolvedConfigMalformedTable`：覆盖 unknown protocol、unknown/duplicate/unsorted capability、
   unknown/duplicate/unsorted operation、非法容量、exact duplicate、错误 RouteOwner 和 operation readiness。
3. `TestRetryableUpstreamErrorDoesNotSwitchProvider`、`TestNetworkErrorDoesNotSwitchProvider`、
   `TestSingleRouteOwnerDoesNotSwitchOnRetryableStatuses` 与 `TestFirstStreamEventFailureDoesNotSwitchProvider`：
   网络错误、408、429、5xx 和首事件失败均只访问唯一 RouteOwner。
4. `TestModelsGETAndPOSTConsistent`：解码后使用 `reflect.DeepEqual` 比较整个 `ModelsListResponse`。

## 12. 验证命令

```bash
gofmt -l $(rg --files -g '*.go')
git diff --check
go test ./internal/config -count=1
go test ./internal/proxy -count=1
go test ./cmd/ai-proxy -count=1
go test ./... -count=1
go vet ./...
```

`gofmt -l` 必须无输出；新增错误 DTO、测试 helper 和本文档必须已纳入最终变更集。

提交前同时确认：

```bash
git ls-files --error-unmatch internal/proxy/api_error.go
git ls-files --error-unmatch internal/proxy/handler_test_helpers.go
git ls-files --error-unmatch docs/workorch-model-catalog-operation-closure-plan-2026-07-15.md
```

live 验证至少覆盖：

1. 携带正确入站 API Key 调用 `GET /v1/models`。
2. 确认每个 model 仅返回 operations、容量等业务能力，不返回 RouteOwner、`owned_by`、`created`。
3. 使用 chat model 调用 `/v1/chat/completions` 成功。
4. 若当前 catalog 发布 embedding model，则使用该 model 调用 `/v1/embeddings` 成功；若未发布，则确认目录
   不包含 `operations: embeddings`，且 WorkOrch 不将其视为可用能力。
5. 使用 chat-only model 调用 embeddings 返回 `operation_unsupported`。
6. 使用 embedding-only model 调用 chat 返回 `operation_unsupported`。
7. 确认被拒绝请求没有产生上游访问；retryable status 不会访问第二个 provider。
8. WorkOrch catalog refresh 成功并能按 operation 分离 chat/embedding selector。

## 13. 完成验收清单

### 配置

- [x] 所有 catalog model 明确配置 operations。
- [x] operation 只允许已知枚举并稳定排序。
- [x] model ID exact 唯一且严格区分大小写；大小写不同 ID 可共存。
- [x] catalog model 唯一匹配 enabled provider。
- [x] 部署配置不包含未补 operations 的旧条目。
- [x] enabled provider 显式配置 endpoint capabilities，运行时不按 protocol 猜测全量 endpoint。
- [x] 配置声明 `fallbacks` 时启动失败，不存在 provider fallback。
- [x] 空 catalog 保持为空，不在 Handler 中合成模型。
- [x] catalog model operation 与 RouteOwner canonical path capability 在启动时交叉校验。

### HTTP Catalog

- [x] `/v1/models` 使用具体 DTO。
- [x] 不使用 `map[string]any`、`[]any` 组装响应。
- [x] operations 始终输出。
- [x] model 顺序稳定。
- [x] RouteOwner 仅内部使用，`/v1/models` 不输出 `owned_by`、`created` 或 provider 名称。
- [x] GET、POST `/v1/models` 的 typed DTO 和稳定排序有直接测试。
- [x] GET、POST `/v1/models` 对整个 typed payload 做深度一致性断言。

### 执行

- [x] 请求前校验 exact model。
- [x] 请求前校验 requested operation。
- [x] 不支持 operation 时不访问上游。
- [x] `NewHandler` 不合成或修补 catalog authority。
- [x] primary RouteOwner 在访问上游前校验当前 path capability。
- [x] endpoint 不支持时返回稳定 typed code 且不访问上游。
- [x] 错误返回稳定 code 且有不泄露 secret 的直接测试。
- [x] `NewHandler` 完整拒绝 unknown protocol、重复/未排序枚举及其它未解析/畸形 Config，或只接收 resolved authority。
- [x] retryable status / 网络错误/首事件失败只访问唯一 RouteOwner，不切换 provider，并有直接测试。

### 测试与联动

- [x] config/proxy/app/all Go 测试通过。
- [x] `go vet ./...` 通过。
- [x] 所有 Go 文件 zero-diff `gofmt`，`git diff --check` 通过。
- [x] endpoint capability 缺失/未知/去重排序和 operation readiness 不匹配测试已补齐。
- [x] protocol/capability 不兼容、完整 Handler fail-fast、单 RouteOwner retryable failure/首事件失败测试补齐。
- [x] README、example、prd 与代码最终语义一致；不再保留 fallback 执行、指标或归档合同。
- [x] 新增错误 DTO、测试 helper 和 closure plan 已纳入 Git 变更集。
- [ ] ai-proxy live chat/embedding/negative operation 验证通过。（代码就绪，待手工 live）
- [ ] WorkOrch catalog refresh、模型过滤和 activation readiness 验证通过。（跨仓，待联调）

只有以上项目全部完成后，本文状态才可以改为 `completed`。

## 14. 最终合同

收口完成后的唯一语义是：

> ai-proxy 的 `model_catalog` 明确声明每个具体模型的容量、operations 和确定 route owner；
> `/v1/models` 使用具体稳定 DTO 输出该 authority；所有 chat/embedding 请求在访问上游前按 exact model
> 和 requested operation 校验；Handler 不合成或修补 catalog；provider path capability 来自显式、可审计合同；
> 每个 operation 的 canonical readiness 在启动时与唯一 RouteOwner 交叉验证；请求期再校验具体 path；不存在
> default provider 兜底或 provider fallback，任何失败都不能绕过目录合同访问其它上游。

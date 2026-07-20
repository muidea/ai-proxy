# ai-proxy

> 本文以当前代码和自动化测试为基线。更新时间：2026-07-16。

## 0. 使用约定

- Goal 描述产品必须达到的用户价值和能力边界。
- DoD 描述可以通过接口、文件、日志、指标或自动化测试独立验证的结果。
- 每个条目只表达一个主要要求，避免在一个条目中混合多个开发任务。
- Goal 和 DoD 使用稳定 ID；实现、测试和变更说明应引用对应 ID。
- `[x]` 表示当前代码已实现；后续开发必须将这些条目作为回归合同。
- 如果基于本 PRD 从零开发，应忽略当前勾选状态并完成全部 DoD。

## 1. Goals

### G-01 统一客户端接入

- **G-01.01** 客户端只需配置一个本地 ai-proxy 地址即可访问已配置的上游模型。
- **G-01.02** OpenAI-compatible 客户端使用标准 OpenAI 路径接入。
- **G-01.03** Anthropic 客户端使用标准 Messages 路径接入。
- **G-01.04** 客户端只提交裸模型 ID，不感知 provider 名称、上游 URL 或上游密钥。
- **G-01.05** 服务提供不依赖上游的健康检查接口。
- **G-01.06** 服务以单个 Go 二进制运行，不依赖数据库、消息队列或常驻中间件。

### G-02 确定性模型路由

- **G-02.01** `model_catalog` 是模型发现、模型能力和请求路由的唯一权威。
- **G-02.02** 模型 ID 使用 exact match，并严格区分大小写。
- **G-02.03** 每个 catalog 模型在启动期解析为唯一 RouteOwner。
- **G-02.04** 请求期只使用已解析的 RouteOwner，不重新扫描 provider 选择路由。
- **G-02.05** 请求失败后不切换到其他 provider。
- **G-02.06** 产品不提供 default provider。
- **G-02.07** 产品不提供 provider fallback。
- **G-02.08** 客户端 header、query 或模型前缀不能改变 RouteOwner。
- **G-02.09** 模型的业务 operation 必须在访问上游前完成校验。
- **G-02.10** provider 的直连 endpoint capability 必须显式配置，不能从 protocol 推导。

### G-03 标准端点与协议适配

- **G-03.01** 支持 OpenAI Chat Completions 原生转发。
- **G-03.02** 支持 OpenAI Responses 原生转发。
- **G-03.03** 支持 OpenAI Completions 原生转发。
- **G-03.04** 支持 OpenAI Embeddings 原生转发。
- **G-03.05** 支持 Anthropic Messages 原生转发。
- **G-03.06** 支持 OpenAI Chat 请求转换为 Anthropic Messages 请求。
- **G-03.07** 支持 Anthropic Messages 请求转换为 OpenAI Chat 请求。
- **G-03.08** 双向转换覆盖基础文本消息和常用生成参数。
- **G-03.09** 双向转换覆盖基础 SSE 流式输出。
- **G-03.10** 不受支持的转换特性必须在访问上游前被拒绝。
- **G-03.11** Responses、Completions 和 Embeddings 不通过协议转换派生能力。
- **G-03.12** 本地模型列表不访问上游。

### G-04 稳定错误与流式结果

- **G-04.01** 代理自身错误使用稳定的 typed error code。
- **G-04.02** OpenAI 入站路径返回 OpenAI-compatible 错误 envelope。
- **G-04.03** Anthropic 入站路径返回 Anthropic-compatible 错误 envelope。
- **G-04.04** 原生转发路径保留上游 HTTP 状态和错误语义。
- **G-04.05** 转换路径保留上游 HTTP 状态，并输出客户端协议兼容的安全错误 envelope。
- **G-04.06** SSE 数据到达后立即向客户端写出，不等待完整响应结束。
- **G-04.07** 流式结果使用独立 outcome 表达首包成功后的真实结束状态。
- **G-04.08** 流式协议必须识别正常终止、显式失败、未完成和异常截断。
- **G-04.09** 客户端取消不能被误判为 provider 故障。

### G-05 安全与资源边界

- **G-05.01** 默认仅监听 loopback 地址。
- **G-05.02** 非 loopback 监听必须启用入站 API Key。
- **G-05.03** 入站认证信息不能转发给上游。
- **G-05.04** 上游认证信息只能由 provider 配置生成。
- **G-05.05** 日志、错误、归档和 probe 输出不能泄露凭据或敏感请求头。
- **G-05.06** 上游请求只允许透传明确的安全 header。
- **G-05.07** 上游响应中的 hop-by-hop header 不能回写给客户端。
- **G-05.08** 入站请求体必须有大小上限。
- **G-05.09** 上游非流式响应必须有大小上限。
- **G-05.10** gzip 解压后的上游响应必须有大小上限。
- **G-05.11** 单次 SSE 流必须有累计大小上限。
- **G-05.12** 单条 SSE 行必须有大小上限。
- **G-05.13** 流式读取必须支持空闲超时。
- **G-05.14** 服务必须支持受控的优雅关闭。

### G-06 用量与交互审计

- **G-06.01** 完成记账流程的请求必须生成结构化用量记录。
- **G-06.02** 用量记录必须区分 provider、model、operation 和 TransportPlan。
- **G-06.03** 用量记录必须区分 HTTP 状态和业务 outcome。
- **G-06.04** 上游提供 usage 时优先使用精确 token 数据。
- **G-06.05** 上游缺少 usage 时允许进行轻量 token 估算。
- **G-06.06** 估算结果必须显式标记为 estimated。
- **G-06.07** OpenAI 与 Anthropic 的 cache token 字段必须归一到统一统计字段。
- **G-06.08** CSV 写入必须保证单进程并发安全。
- **G-06.09** 每个进入归档流程的请求必须分配独立 round。
- **G-06.10** 归档必须包含脱敏后的客户端请求元信息。
- **G-06.11** 归档必须包含脱敏后的上游请求和响应摘要。
- **G-06.12** 归档必须包含最终 metadata。
- **G-06.13** 完整正文归档必须可以关闭。
- **G-06.14** 归档保留策略不能删除正在处理的 round。
- **G-06.15** 请求指纹与 stable-prefix drift 必须可用于定位 prompt 漂移。

### G-07 可观测性

- **G-07.01** 服务提供 Prometheus-compatible 指标接口。
- **G-07.02** 服务提供 JSON 聚合快照接口。
- **G-07.03** 服务提供持续推送聚合快照的 SSE 接口。
- **G-07.04** 观测接口默认只允许 loopback 访问。
- **G-07.05** 观测接口可显式开放远程访问。
- **G-07.06** 远程观测访问可限制到指定 IP 或 CIDR。
- **G-07.07** 指标必须覆盖请求量、时延、token、cache 和上游错误。
- **G-07.08** 请求指标必须包含 outcome 和 TransportPlan 维度。
- **G-07.09** 指标标签基数和时延样本内存必须有界。
- **G-07.10** 调试日志必须支持 JSON 与人类可读 text 两种格式。

### G-08 SLO 与状态变化通知

- **G-08.01** SLO 可按 provider 检查 cache 命中率下限。
- **G-08.02** SLO 可按 provider 检查上游错误率上限。
- **G-08.03** SLO 可按 provider 检查上游 attempt p99 延迟上限。
- **G-08.04** SLO 规则必须在达到最小样本数后才参与判定。
- **G-08.05** SLO 巡检周期必须可配置和关闭。
- **G-08.06** SLO 进入违规状态时产生 entered 事件。
- **G-08.07** SLO 恢复时产生 resolved 事件。
- **G-08.08** 持续违规不能在每次巡检时重复通知。
- **G-08.09** 可选 webhook 必须异步投递状态变化。
- **G-08.10** webhook 投递必须有有界队列、超时、重试、幂等和顺序语义。
- **G-08.11** webhook 关闭过程必须中断在途请求并统计丢弃任务。

### G-09 配置、运维与质量门禁

- **G-09.01** 配置文件必须覆盖 server、provider、model catalog、metrics 和 SLO。
- **G-09.02** server 配置必须支持环境变量覆盖。
- **G-09.03** 配置值必须支持 `${ENV}` 展开。
- **G-09.04** provider 只能在配置文件中声明，不能由环境变量动态创建。
- **G-09.05** 配置错误必须在监听端口前 fail-fast。
- **G-09.06** 产品提供独立 provider live probe。
- **G-09.07** probe 只验证配置中声明的直连 capability。
- **G-09.08** 构建流程必须生成可独立运行的主程序二进制。
- **G-09.09** 自动化测试必须覆盖所有核心产品合同。
- **G-09.10** 格式化、静态检查和测试必须能通过统一质量门禁执行。

## 2. Non-Goals

- **NG-01** 不提供多用户账号体系。
- **NG-02** 不提供团队权限管理。
- **NG-03** 不提供计费账单。
- **NG-04** 不提供团队成本分摊。
- **NG-05** 不提供跨实例共享的本地 CSV 写入协调。
- **NG-06** 不提供长期指标存储。
- **NG-07** 不提供完整 OpenTelemetry 分布式追踪。
- **NG-08** 不承诺兼容所有 provider 私有扩展字段。
- **NG-09** 不在协议转换中支持 tools 或 function calling。
- **NG-10** 不在协议转换中支持多模态内容。
- **NG-11** 不在协议转换中支持 `response_format`。

## 3. Definition of Done

### D-01 HTTP 接口与模型发现

- [x] **D-01.01** `GET /healthz` 返回 HTTP 200。
- [x] **D-01.02** `/healthz` 返回 JSON `{"status":"ok"}`。
- [x] **D-01.03** `/healthz` 在配置入站 API Key 后仍可匿名访问。
- [x] **D-01.04** `/healthz` 响应包含透传或新生成的 `X-Request-ID`。
- [x] **D-01.05** `POST /v1/chat/completions` 被识别为受支持入站接口。
- [x] **D-01.06** `POST /v1/messages` 被识别为受支持入站接口。
- [x] **D-01.07** `POST /v1/responses` 被识别为受支持入站接口。
- [x] **D-01.08** `POST /v1/completions` 被识别为受支持入站接口。
- [x] **D-01.09** `POST /v1/embeddings` 被识别为受支持入站接口。
- [x] **D-01.10** `GET /v1/models` 被识别为受支持入站接口。
- [x] **D-01.11** `POST /v1/models` 被识别为受支持入站接口。
- [x] **D-01.12** 其他 `/v1/*` 路径返回 HTTP 404。
- [x] **D-01.13** 执行端点只接受 POST。
- [x] **D-01.14** `/v1/models` 的 GET 与 POST 返回一致的模型列表。
- [x] **D-01.15** `/v1/models` 只返回 `model_catalog` 中的模型。
- [x] **D-01.16** `/v1/models` 按 exact 模型 ID 稳定排序。
- [x] **D-01.17** `/v1/models` 为每个模型返回非空 `operations`。
- [x] **D-01.18** `/v1/models` 在配置时返回 `contextWindowTokens`。
- [x] **D-01.19** `/v1/models` 在配置时返回 `maxOutputTokens`。
- [x] **D-01.20** `/v1/models` 不返回 RouteOwner。
- [x] **D-01.21** `/v1/models` 不返回 provider base URL。
- [x] **D-01.22** `/v1/models` 不返回 provider API Key。
- [x] **D-01.23** `POST /v1/models` 的请求体受入站大小限制。
- [x] **D-01.24** `/v1/models` 不向任何上游发起请求。

### D-02 配置加载与启动校验

- [x] **D-02.01** 未指定配置路径时优先读取工作目录的 `config.yaml`。
- [x] **D-02.02** `-config` 可以指定配置文件。
- [x] **D-02.03** `AI_PROXY_CONFIG` 可以指定配置文件。
- [x] **D-02.04** 配置值支持 `${ENV}` 展开。
- [x] **D-02.05** 未知顶层 section 导致配置加载失败。
- [x] **D-02.06** 未知配置 key 导致配置加载失败。
- [x] **D-02.07** 至少存在一个 provider 才能启动。
- [x] **D-02.08** 至少存在一个 enabled provider 才能启动。
- [x] **D-02.09** provider 名称按大小写折叠后必须唯一。
- [x] **D-02.10** enabled provider 必须显式配置 `protocol`。
- [x] **D-02.11** provider protocol 只允许 `openai` 或 `anthropic`。
- [x] **D-02.12** enabled provider 必须显式配置 `base_url`。
- [x] **D-02.13** provider base URL 只允许 HTTP 或 HTTPS。
- [x] **D-02.14** enabled provider 必须显式配置非空 `models`。
- [x] **D-02.15** enabled provider 必须显式配置非空 `endpoint_capabilities`。
- [x] **D-02.16** OpenAI provider 不允许声明 `messages` 直连能力。
- [x] **D-02.17** Anthropic provider 不允许声明 OpenAI 直连能力。
- [x] **D-02.18** 远程 provider 缺少 API Key 时启动失败。
- [x] **D-02.19** loopback provider 只有在 `allow_unauthenticated=true` 时允许空 API Key。
- [x] **D-02.20** `allow_unauthenticated=true` 与非空 API Key 不能同时使用。
- [x] **D-02.21** `allow_unauthenticated=true` 不能用于远程 provider。
- [x] **D-02.22** 非 loopback `listen_addr` 缺少入站 API Key 时启动失败。
- [x] **D-02.23** `model_catalog` 的 exact ID 必须唯一。
- [x] **D-02.24** 大小写不同的模型 ID 允许同时存在。
- [x] **D-02.25** 模型 ID 长度不能超过 256 字符。
- [x] **D-02.26** 模型 ID 不能包含控制字符。
- [x] **D-02.27** 每个 catalog 模型必须配置正数 `context_window_tokens`。
- [x] **D-02.28** 每个 catalog 模型必须配置正数 `max_output_tokens`。
- [x] **D-02.29** `max_output_tokens` 必须小于 `context_window_tokens`。
- [x] **D-02.30** 每个 catalog 模型必须配置至少一个 operation。
- [x] **D-02.31** operation 只允许 `chat_completions` 和 `embeddings`。
- [x] **D-02.32** operation 在加载时完成去重和稳定排序。
- [x] **D-02.33** endpoint capability 在加载时完成去重和稳定排序。
- [x] **D-02.34** catalog 模型未匹配 enabled provider 时启动失败。
- [x] **D-02.35** catalog 模型匹配多个 enabled provider 时启动失败。
- [x] **D-02.36** catalog operation 无法由 RouteOwner 服务时启动失败。
- [x] **D-02.37** 配置出现 `default_provider` 时启动失败。
- [x] **D-02.38** 配置出现 `fallbacks` 时启动失败。
- [x] **D-02.39** provider 不能通过环境变量动态创建。
- [x] **D-02.40** `AI_PROXY_PORT` 生成 `127.0.0.1:<port>`。
- [x] **D-02.41** `AI_PROXY_LISTEN_ADDR` 可以覆盖监听地址。
- [x] **D-02.42** `AI_PROXY_INBOUND_API_KEY` 可以覆盖入站 API Key。
- [x] **D-02.43** `AI_PROXY_MAX_REQUEST_BODY_BYTES` 可以覆盖请求体上限。
- [x] **D-02.44** `AI_PROXY_MAX_UPSTREAM_RESPONSE_BYTES` 可以覆盖上游响应上限。
- [x] **D-02.45** `AI_PROXY_MAX_STREAM_BYTES` 可以覆盖 SSE 累计输出上限。
- [x] **D-02.46** `AI_PROXY_MAX_SSE_LINE_BYTES` 可以覆盖单条 SSE 行上限。
- [x] **D-02.47** `AI_PROXY_ARCHIVE_FULL_CONTENT` 可以覆盖完整正文归档开关。
- [x] **D-02.48** `AI_PROXY_USAGE_FILE` 可以覆盖 CSV 路径。
- [x] **D-02.49** `AI_PROXY_INTERACTION_DIR` 可以覆盖归档目录。
- [x] **D-02.50** `AI_PROXY_INTERACTION_RETENTION` 可以覆盖归档保留轮数。
- [x] **D-02.51** `AI_PROXY_DEBUG_LOG` 可以覆盖调试日志开关。
- [x] **D-02.52** `AI_PROXY_LOG_FORMAT` 或 `LOG_FORMAT` 可以覆盖日志格式。
- [x] **D-02.53** `AI_PROXY_REQUEST_TIMEOUT_SECONDS` 可以覆盖请求超时。
- [x] **D-02.54** `AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS` 可以覆盖流空闲超时。
- [x] **D-02.55** `AI_PROXY_METRICS_REMOTE_ACCESS` 可以覆盖远程观测访问开关。
- [x] **D-02.56** `AI_PROXY_METRICS_ALLOWED_CIDRS` 可以覆盖观测访问白名单。
- [x] **D-02.57** `AI_PROXY_STREAM_IDLE_TIMEOUT_SECONDS=0` 可以关闭流空闲超时。
- [x] **D-02.58** 非法布尔值环境变量导致加载失败。
- [x] **D-02.59** 非法数字环境变量导致加载失败。
- [x] **D-02.60** 非法 CIDR 导致加载失败。
- [x] **D-02.61** 非 HTTP/HTTPS 的 SLO webhook URL 导致加载失败。

### D-03 路由与 TransportPlan

- [x] **D-03.01** 请求从 JSON body 的 `model` 字段读取模型 ID。
- [x] **D-03.02** 空模型返回 `model_required`。
- [x] **D-03.03** catalog 中不存在的模型返回 `model_not_found`。
- [x] **D-03.04** model lookup 严格区分大小写。
- [x] **D-03.05** disabled provider 不参与 catalog 模型匹配。
- [x] **D-03.06** disabled RouteOwner 在请求期返回 `provider_unavailable`。
- [x] **D-03.07** RouteOwner 缺失时返回 `route_contract_invalid`。
- [x] **D-03.08** 模型未声明目标 operation 时返回 `operation_unsupported`。
- [x] **D-03.09** RouteOwner 无法服务目标端点时返回 `endpoint_unsupported`。
- [x] **D-03.10** `X-AI-Provider` 不影响 RouteOwner。
- [x] **D-03.11** `?provider=` 不影响 RouteOwner。
- [x] **D-03.12** `provider/model` 形式不用于选择 RouteOwner。
- [x] **D-03.13** 网络错误后不切换 provider。
- [x] **D-03.14** 上游 408、429 或 5xx 后不切换 provider。
- [x] **D-03.15** 首个 SSE 事件读取失败后不切换 provider。
- [x] **D-03.16** TransportPlan 记录 model、operation 和 RouteOwner。
- [x] **D-03.17** TransportPlan 记录 client protocol 和 client endpoint。
- [x] **D-03.18** TransportPlan 记录 upstream protocol 和 upstream endpoint。
- [x] **D-03.19** TransportPlan 记录 conversion mode。
- [x] **D-03.20** 非法 JSON 请求返回 HTTP 400。
- [x] **D-03.21** 非法 JSON 请求返回 `invalid_request`。
- [x] **D-03.22** 无效运行期路由合同返回 HTTP 500 和 `route_contract_invalid`。
- [x] **D-03.23** 不可用 RouteOwner 返回 HTTP 503 和 `provider_unavailable`。
- [x] **D-03.24** 代理内部失败返回 HTTP 500 和 `proxy_internal_error`。
- [x] **D-03.25** 上游连接、读取或解码失败返回 HTTP 502 和 `upstream_unavailable`。
- [x] **D-03.26** OpenAI typed error envelope 包含稳定的 code 和 message。
- [x] **D-03.27** Anthropic typed error envelope 包含稳定的 type 和 message。

### D-04 原生转发与协议转换

- [x] **D-04.01** OpenAI Chat + OpenAI `chat_completions` 生成 native plan。
- [x] **D-04.02** OpenAI Chat + Anthropic `messages` 生成 `openai_to_anthropic` plan。
- [x] **D-04.03** Anthropic Messages + Anthropic `messages` 生成 native plan。
- [x] **D-04.04** Anthropic Messages + OpenAI `chat_completions` 生成 `anthropic_to_openai` plan。
- [x] **D-04.05** OpenAI Responses 只允许 OpenAI `responses` 原生转发。
- [x] **D-04.06** OpenAI Completions 只允许 OpenAI `completions` 原生转发。
- [x] **D-04.07** OpenAI Embeddings 只允许 OpenAI `embeddings` 原生转发。
- [x] **D-04.08** Embeddings-only 模型拒绝 chat、messages、responses 和 completions 请求。
- [x] **D-04.09** OpenAI → Anthropic 转换支持 system 文本。
- [x] **D-04.10** OpenAI → Anthropic 转换支持 user 文本。
- [x] **D-04.11** OpenAI → Anthropic 转换支持 assistant 文本。
- [x] **D-04.12** Anthropic → OpenAI 转换支持 system 文本。
- [x] **D-04.13** Anthropic → OpenAI 转换支持 user 文本。
- [x] **D-04.14** Anthropic → OpenAI 转换支持 assistant 文本。
- [x] **D-04.15** 双向转换支持 `max_tokens`。
- [x] **D-04.16** 双向转换支持 `temperature`。
- [x] **D-04.17** 双向转换支持 `top_p`。
- [x] **D-04.18** 双向转换支持 stop sequence 归一化。
- [x] **D-04.19** 非法 stop 配置在访问上游前返回 `conversion_unsupported`。
- [x] **D-04.20** 非空 tools 或 function calling 在访问上游前返回 `conversion_unsupported`。
- [x] **D-04.21** 非文本内容在访问上游前返回 `conversion_unsupported`。
- [x] **D-04.22** 不支持的 `response_format` 在访问上游前返回 `conversion_unsupported`。
- [x] **D-04.23** OpenAI → Anthropic 非流式响应转换为 OpenAI-compatible 响应。
- [x] **D-04.24** Anthropic → OpenAI 非流式响应转换为 Anthropic-compatible 响应。
- [x] **D-04.25** 转换路径的上游错误保留 HTTP 状态。
- [x] **D-04.26** 转换路径的上游错误正文经过客户端协议安全封装。
- [x] **D-04.27** OpenAI SDK 可调用 models、chat 和 embeddings 原生路径。
- [x] **D-04.28** Anthropic SDK 可调用 Messages 原生路径。
- [x] **D-04.29** OpenAI SDK 可消费 OpenAI → Anthropic 转换结果。
- [x] **D-04.30** Anthropic SDK 可消费 Anthropic → OpenAI 转换结果。
- [x] **D-04.31** base URL 已以 `/v1` 结尾时不会重复拼接 `/v1`。
- [x] **D-04.32** 带嵌套前缀并以 `/v1` 结尾的 base URL 保留其前缀。
- [x] **D-04.33** `provider` query 参数不会转发给上游。

### D-05 流式处理与 outcome

- [x] **D-05.01** SSE 响应逐行写入客户端。
- [x] **D-05.02** 每次写入 SSE 数据后执行 flush。
- [x] **D-05.03** Chat Completions 和 Completions 使用 `[DONE]` 识别成功终止。
- [x] **D-05.04** Anthropic Messages 使用 `message_stop` 识别成功终止。
- [x] **D-05.05** Responses 使用 `response.completed` 识别成功终止。
- [x] **D-05.06** Responses 使用 `response.failed` 识别上游显式失败。
- [x] **D-05.07** Responses 使用 `response.incomplete` 识别未完成结果。
- [x] **D-05.08** 需要终止事件的协议在 EOF 前未出现终止事件时标记 `upstream_truncated`。
- [x] **D-05.09** 转换流收到协议终止事件后即可结束，不等待上游连接 EOF。
- [x] **D-05.10** 客户端取消标记为 `client_canceled`。
- [x] **D-05.11** 流空闲超时标记为 `idle_timeout`。
- [x] **D-05.12** 流大小或行大小超限标记为 `limit_exceeded`。
- [x] **D-05.13** 上游显式失败标记为 `upstream_failed`。
- [x] **D-05.14** 上游未完成标记为 `incomplete`。
- [x] **D-05.15** 客户端写入失败标记为 `client_write`。
- [x] **D-05.16** 转换失败标记为 `conversion`。
- [x] **D-05.17** SSE 或 JSON 协议损坏标记为 `protocol`。
- [x] **D-05.18** 其他失败标记为 `error`。
- [x] **D-05.19** 正常结束标记为 `success`。
- [x] **D-05.20** `idle_timeout` 计入上游错误率。
- [x] **D-05.21** `upstream_truncated` 计入上游错误率。
- [x] **D-05.22** `upstream_failed` 计入上游错误率。
- [x] **D-05.23** 上游导致的 `protocol` 失败计入上游错误率。
- [x] **D-05.24** `client_canceled` 不计入上游错误率。
- [x] **D-05.25** `limit_exceeded` 不计入上游错误率。
- [x] **D-05.26** `client_write` 不计入上游错误率。
- [x] **D-05.27** `incomplete` 不计入上游错误率。
- [x] **D-05.28** 非流式错误不会与状态码统计重复计入上游错误率。
- [x] **D-05.29** 流式响应在提交成功状态前探测首条 SSE 数据或错误。

### D-06 认证、header 与资源限制

- [x] **D-06.01** 配置入站 API Key 后，所有 `/v1/*` 白名单接口都要求认证。
- [x] **D-06.02** `Authorization: Bearer <key>` 可通过入站认证。
- [x] **D-06.03** `X-API-Key: <key>` 可通过入站认证。
- [x] **D-06.04** 缺失或错误入站密钥返回 HTTP 401。
- [x] **D-06.05** 认证失败返回 `authentication_failed`。
- [x] **D-06.06** 入站 `Authorization` 不直接转发给上游。
- [x] **D-06.07** 入站 `X-API-Key` 不直接转发给上游。
- [x] **D-06.08** OpenAI 上游认证使用 provider 的 Bearer token。
- [x] **D-06.09** Anthropic 上游认证使用 provider 的 `X-API-Key`。
- [x] **D-06.10** Anthropic 上游版本固定为 `2023-06-01`。
- [x] **D-06.11** 上游请求只允许透传安全的 `Content-Type`。
- [x] **D-06.12** 上游请求只允许透传安全的 `Accept`。
- [x] **D-06.13** 上游请求只允许透传格式安全且不超过 128 字符的 `X-Request-ID`。
- [x] **D-06.14** 入站未提供 request ID 时生成 32 字符十六进制 ID。
- [x] **D-06.15** 响应返回与请求上下文一致的 `X-Request-ID`。
- [x] **D-06.16** 响应删除标准 hop-by-hop header。
- [x] **D-06.17** 响应删除 `Connection` 动态列出的扩展 header。
- [x] **D-06.18** 请求体默认上限为 32 MiB。
- [x] **D-06.19** 请求体超限返回 HTTP 413。
- [x] **D-06.20** 请求体超限返回 `request_too_large`。
- [x] **D-06.21** 上游非流式响应默认上限为 64 MiB。
- [x] **D-06.22** 上游非流式响应超限返回 HTTP 502。
- [x] **D-06.23** gzip 上游响应在记账和回写前完成解压。
- [x] **D-06.24** gzip 解压后的正文继续受上游响应大小限制。
- [x] **D-06.25** SSE 累计输出默认上限为 64 MiB。
- [x] **D-06.26** 单条 SSE 行默认上限为 1 MiB。
- [x] **D-06.27** 非流式请求使用可配置总超时。
- [x] **D-06.28** 流式请求等待上游响应或首事件时受超时保护。
- [x] **D-06.29** 流式读取使用可配置空闲超时。
- [x] **D-06.30** API error message 不包含 API Key 或 Authorization。
- [x] **D-06.31** 归档 header 对 Authorization、X-API-Key 和 Cookie 脱敏。
- [x] **D-06.32** webhook 错误日志不输出 URL 中的 secret path 或 query。
- [x] **D-06.33** probe 摘要检测到凭据特征时隐藏完整上游正文。

### D-07 用量 CSV

- [x] **D-07.01** 在线用量唯一主存储为 `usage_store.path` 指定的 DuckDB；`usage_file` 不再支持。
- [x] **D-07.02** CSV 首次写入时创建表头。
- [x] **D-07.03** CSV 记录 `time`。
- [x] **D-07.04** CSV 记录 `provider` 和 `model`。
- [x] **D-07.05** CSV 记录 `operation` 和 `client_endpoint`。
- [x] **D-07.06** CSV 记录 `upstream_protocol` 和 `upstream_endpoint`。
- [x] **D-07.07** CSV 记录 `conversion_mode`。
- [x] **D-07.08** CSV 记录输入、输出和总 token。
- [x] **D-07.09** CSV 记录 duration、stream 和 estimated。
- [x] **D-07.10** CSV 记录 HTTP status 和 outcome。
- [x] **D-07.11** CSV 记录 cached input token。
- [x] **D-07.12** CSV 记录 cache creation input token。
- [x] **D-07.13** CSV 记录 cache hit rate。
- [x] **D-07.14** OpenAI `prompt_tokens_details.cached_tokens` 被解析为 cached input token。
- [x] **D-07.15** OpenAI `input_tokens_details.cached_tokens` 被解析为 cached input token。
- [x] **D-07.16** OpenAI `input_tokens_details.cache_read_tokens` 被解析为 cached input token。
- [x] **D-07.17** OpenAI `input_tokens_details.cache_creation_tokens` 被解析为 cache creation input token。
- [x] **D-07.18** Anthropic `cache_read_input_tokens` 被解析为 cached input token。
- [x] **D-07.19** Anthropic `cache_creation_input_tokens` 被解析为 cache creation input token。
- [x] **D-07.20** 缺失 usage 的适用响应使用轻量估算。
- [x] **D-07.21** 估算 token 的记录设置 `estimated=true`。
- [x] **D-07.22** 无模型或 token 语义的请求使用空模型和 0 token。
- [x] **D-07.23** 同一进程内并发追加不会交叉写半行。
- [x] **D-07.24** 同一进程内并发追加不会写重复表头。
- [x] **D-07.25** 旧 CSV schema 自动轮转为带时间和 PID 的备份。
- [x] **D-07.26** CSV 备份轮转不会覆盖已存在备份。
- [x] **D-07.27** 文档明确禁止多个进程共享同一 usage 文件。
- [x] **D-07.28** 非流式响应优先解析上游返回的 usage。
- [x] **D-07.29** 流式响应优先解析 SSE 事件中的 usage。
- [x] **D-07.30** Responses 流从完成事件中解析 usage。
- [x] **D-07.31** cache hit rate 按 cached input token 除以 input token 计算。

### D-08 交互归档与日志

- [x] **D-08.01** `interaction_dir` 默认值为 `interactions`。
- [x] **D-08.02** `interaction_retention` 默认值为 500。
- [x] **D-08.03** round ID 在单进程内递增且并发安全。
- [x] **D-08.04** round 目录使用六位补零数字名称。
- [x] **D-08.05** 新建的 round 目录使用 `0700` 权限。
- [x] **D-08.06** 新建的归档内容文件使用 `0600` 权限。
- [x] **D-08.07** `request.meta.json` 记录脱敏客户端请求摘要。
- [x] **D-08.08** `upstream_request.json` 记录脱敏上游请求摘要。
- [x] **D-08.09** `upstream_response.json` 记录上游状态和耗时摘要。
- [x] **D-08.10** 非流式响应按内容类型保存为 JSON、文本或二进制文件。
- [x] **D-08.11** 流式原始数据增量保存为 `response.sse`。
- [x] **D-08.12** 支持归并的成功流在未截断时生成完整 `response.json`。
- [x] **D-08.13** `metadata.json` 记录 round ID 和 request ID。
- [x] **D-08.14** `metadata.json` 记录 provider 和 model。
- [x] **D-08.15** `metadata.json` 记录完整 TransportPlan 字段。
- [x] **D-08.16** `metadata.json` 记录 HTTP status、outcome 和 duration。
- [x] **D-08.17** `metadata.json` 记录 token、cache 和 estimated。
- [x] **D-08.18** `metadata.json` 只引用实际成功写入的文件路径。
- [x] **D-08.19** `metadata.json` 记录最终错误信息。
- [x] **D-08.20** `archive_full_content=false` 时不写请求和响应完整正文。
- [x] **D-08.21** `archive_full_content=false` 时仍写元数据文件。
- [x] **D-08.22** active round 不参与 retention 删除。
- [x] **D-08.23** metadata 写入失败仍释放 active round。
- [x] **D-08.24** 中途 abort 仍释放 active round。
- [x] **D-08.25** retention 只保留最新的已完成数字 round 目录。
- [x] **D-08.26** 请求指纹对相同有效输入保持稳定。
- [x] **D-08.27** stable-prefix hash 只使用 system 与 messages 的前 256 字节。
- [x] **D-08.28** full request fingerprint 使用完整请求体计算。
- [x] **D-08.29** 非 JSON 请求仍可生成确定性 fingerprint。
- [x] **D-08.30** stable-prefix 连续两次变化后记录 drift。
- [x] **D-08.31** drift 计数在再次出现相同 hash 时重置。
- [x] **D-08.32** 控制台摘要包含 round、provider、model、status、duration 和 token。
- [x] **D-08.33** provider 异常日志使用 WARN 或 ERROR 级别突出显示。
- [x] **D-08.34** JSON 日志不包含 ANSI 颜色转义。
- [x] **D-08.35** text 日志只对 level token 应用颜色。
- [x] **D-08.36** 完整正文关闭时 metadata 不引用未写入的正文文件。

### D-09 Metrics 与 Stats

- [x] **D-09.01** `GET /metrics` 返回 Prometheus text exposition format。
- [x] **D-09.02** `HEAD /metrics` 返回状态和 header，不返回正文。
- [x] **D-09.03** `/metrics` 的其他方法返回 HTTP 405。
- [x] **D-09.04** `GET /stats` 返回 JSON 快照。
- [x] **D-09.05** `HEAD /stats` 返回状态和 header，不返回正文。
- [x] **D-09.06** `/stats` 的其他方法返回 HTTP 405。
- [x] **D-09.07** `GET /stats/stream` 返回 SSE。
- [x] **D-09.08** `/stats/stream` 建立连接后立即推送首个快照。
- [x] **D-09.09** `/stats/stream` 默认每秒推送一次快照。
- [x] **D-09.10** `/stats/stream` 在客户端断开后停止 ticker 并返回。
- [x] **D-09.11** `/stats/stream` 的非 GET 方法返回 HTTP 405。
- [x] **D-09.12** 观测接口默认拒绝非 loopback 请求。
- [x] **D-09.13** `metrics_remote_access=true` 可以允许远程请求。
- [x] **D-09.14** 远程访问开启且 CIDR 列表非空时只允许白名单来源。
- [x] **D-09.15** loopback 来源始终允许访问观测接口。
- [x] **D-09.16** `/metrics` 暴露请求计数。
- [x] **D-09.17** `/metrics` 暴露请求时延 sum 和 count。
- [x] **D-09.18** `/metrics` 暴露输入和输出 token 计数。
- [x] **D-09.19** `/metrics` 暴露 cached input 和 cache creation token 计数。
- [x] **D-09.20** `/metrics` 暴露 cache hit rate。
- [x] **D-09.21** `/metrics` 暴露上游错误计数。
- [x] **D-09.22** 请求指标包含 provider、model、route、status 和 outcome 标签。
- [x] **D-09.23** 请求指标包含 client endpoint、upstream protocol、upstream endpoint 和 conversion mode 标签。
- [x] **D-09.24** 每个 provider 最多保留 64 个独立 model label。
- [x] **D-09.25** 超出 model label 上限的模型聚合为 `_other`。
- [x] **D-09.26** catalog 模型和精确 provider model 在启动时优先预占 label 槽位。
- [x] **D-09.27** 每个 provider/model 的完成请求延迟样本最多保留 2048 个。
- [x] **D-09.28** 每个 provider 的上游 attempt 延迟样本最多保留 2048 个。
- [x] **D-09.29** `/stats` 返回 uptime。
- [x] **D-09.30** `/stats` 返回按 provider、status 和 outcome 聚合的请求量。
- [x] **D-09.31** `/stats` 返回按 provider 聚合的 cache hit、miss、hit rate 和平均 cached token。
- [x] **D-09.32** `/stats` 返回 provider/model 的 p50、p75、p90、p95 和 p99 延迟。
- [x] **D-09.33** `/stats` 返回上游 5xx、408、429 和具体状态码分布。

### D-10 SLO 与 webhook

- [x] **D-10.01** cache hit rate 阈值只接受 `[0,1]`。
- [x] **D-10.02** upstream error rate 阈值只接受 `[0,1]`。
- [x] **D-10.03** p99 latency 阈值不能为负数。
- [x] **D-10.04** check interval 不能为负数。
- [x] **D-10.05** 阈值为 0 时对应规则关闭。
- [x] **D-10.06** check interval 为 0 时后台周期巡检关闭。
- [x] **D-10.07** cache hit rate 样本少于 10 时不判定违规。
- [x] **D-10.08** upstream error rate attempt 少于 10 时不判定违规。
- [x] **D-10.09** p99 latency 样本少于 10 时不判定违规。
- [x] **D-10.10** p99 SLO 使用上游 attempt 延迟而不是完整请求耗时。
- [x] **D-10.11** upstream error rate 使用上游 attempt 数作为分母。
- [x] **D-10.12** 新违规只产生一次 entered 状态变化。
- [x] **D-10.13** 持续违规不重复产生 entered 状态变化。
- [x] **D-10.14** 违规恢复时产生一次 resolved 状态变化。
- [x] **D-10.15** 本地 listener 对 entered 输出 WARN。
- [x] **D-10.16** 本地 listener 对 resolved 输出 INFO。
- [x] **D-10.17** violation 历史最多保留 256 条。
- [x] **D-10.18** 并发 `CheckNow` 串行执行。
- [x] **D-10.19** webhook payload 包含 `instance_id`。
- [x] **D-10.20** webhook payload 包含实例内递增 `seq`。
- [x] **D-10.21** 每个 violation 包含 `generation`。
- [x] **D-10.22** 每个状态变化包含稳定 `event_id`。
- [x] **D-10.23** 同一事件重试时 `event_id` 保持不变。
- [x] **D-10.24** evaluator 重启后生成新的 `instance_id`。
- [x] **D-10.25** webhook 队列容量为 64。
- [x] **D-10.26** webhook 使用单 worker 保持投递顺序。
- [x] **D-10.27** webhook 单次请求超时为 3 秒。
- [x] **D-10.28** 网络错误可进入重试流程。
- [x] **D-10.29** HTTP 408、425、429 和 5xx 可进入重试流程。
- [x] **D-10.30** webhook 最多执行 3 次 attempt。
- [x] **D-10.31** 429 优先解析秒数形式的 `Retry-After`。
- [x] **D-10.32** 429 支持 HTTP-date 形式的 `Retry-After`。
- [x] **D-10.33** `Retry-After` 最大等待值限制为 30 秒。
- [x] **D-10.34** 其他 4xx 视为永久失败。
- [x] **D-10.35** webhook 客户端禁止自动重定向。
- [x] **D-10.36** 可重试失败存入有界 undelivered 队列。
- [x] **D-10.37** undelivered 队列最多保留 32 批。
- [x] **D-10.38** 下一轮 `CheckNow` 重新入队到期的 undelivered 任务。
- [x] **D-10.39** 重投前按当前 active 状态和 generation 丢弃过期事件。
- [x] **D-10.40** 重投重新分配 seq，但保留每条事件的 event ID。
- [x] **D-10.41** webhook 队列满时丢弃新任务并增加 dropped 计数。
- [x] **D-10.42** Close 取消在途 webhook HTTP 请求。
- [x] **D-10.43** Close 将剩余队列和 undelivered 计入 dropped。
- [x] **D-10.44** Close 后 webhook queue length 归零。
- [x] **D-10.45** `/metrics` 暴露 webhook dropped 总数。
- [x] **D-10.46** `/metrics` 暴露 webhook queue length。
- [x] **D-10.47** `/metrics` 按 ok、error、non_2xx、canceled 暴露 webhook attempt 计数。

### D-11 Probe、关闭与质量门禁

- [x] **D-11.01** 主程序支持从命令行指定配置文件。
- [x] **D-11.02** 主程序收到 SIGINT 后启动优雅关闭。
- [x] **D-11.03** 主程序收到 SIGTERM 后启动优雅关闭。
- [x] **D-11.04** HTTP server 优雅关闭超时为 10 秒。
- [x] **D-11.05** 关闭过程先停止 SLO 巡检。
- [x] **D-11.06** 关闭过程等待 SLO Run goroutine 退出。
- [x] **D-11.07** `ai-proxy-probe` 与主服务使用独立入口。
- [x] **D-11.08** probe 要求显式 provider、capability 和 exact model。
- [x] **D-11.09** probe 拒绝缺失或 disabled provider。
- [x] **D-11.10** probe 拒绝 provider 未声明的直连 capability。
- [x] **D-11.11** probe 拒绝不在 catalog 中的模型。
- [x] **D-11.12** probe 拒绝 RouteOwner 与指定 provider 不一致的模型。
- [x] **D-11.13** probe 支持 chat_completions 最小请求。
- [x] **D-11.14** probe 支持 messages 最小请求。
- [x] **D-11.15** probe 支持 responses 最小请求。
- [x] **D-11.16** probe 支持 completions 最小请求。
- [x] **D-11.17** probe 支持 embeddings 最小请求。
- [x] **D-11.18** probe 可选执行流式探测。
- [x] **D-11.19** probe 输出 provider、protocol、capability、model、path、status、duration 和 conclusion。
- [x] **D-11.20** probe 区分 `success`。
- [x] **D-11.21** probe 区分 `credential_issue`。
- [x] **D-11.22** probe 区分 `capability_drift`。
- [x] **D-11.23** probe 区分 `environment_undetermined`。
- [x] **D-11.24** probe 区分通用 `error`。
- [x] **D-11.25** `make build` 可以生成单个主程序二进制。
- [x] **D-11.26** `make test` 执行全部 Go 测试。
- [x] **D-11.27** `make fmt` 执行 Go 格式化。
- [x] **D-11.28** `make vet` 执行 Go 静态检查。
- [x] **D-11.29** `make check` 依次执行 fmt、vet 和 test。
- [x] **D-11.30** 自动化测试覆盖配置加载与失败校验。
- [x] **D-11.31** 自动化测试覆盖路由矩阵与禁止 fallback。
- [x] **D-11.32** 自动化测试覆盖 OpenAI 和 Anthropic SDK 接入。
- [x] **D-11.33** 自动化测试覆盖非流式与流式协议转换。
- [x] **D-11.34** 自动化测试覆盖用量、cache 与 CSV schema 轮转。
- [x] **D-11.35** 自动化测试覆盖归档生命周期与敏感信息保护。
- [x] **D-11.36** 自动化测试覆盖 metrics、stats 和 label 基数限制。
- [x] **D-11.37** 自动化测试覆盖 SLO 状态变化、重试、幂等、关闭和指标。
- [x] **D-11.38** 自动化测试覆盖 probe 请求与结论合同。

## 4. Goal 与 DoD 对应关系

| Goal | 主要 DoD |
| --- | --- |
| G-01 统一客户端接入 | D-01、D-11 |
| G-02 确定性模型路由 | D-02、D-03 |
| G-03 标准端点与协议适配 | D-01、D-04、D-05 |
| G-04 稳定错误与流式结果 | D-03、D-04、D-05、D-06 |
| G-05 安全与资源边界 | D-02、D-06、D-08、D-11 |
| G-06 用量与交互审计 | D-07、D-08 |
| G-07 可观测性 | D-09 |
| G-08 SLO 与状态变化通知 | D-10 |
| G-09 配置、运维与质量门禁 | D-02、D-11 |

## 5. 实施规则

- 每个 DoD 至少应由一个自动化测试、可重复命令或明确人工验收步骤覆盖。
- 开发提交应列出本次完成或影响的 Goal ID 与 DoD ID。
- 修改外部接口、错误码、CSV schema、metadata schema、指标标签或 webhook payload 时，必须同步更新对应 DoD。
- 修改路由或转换能力时，必须同时覆盖启动期配置校验和请求期 TransportPlan 测试。
- 修改流式处理时，必须同时覆盖正常终止、客户端取消、上游截断、协议失败和资源上限。
- 修改归档、metrics 或 SLO 时，必须证明内存、队列、样本或文件保留范围仍然有界。
- 所有 DoD 通过前，不得将对应 Goal 标记为完成。

## G-USAGE client_api_keys + DuckDB（2026-07-17 收口）

> 本段增量补充，不删除既有 Goal/DoD。完整合同见 `docs/api-key-usage-duckdb-web-closure-plan-2026-07-17.md`。

### Goals

- **G-USAGE.01** 客户端 API Key 用于调用方识别与用量归属；唯一配置 authority 为 `client_api_keys`。
- **G-USAGE.02** 每个数据请求必须携带已启用 Key；缺失/未知/禁用/格式错误/冲突 Key 返回 401 且不计 usage。
- **G-USAGE.03** DuckDB `usage_events` 是唯一在线用量持久化 authority；CSV 仅导出与一次性导入。
- **G-USAGE.04** 调用在访问上游前持久化 `started`，所有退出路径尝试 `completed`。
- **G-USAGE.05** Web 管理端提供使用统计页签（Dashboard/趋势/Key 汇总/明细分页/CSV 导出），loopback-only。

### Non-Goals

- 不建设账号/账单/额度系统。
- 不支持多实例共享同一 DuckDB 文件。
- 不在启动时自动导入旧 `usage.csv`。
- `client_api_keys` 不是强制登录体系；非 loopback 访问需独立网络保护。

### DoD

- [x] **D-USAGE.01** 配置拒绝 `inbound_api_key` / `usage_file` / `AI_PROXY_INBOUND_API_KEY` / `AI_PROXY_USAGE_FILE`。
- [x] **D-USAGE.02** `clientauth` 解析 OpenAI Bearer 与 Anthropic X-API-Key；原始 Key 不落盘。
- [x] **D-USAGE.03** `internal/usage` DuckDB Store：migration、Start/Complete、RecoverInterrupted、Dashboard、Events、ExportCSV。
- [x] **D-USAGE.04** 代理路径接线 Start/Complete；401 不计 usage；Start 失败 503。
- [x] **D-USAGE.05** `/admin/api/usage/*` 与 Web `#/usage` 页签可用。
- [x] **D-USAGE.06** `cmd/ai-proxy-usage-import` 提供旧 CSV 一次性导入。
- [x] **D-USAGE.07** `go test ./...` / `go vet` / `gofmt` 门禁通过。

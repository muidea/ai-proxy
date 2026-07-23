# 统一文本流式 SSE 收口设计

状态：Accepted / Implemented  
日期：2026-07-23

## 1. 决策

`ai-proxy` 统一使用 Server-Sent Events（SSE）承载文本生成的增量输出，不在本阶段提供 WebSocket 或 OpenAI Realtime 代理。

“统一”指传输与生命周期合同统一，不指把 OpenAI 和 Anthropic 的事件 JSON 合并为代理私有格式：

- 客户端调用 `/v1/chat/completions`，始终接收 OpenAI Chat Completions SSE 事件；路由到 Anthropic 时由代理转换。
- 客户端调用 `/v1/messages`，始终接收 Anthropic Messages SSE 事件；路由到 OpenAI 时由代理转换。
- 客户端调用 `/v1/responses`，接收 OpenAI Responses SSE 事件；当前仅支持具备 `responses` direct capability 的 OpenAI 协议 Provider。
- `stream: false` 或省略 `stream` 时返回普通 JSON。

## 2. 范围

本阶段覆盖：

- OpenAI Chat Completions 原生 SSE；
- Anthropic Messages 原生 SSE；
- OpenAI Chat Completions → Anthropic Messages 的基础文本请求及 SSE 转换；
- Anthropic Messages → OpenAI Chat Completions 的基础文本请求及 SSE 转换；
- OpenAI Responses 原生 SSE；
- 统一认证、模型路由、限制、用量、归档和指标生命周期。

本阶段不覆盖：

- WebSocket、WebRTC、SIP 或 `/v1/realtime`；
- 双向音频、VAD、客户端中途发送会话事件和实时打断；
- 代理私有的统一事件 JSON；
- 将 Anthropic Messages 转换为 OpenAI Responses；
- 跨协议转换中的 tools/function calling、thinking、多模态或其它未声明能力。

上述非文本或双向能力需要独立 operation、能力合同和设计，不能作为 SSE 基础文本能力隐式派生。

## 3. 端点矩阵

| 客户端端点 | 下游 SSE 格式 | OpenAI Provider | Anthropic Provider |
| --- | --- | --- | --- |
| `/v1/chat/completions` | OpenAI Chat Completions | native | 基础文本转换 |
| `/v1/messages` | Anthropic Messages | 基础文本转换 | native |
| `/v1/responses` | OpenAI Responses | native | 不支持 |
| `/v1/completions` | OpenAI Completions | native | 不支持 |
| `/v1/embeddings` | 不适用 | 非流式 native | 不支持 |

Provider 的 `endpoint_capabilities` 继续描述上游直接端点能力；`model_catalog.operations` 继续描述模型业务能力。SSE 是请求的传输模式，不新增 `sse` operation，也不从 protocol 自动推断 direct endpoint。

## 4. 请求合同

标准推理端点只以 JSON 请求体中的 `stream: true` 开启 SSE：

```http
POST /v1/chat/completions HTTP/1.1
Authorization: Bearer <client-api-key>
Content-Type: application/json
Accept: text/event-stream

{"model":"gpt-test","stream":true,"messages":[...]}
```

`Accept: text/event-stream` 可以声明客户端可接收 SSE，但不能单独把非流式请求变成长连接。省略 `stream` 或设置为 `false` 时，即使存在该 Accept 值，也按普通 JSON 请求处理。

## 5. 响应合同

所有模型 SSE 响应统一设置：

```http
Content-Type: text/event-stream
Cache-Control: no-cache, no-transform
X-Accel-Buffering: no
```

代理不向下游写入 `Connection`、`Keep-Alive`、`Transfer-Encoding` 等 hop-by-hop header，也不继承上游 `Content-Length`。每个完整 SSE 行写出后立即 `Flush`，不等待完整响应结束。

终止事件保持客户端协议语义：

- Chat Completions：`data: [DONE]`；
- Anthropic Messages：`message_stop`；
- Responses：`response.completed`、`response.failed`、`response.cancelled` 或相应终态事件。

代理收到终止事件后立即结束本次下游流，不依赖上游主动关闭 TCP 连接。

## 6. 转换边界

跨协议转换只保证基础文本。请求进入上游前执行能力校验；tools/function calling、多模态、`response_format`、非文本 content block 等未支持特性返回 `conversion_unsupported`，不能静默删除字段。

流开始后若上游产生无法转换的事件，代理终止流，并将结果记录为 `conversion` 或 `protocol`。HTTP 响应头已经提交后，不再尝试向 SSE 流中混入另一种协议的 JSON error envelope。

## 7. 生命周期与安全

- 客户端仍通过 `Authorization: Bearer` 或 `X-API-Key` 完成应用层认证；客户端凭据不转发给上游。
- 请求只按 exact `model` 解析唯一 RouteOwner，SSE 不改变路由策略，也不触发流中 fallback。
- 首个 SSE 行在提交成功状态前探测；首事件失败作为上游错误返回。
- 客户端断开时取消上游请求。
- `stream_idle_timeout_seconds` 限制连续无事件时长。
- `max_stream_bytes` 和 `max_sse_line_bytes` 分别限制累计流量与单条 SSE 行。
- 原始事件增量归档为 `response.sse`；可完整重建时另存 `response.json`，音频/二进制不在本阶段范围内。

## 8. 浏览器调用

模型端点使用 POST 和认证 header，浏览器应使用 `fetch()` 并读取 `Response.body` 的 `ReadableStream`。原生 `EventSource` 只适合 GET 且不便设置 `Authorization`，不是本合同的客户端入口。

生产部署的反向代理还应关闭该路径的响应缓冲；代理返回的 `X-Accel-Buffering: no` 为常见 Nginx 部署提供提示，但不能替代部署侧配置。

## 9. 验收标准

- OpenAI 与 Anthropic native SSE 均使用统一响应 header，并逐事件 flush；
- 两个方向的基础文本转换返回各自客户端端点的标准终止事件；
- `Accept: text/event-stream` 且未设置 `stream: true` 时返回 JSON，不进入 SSE 首事件探测和 idle timeout；
- `/v1/responses` 只走 OpenAI direct capability，不通过 Anthropic 转换派生；
- 流大小、单行大小、空闲超时、客户端取消、终止事件和用量归档测试通过；
- 不新增 WebSocket 路由、Hijacker 依赖或 WS 配置项。


# Provider Capability Audit

Date: 2026-07-15

Status: active — 收口配置已部署并验证；`krill-ai.completions` 仍为 520，等待最终能力结论

Scope: `/home/rangh/aispace/ai-proxy`。WorkOrch 后续按 ai-proxy 合同单独同步，不在本审计中伪造跨仓联调结论。

## 1. 审计规则

- model ID 严格区分大小写并按 exact ID 路由：`DeepSeek-V4-Flash` 与 `deepseek-v4-flash` 是两个模型。
- `endpoint_capabilities` 只表示 provider 上游 direct endpoint；协议转换能力不反写到该字段。
- 每个 enabled provider 的每个已声明 direct capability 分别以最小 non-stream 请求验证；成功的
  chat/responses/completions 再单独验证 streaming。
- 2xx=`success`；401/403=`credential_issue`；404/405 或上游明确的“not implemented / unsupported /
  current base URL does not support”=`capability_drift`；timeout、408、429、520 或无语义的 5xx
  =`environment_undetermined`。
- SDK mock 验收证明 ai-proxy 客户端合同可解析，不能替代真实 provider capability live。
- probe 输出和本文不记录 API Key、Authorization 或完整敏感上游 body。

## 2. 验证任务与环境

| Task ID | 范围 | 状态 | 结论 |
| --- | --- | --- | --- |
| PC-CODE-20260715-01 | config/proxy/probe 单测、全量 test、vet、gofmt、diff | completed | 代码门禁通过；正式配置加载成功 |
| PC-SDK-20260715-01 | OpenAI/Anthropic Go SDK + mock upstream | completed | 仅客户端兼容性证据 |
| PC-LIVE-20260715-DIRECT-02 | 每个 RouteOwner × direct capability non-stream | completed | 见 §4；发现多项 capability drift 和一项环境不确定 |
| PC-LIVE-20260715-STREAM-03 | 成功 direct capability 的 streaming | completed | 见 §5；MiniMax non-stream 出现一次 529，stream 成功 |
| PC-GATEWAY-20260715-04 | 经部署 ai-proxy 的 native/conversion 转发 | completed | chat、completions、responses、Anthropic conversion 均 200 |
| PC-CONFIG-20260715-05 | 根据 live 证据收口 config.yaml | completed | 本地 source config 已更新并成功加载到监听阶段 |
| PC-REDEPLOY-20260715-06 | 部署收口后的 config.yaml 并复验 `/v1/models` | completed | 当前 8090 实例已反映 7-model 目录与最小公开 DTO |
| PC-LIVE-20260715-KRILL-07 | `krill-ai.completions` 520 重测 | active | 最新复测仍为 Cloudflare 520，等待上游恢复 |
| PC-DOC-20260715-01 | provider 官方文档/内部合同来源 | completed | 见 `provider-profile-contracts-2026-07-15.md`；含来源、版本、责任与复验规则 |

环境：已部署的 ai-proxy `127.0.0.1:8090`；`GET /healthz` 返回 200，`GET /v1/models` 返回 7 个
已解析 chat catalog model。两条大小写不同的 DeepSeek model 仍同时存在；公开目录不返回内部 RouteOwner、
`owned_by` 或无业务含义的 `created`。

## 3. 当前配置 authority

| RouteOwner | Protocol | Declared direct capabilities | 实测 model |
| --- | --- | --- | --- |
| `api_test_abc` | openai | chat_completions, completions | `gpt-5.5` |
| `deepseek` | openai | chat_completions | `deepseek-v4-flash` |
| `krill-ai` | openai | chat_completions, responses, completions | `grok-4.5` |
| `aiapi-minimax` | openai | chat_completions | `MiniMax-M3` |
| `aiapi-deepseek` | openai | chat_completions | `DeepSeek-V4-Flash` |

`text-embedding-3-large` 已从 source catalog 移除，因为 `api_test_abc` 的 `/v1/embeddings` 确认 404。
当前配置不发布 embeddings operation；日后接入已验证的 embedding provider 时，再将模型与 capability 一起加入。

## 4. Direct capability non-stream live matrix

| RouteOwner | Capability | Model | HTTP | Conclusion | Evidence / action |
| --- | --- | --- | --- | --- | --- |
| `api_test_abc` | chat_completions | `gpt-5.5` | 200 | success | `pong`，约 5.0s |
| `api_test_abc` | completions | `gpt-5.5` | 200 | success | `pong`，约 2.3s |
| `api_test_abc` | embeddings | `text-embedding-3-large` | 404 | capability_drift | 删除声明并移除/迁移该 embedding catalog model，或修正上游 endpoint 后重测 |
| `deepseek` | chat_completions | `deepseek-v4-flash` | 200 | success | 约 1.3s |
| `deepseek` | responses | `deepseek-v4-flash` | 404 | capability_drift | 从当前 profile 删除 `responses` |
| `deepseek` | completions | `deepseek-v4-flash` | 400 | capability_drift | 当前 base URL 不支持；上游明确要求 beta API。仅在切换/拆分为已验证 beta profile 后才能保留 |
| `krill-ai` | chat_completions | `grok-4.5` | 200 | success | 约 2.7s |
| `krill-ai` | responses | `grok-4.5` | 200 | success | 约 1.5s |
| `krill-ai` | completions | `grok-4.5` | 520 ×2 | environment_undetermined | Cloudflare origin 520；保留声明，恢复后重 probe |
| `aiapi-minimax` | chat_completions | `MiniMax-M3` | 200 | success | 初次 non-stream 成功，约 1.3s |
| `aiapi-minimax` | responses | `MiniMax-M3` | 500 | capability_drift | 上游明确 `not implemented` |
| `aiapi-minimax` | completions | `MiniMax-M3` | 500 | capability_drift | 上游明确 `unsupported relay mode: 2` |
| `aiapi-minimax` | embeddings | `MiniMax-M3` | 500 | capability_drift | 上游明确 `unsupported relay mode: 3` |
| `aiapi-deepseek` | chat_completions | `DeepSeek-V4-Flash` | 200 | success | 约 0.6s |
| `aiapi-deepseek` | responses | `DeepSeek-V4-Flash` | 500 | capability_drift | 上游明确 `not implemented` |
| `aiapi-deepseek` | completions | `DeepSeek-V4-Flash` | 404 | capability_drift | Not Found |
| `aiapi-deepseek` | embeddings | `DeepSeek-V4-Flash` | 500 | capability_drift | 上游明确 `not implemented` |

probe 已修改为识别明确的 400/500 capability 拒绝，避免把 `not implemented`、`unsupported` 或要求 beta
base URL 的响应误归为临时环境故障；无语义的 520 仍保守地保留为 `environment_undetermined`。

## 5. Streaming live matrix

| RouteOwner | Capability | Model | HTTP | Conclusion | Notes |
| --- | --- | --- | --- | --- | --- |
| `api_test_abc` | chat_completions | `gpt-5.5` | 200 | success | SSE，约 1.5s |
| `api_test_abc` | completions | `gpt-5.5` | 200 | success | SSE，约 4.4s |
| `deepseek` | chat_completions | `deepseek-v4-flash` | 200 | success | SSE，约 0.2s |
| `krill-ai` | chat_completions | `grok-4.5` | 200 | success | SSE，约 0.9s |
| `krill-ai` | responses | `grok-4.5` | 200 | success | Responses event stream，约 0.9s |
| `aiapi-minimax` | chat_completions | `MiniMax-M3` | 200 | success | SSE，约 0.9s；同轮 non-stream 一次 529，归为环境波动 |
| `aiapi-deepseek` | chat_completions | `DeepSeek-V4-Flash` | 200 | success | SSE，约 0.1s |

已明确 drift 的 endpoint 不再做 stream probe；`krill-ai.completions` 要等 520 恢复后先完成 non-stream，再验证
stream。

## 6. 已部署 ai-proxy gateway 验证

| Client endpoint | Model | Result | Transport |
| --- | --- | --- | --- |
| `POST /v1/chat/completions` | `gpt-5.5` | 200；`pong` | OpenAI native → `api_test_abc` |
| `POST /v1/completions` | `gpt-5.5` | 200；`pong` | OpenAI native → `api_test_abc` |
| `POST /v1/responses` | `grok-4.5` | 200；completed response / `pong` | OpenAI native → `krill-ai` |
| `POST /v1/messages` | `gpt-5.5` | 200；Anthropic message / `pong` | Anthropic → OpenAI conversion → `api_test_abc` |

上述四次请求均只使用 catalog 已解析的 RouteOwner；未使用 provider override 或 fallback。

## 7. 配置收口结论

已根据明确 drift 更新 source `config.yaml`：

1. `api_test_abc` 已删除 `embeddings`，`text-embedding-3-large` 已移出 catalog。
2. `deepseek` 已删除 `responses` 与 `completions`；beta profile 未在本轮建立，避免改变现有 chat 的 base URL。
3. `aiapi-minimax`、`aiapi-deepseek` 均已仅保留 `chat_completions`。
4. `krill-ai` 保留 chat/responses/completions；completions 在 Cloudflare 520 恢复前维持
   `environment_undetermined`，不得宣称支持，也不应因本次结果自动删除。

收口配置已部署到 8090 并完成下列验证：

1. `/v1/models` 不再发布 `text-embedding-3-large`，仅返回 7 个 chat model，且不存在 `created`、`owned_by`。
2. 移除后的 embedding 请求返回本地 `400 model_not_found`。
3. `deepseek-v4-flash` 的 `/v1/responses` 返回本地 `400 endpoint_unsupported`，不再按旧 capability 转发。
4. `krill-ai.completions` 最新 non-stream probe 仍为 Cloudflare 520（约 1.0s），保持
   `environment_undetermined`；在恢复 2xx 或出现明确不支持语义前，不改变其配置。

每个 profile 的官方文档或内部合同来源、版本、责任和复验规则已登记在
[`provider-profile-contracts-2026-07-15.md`](provider-profile-contracts-2026-07-15.md)。待
`krill-ai.completions` 取得最终结论后，本文才能更新为 `completed`。WorkOrch 的 exact model、operations、
RouteOwner 同步和联调仍是后续独立任务。

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目定位

`ai-proxy` 是单进程、单二进制的本地 LLM API 网关。客户端只访问标准入站 path（OpenAI / Anthropic），代理**仅按请求 body 中的 exact `model`** 路由到唯一上游 RouteOwner，必要时做基础协议转换。不依赖外部数据库服务、消息队列或常驻中间件；用量明细使用进程内嵌 DuckDB。

权威产品合同见 `prd.md`（Goals / DoD 稳定 ID，如 G-02、G-03）；设计细节见 `docs/` 与 `README.md`。实现与测试应能映射回这些 ID。

## 常用命令

```bash
make run          # go run ./cmd/ai-proxy（读 config.yaml / AI_PROXY_CONFIG）
make build        # 产出 ./ai-proxy；可用 BINARY=bin/ai-proxy
make test         # go test ./...
make fmt          # go fmt ./...
make vet          # go vet ./...
make check        # fmt + vet + test（本地完整门禁）
make clean

# 单包 / 单测
go test ./internal/modules/application/proxyapi/service/proxy -count=1
go test ./internal/modules/application/proxyapi/service/proxy -run TestResolveTransportPlan -count=1
go test ./internal/pkg/aiproxyconfig -run TestValidateModelRoutes -count=1

# 上游 capability 现场探测（独立入口，不在服务启动时跑）
go run ./cmd/ai-proxy-probe -config config.yaml \
  -provider <route-owner> -capability chat_completions -model <exact-model-id>
```

配置：`cp config.example.yaml config.yaml`，密钥用 `${ENV}` 展开；**provider 条目本身必须写在配置文件中**，不能靠 env 注入创建 provider。

## 架构总览

```
cmd/ai-proxy          主服务入口：参数/信号 → internal/services/aiproxy Runtime
cmd/ai-proxy-probe    运维探针入口：调用 internal/services/probe
cmd/ai-proxy-usage-import  历史 CSV 导入入口：调用 internal/services/usageimport

internal/services/aiproxy  主 gateway process service：驱动 magicCommon application 生命周期并等待 HTTP listener
internal/services/probe    Provider live probe process service
internal/services/usageimport  CSV → DuckDB 一次性导入 process service

internal/modules/blocks/configruntime  配置 Block：启动快照与 Provider 热更新的当前配置 owner
internal/modules/blocks/usageruntime  DuckDB 用量 Block
internal/modules/blocks/metricsruntime  metrics/SLO Block（经 EventHub 获取 Usage Block）
internal/modules/application/proxyapi  OpenAI/Anthropic Application Module：Proxy 路由与热更新契约
internal/modules/application/adminapi  Provider 管理与 usage Application Module
internal/initiators/routeregistry  magicEngine RouteRegistry 与 HTTP listener Initiator
internal/pkg/aiproxycontract  Initiator / Block / Module 的 EventHub topic 与 DTO

internal/pkg/aiproxyconfig       配置加载、规范化、启动期校验；解析 model_catalog → RouteOwner
internal/pkg/aiproxyarchive      interactions/{round_id}/ 轮次归档与保留策略
internal/pkg/aiproxyclientauth   客户端 API Key 身份解析（SHA-256 索引，仅内存）
internal/pkg/aiproxyusage        DuckDB 用量 Store（Start/Complete/Dashboard/Events/导出）
internal/pkg/aiproxymetrics      Registry、Prometheus 投影、SLO 巡检与 webhook（无 HTTP adapter）
internal/pkg/aiproxymetricsport  Metrics Block 的 EventHub-backed 读写端口

web/admin             嵌入二进制的管理页（Provider + 使用统计，go:embed，无 Node 构建链）
cmd/ai-proxy-usage-import  旧 usage.csv 一次性导入 DuckDB
```

装配入口在 `internal/services/aiproxy/runtime.go`：通过 magicCommon `framework/application` + `framework/service.DefaultService` 管理 Initiator / Block / Module 生命周期。`cmd/ai-proxy/main.go` 显式 side-effect import 所需 Initiator、Block、Application Module；不得由业务包间接注册。Config Block 提供启动快照与配置激活；`routeregistry` Initiator 提供 magicEngine RouteRegistry 与 listener；Proxy、Admin Module 经 EventHub 获取 Usage 与 Metrics 端口，并经 `initiator.GetEntity` 注入 RouteRegistry 后注册路由。`cmd/` 不放业务装配。

## 路由与协议合同（核心）

两阶段权威，不要绕过：

1. **启动期** `config.Load`：每个 `model_catalog` 条目必须 **exact、大小写敏感** 地唯一匹配一个 enabled provider 的 `models` pattern，写入 `ModelInfo.RouteOwner` / `ResolvedModelRoute`。`operations` 必填且仅允许 `chat_completions` / `embeddings`。enabled provider 必须显式配置 `endpoint_capabilities`（不得从 protocol 推断）。
2. **请求期** `ResolveTransportPlan(cfg, method, path, model)`（`internal/modules/application/proxyapi/service/proxy/route.go`）：只消费已解析 RouteOwner + 固定转发矩阵，生成 `TransportPlan`（入站协议/path、上游协议/path、mode）。**禁止**再扫 provider 选路、fallback、`default_provider`、`X-AI-Provider` / `?provider=` / `provider/model` 前缀。

入站白名单：

- OpenAI：`POST /v1/chat/completions|responses|completions|embeddings`，`GET|POST /v1/models`
- Anthropic：`POST /v1/messages`
- 其它 `/v1/*` → 404

`GET/POST /v1/models` **本地合成**，不访问上游；不暴露 provider 名、base URL 或密钥。

转发矩阵（`endpoint_capabilities` 只表示上游直连能力）：

| Client | Upstream protocol | 需要的 capability | Upstream path | Mode |
|---|---|---|---|---|
| `/v1/chat/completions` | openai | `chat_completions` | 同 path | native |
| `/v1/chat/completions` | anthropic | `messages` | `/v1/messages` | `openai_to_anthropic` |
| `/v1/messages` | anthropic | `messages` | 同 path | native |
| `/v1/messages` | openai | `chat_completions` | `/v1/chat/completions` | `anthropic_to_openai` |
| `/v1/responses` | openai | `responses` | 同 path | native |
| `/v1/completions` | openai | `completions` | 同 path | native |
| `/v1/embeddings` | openai | `embeddings` | 同 path | native |

转换仅保证基础文本与基础 SSE。tools / function calling / 多模态 / `response_format` 等 → 访问上游前 `conversion_unsupported`。responses / completions / embeddings **不能**靠 chat/messages 转换派生。

本地 typed error（访问上游前）：`model_required`、`model_not_found`、`operation_unsupported`、`endpoint_unsupported`、`conversion_unsupported`、`authentication_failed`、`route_contract_invalid`、`provider_unavailable` 等；envelope 按入站协议（OpenAI vs Anthropic）输出。

## 请求处理路径（proxy）

`proxyapi/service/proxy.Handler.ServeHTTP`：路径白名单 → `clientauth` 身份解析（无 Key→`default`；未知/禁用/冲突→401）→ `UsageStore.Start`（失败 503，不访问上游）→ 读限大体 → 解析 model → `ResolveTransportPlan` → native 或 conversion → `doUpstream*` → 缓冲或 SSE 流式 → `UsageStore.Complete` / metrics / archive。

流式：首包写出后 HTTP 状态不可改写；真实结束态用 **outcome**（`success`、`client_canceled`、`idle_timeout`、`upstream_truncated`、`upstream_failed` 等）统一写入 DuckDB / Prometheus / `metadata.json`。客户端取消不得计为 upstream 故障。

热更新：`Handler.UpdateConfig` / `ConfigSnapshot` 供 admin 写回后切换运行配置（含 `client_api_keys` 索引重建）；`usage_store` 路径不热切换。保存路径必须通过与启动期相同的完整校验，且不得破坏 model_catalog 的唯一 RouteOwner 合同。

## 安全与资源边界

- 默认 `127.0.0.1:8080`。`client_api_keys` 是归属机制而非强制登录；非 loopback 需由网络层保护。**已删除** `inbound_api_key` / `AI_PROXY_INBOUND_API_KEY` / `usage_file`。
- 客户端 Key 不转上游；上游鉴权只来自 provider 配置。原始客户端 Key 不进日志/DuckDB/Web。
- `/admin` 与 admin API **固定 loopback-only**；Provider API Key 只显示“已配置”，不回显明文。
- `/metrics`、`/stats` 默认 loopback；`metrics_remote_access` 可放开。
- 体/流/SSE 行大小与 stream idle timeout 有硬上限（见 config 默认值与 env）。
- 日志与归档脱敏 `Authorization` / `X-API-Key` / `Cookie` 等。

## 可观测与落盘

- `usage.duckdb`：**单进程** DuckDB 唯一在线用量 authority；多实例不得共享。CSV 仅导出/一次性导入。
- `interactions/{round_id}/`：request/upstream/response/metadata；默认保留最近 N 轮；`archive_full_content` 可关正文。
- Prometheus 指标前缀 `ai_proxy_`；SLO 可选 webhook（状态变化、幂等 `event_id`、listener 禁止重入 `CheckNow`）。

## 修改时注意

- **model id 严格大小写敏感**；catalog 与 body 必须原文 exact 匹配。
- 改路由/能力矩阵时同步：`internal/pkg/aiproxyconfig` 校验、`ResolveTransportPlan`、`prd.md` DoD、相关 `*_test.go`、必要时 `README.md` / `docs/`。
- 不引入 provider fallback、default_provider，或从 protocol 推断 `endpoint_capabilities`。
- `Makefile` 默认 `-buildvcs=false`，避免非完整 git worktree 下 build 失败。
- 文档、管理 UI 文案以中文为主；代码标识符保持英文。

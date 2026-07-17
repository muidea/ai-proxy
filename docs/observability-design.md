# ai-proxy Observability Improvement Design

Status: superseded

Type: module-architecture

Last Updated: 2026-06-11

> 历史设计草案：下文的“当前状态”和阶段规划记录 2026-06-11 的改造起点，不描述当前实现，且不构成
> provider 路由或 fallback 合同。当前运行语义以 `README.md`、`prd.md` 和
> `workorch-model-catalog-operation-closure-plan-2026-07-15.md` 为准。

Related:

- [README.md](../README.md)
- [prd.md](../prd.md)
- `internal/pkg/aiproxyconfig/config.go`
- `internal/pkg/aiproxyarchive/recorder.go`
- `internal/pkg/aiproxyusage/`（当前实现；本文其余 CSV 内容为历史记录）
- `internal/modules/application/proxyapi/service/proxy/handler.go`

## Purpose

本文是 ai-proxy 可观测性能力的改进设计文档。当前 ai-proxy 已经具备基础的 per-interaction 落盘能力(`interactions/{round_id}/` 七件套 + `usage.csv` 追加写),但缺少聚合指标端点、时序统计、追踪 ID、告警能力,不便于从外部实时观察 cache 命中率、provider 故障率、延迟分位数等关键 SLO 指标。

本文按 P0 → P3 优先级列出改造落点,供后续代码收口使用。

## Current State Analysis

### 已具备的可观测性能力

| 能力 | 实现位置 | 落盘内容 |
|---|---|---|
| Per-interaction 全量归档 | `internal/pkg/aiproxyarchive/recorder.go:57-100` | `interactions/{round_id}/{metadata,request,request.meta,response,response.sse,upstream_request,upstream_response}.json` |
| CSV 累计记录 | `internal/stats/recorder.go:36-40` | `usage.csv`(当前 6441 行,12 列:time / provider / model / input_tokens / output_tokens / total_tokens / duration_ms / stream / estimated / http_status,缺 cache 字段) |
| Debug 日志 | `internal/modules/application/proxyapi/service/proxy/debug.go:63-67` (`debugf`) | stderr 文本日志,每 round 一组 `client_request / selected / upstream_request / upstream_response / [ai-proxy][OK] provider=...` |
| SSE 流式跟踪 | `internal/modules/application/proxyapi/service/proxy/stream_archive.go:30,166` | `TrackSSELine` 累计 usage 与 content |
| Health 端点 | `internal/modules/application/proxyapi/service/proxy/handler.go:52-58` | `GET /healthz` 返回 `{"status":"ok"}` |

### 缺失的可观测性能力

| 缺失项 | 影响 | 优先级 |
|---|---|---|
| **无 `/metrics` 端点** | 外部无法通过 Prometheus / Grafana 拉取指标 | P0 |
| **无聚合统计** | cache 命中率、provider 故障率、延迟分位数只能事后扫 CSV | P0 |
| **无 request_id / correlation_id** | 无法串联 workorch run → ai-proxy round 链路 | P0 |
| **无 p50 / p95 / p99 延迟** | 只记录单次 `duration_ms`,无滑动窗口 | P1 |
| **无 cache hit rate 时序** | cache 命中率无法按时间窗口观察 | P1 |
| **无结构化日志** | 文本 `debugf` 难被 Loki / Elastic 消费 | P1 |
| **无告警阈值** | provider 失败、cache 命中率突降无人值守 | P2 |
| **无实时流式指标推送** | TUI / 监控面板只能轮询 CSV | P2 |
| **无 stable prefix 指纹** | 无法在 ai-proxy 端发现 workorch 的 prompt 漂移 | P2 |
| **无 OpenTelemetry / Prometheus 集成** | 只能走文件 + 文本日志,无法对接现代监控栈 | P3 |
| **usage.csv 无 cache 字段** | `internal/stats/recorder.go:11` 的 Record 结构已含 CachedInputTokens / CacheCreationInputTokens / CacheHitRate,但 CSV 输出只 10 列(没写 cache 字段) | P0 |

### 关键证据

- `usage.csv` 实际表头:`time,provider,model,input_tokens,output_tokens,total_tokens,duration_ms,stream,estimated,http_status` —— **没有 cache_hit_rate / cached_input_tokens / cache_creation_input_tokens 任何一列**,即使 Record 结构里有也未落盘
- `interaction_retention: 500` 默认值,历史交互被主动清理,无法长期复盘
- 无任何 trace_id / X-Request-ID / X-Correlation-ID 注入(grep 全 0 命中)
- 无 prometheus / otel / opentelemetry 关键字

## Goals and Non-Goals

### Goals

1. 提供 Prometheus 兼容的 `/metrics` 端点,支持外部监控拉取
2. 增加 request_id / correlation_id 串联 workorch 与 ai-proxy 调用链
3. 实时聚合 cache hit rate / provider error rate / 延迟分位数
4. usage.csv 补全 cache 字段,允许 CSV-based 复盘
5. 提供 stable prefix 指纹字段,便于 workorch 侧 drift 检测
6. 结构化日志(zap / zerolog 或 slog),可被日志聚合系统消费
7. 内置 SLO 告警阈值,命中时通过事件或 webhook 发出

### Non-Goals

1. 不引入完整的 OpenTelemetry SDK(P3 才考虑,先做轻量 Prometheus)
2. 不做分布式追踪(P3 之后)
3. 不修改 ai-proxy 现有协议处理路径,仅在观测层叠加
4. 不替换 stats/recorder.go 现有的 CSV 写入,只是补字段与并行追加
5. 不为 ai-proxy 增加新的对外协议端点(仅 `/metrics` / `/stats` / 内部 SSE 推送)

## Design Overview

### 三层结构

```
[handler 层]                request_id 注入 + 计时 + 单 RouteOwner 上游状态采集
    |
    v
[metrics 聚合层]            滑动窗口 / 计数器 / 直方图(provider / model / route 维度)
    |
    v
[输出层]
    - /metrics (Prometheus 文本格式, P0)
    - /stats   (JSON 格式, P0)
    - 实时 SSE 流(可选, P2)
    - 结构化日志(stderr, JSON 格式, P1)
    - usage.csv(补 cache 字段, P0)
```

### 改造矩阵

| 阶段 | 范围 | 输出指标 |
|---|---|---|
| P0 | `/metrics` + `/stats` 端点 + request_id 注入 + usage.csv 补字段 | 可被 Prometheus 拉取 |
| P1 | 滑动窗口聚合 + p50/p95/p99 + 结构化日志 | 时间序列可见 |
| P2 | SLO 告警 + 实时 SSE 推送 + stable prefix 指纹 | 异常可主动通知 |
| P3 | OpenTelemetry 接入 + 分布式 trace 关联 | 完整可观测性栈 |

## Detailed Design

### P0 — 基础指标输出

#### P0-1. usage.csv 补 cache 字段

**位置**:`internal/stats/recorder.go`

**当前问题**:`Record` 结构已含 `CachedInputTokens` / `CacheCreationInputTokens` / `CacheHitRate`,但 `CSVRecorder.Append` 未写这些列。

**改造**:
- 在 `usage.csv` 表头追加 `cached_input_tokens,cache_creation_input_tokens,cache_hit_rate` 三列
- 写入逻辑对应 Record 字段
- 保留向后兼容(老 CSV 文件读时不报错,缺字段默认 0)

#### P0-2. 引入 request_id 注入

**位置**:`internal/modules/application/proxyapi/service/proxy/handler.go` `ServeHTTP` 入口

**改造**:
- 在每个请求入口生成或透传 `X-Request-ID`:
 - 客户端已带 → 透传
 - 客户端未带 → 用 `crypto/rand` 生成 16 字节 hex 字符串
- 写入 `http.ResponseWriter.Header()` 响应头
- 注入到 `context.Context`,供下游 archive / log / metrics 使用
- 写入 `interactions/{round_id}/metadata.json` 的 `request_id` 字段(扩展 `archive.Metadata`)

#### P0-3. `/metrics` Prometheus 端点

**位置**:`cmd/ai-proxy/main.go` 启动入口,新增路由

**实现**:
- 新建 `internal/pkg/aiproxymetrics/prometheus.go`,实现轻量 Prometheus 文本格式输出(不引入 `github.com/prometheus/client_golang`,直接手写 minimal exposition format)
- 暴露以下 metric:
 - `ai_proxy_requests_total{provider,model,route,status}` Counter
 - `ai_proxy_request_duration_seconds{provider,model,route}` Histogram
 - `ai_proxy_input_tokens_total{provider,model}` Counter
 - `ai_proxy_output_tokens_total{provider,model}` Counter
 - `ai_proxy_cached_input_tokens_total{provider,model}` Counter
 - `ai_proxy_cache_creation_input_tokens_total{provider,model}` Counter
 - `ai_proxy_cache_hit_rate{provider,model}` Gauge
 - `ai_proxy_upstream_errors_total{provider,status_code}` Counter
- 路由:`GET /metrics`,返回 `Content-Type: text/plain; version=0.0.4`
- 限流:仅本机 / localhost 可访问(默认开启,可在 config.yaml 加 `metrics_remote_access: true` 关闭)

#### P0-4. `/stats` JSON 端点

**位置**:同 P0-3

**实现**:
- `GET /stats` 返回 JSON 格式的实时聚合
- 字段:
 ```json
 {
   "uptime_seconds": 1234,
   "requests": {
     "total": 493,
     "by_provider": {"aiapi-Deepseek": 107, "deepseek-v4-flash": 119, ...},
     "by_status": {"200": 400, "5xx": 50, "4xx": 43}
   },
   "cache": {
     "by_provider": {
       "deepseek-v4-flash": {"hit": 102, "miss": 17, "hit_rate": 0.8571, "avg_cached_tokens": 11416},
       "DeepSeek-V4-Flash": {"hit": 0, "miss": 107, "hit_rate": 0.0, "avg_cached_tokens": 0}
     }
   },
   "latency_ms": {
     "p50": 1234,
     "p95": 4567,
     "p99": 8901
   },
   "errors": {
     "upstream_5xx": 50,
     "upstream_timeout": 12
   }
 }
 ```
- 路由:`GET /stats`
- 与 `/metrics` 共享底层数据,但格式更适合人工 / TUI 消费

### P1 — 时间序列聚合

#### P1-1. 滑动窗口聚合器

**位置**:`internal/pkg/aiproxymetrics/rolling.go`(新建)

**实现**:
- 滑动窗口:默认 5min / 15min / 1h / 24h 四个粒度
- 数据结构:`sync.Map` + 每粒度一个 ring buffer
- 维护:
 - 请求数 / 命中数
 - input / output / cached / cache_creation token 数
 - 延迟样本(用于分位数)
- 内存上限:每个粒度最多 10000 样本,超过则降采样

#### P1-2. 延迟分位数估算

**位置**:`internal/pkg/aiproxymetrics/quantile.go`(新建)

**实现**:
- 轻量 t-digest 或 HDR Histogram 实现(不引入第三方库,或仅引入 `github.com/beorn7/perks/quantile`)
- 暴露 p50 / p75 / p90 / p95 / p99
- 每 provider / model 单独计算

#### P1-3. 结构化日志(slog)

**位置**:`internal/modules/application/proxyapi/service/proxy/debug.go`

**当前问题**:`debugf` 输出纯文本,无 request_id 串联,无结构化字段。

**改造**:
- 切换到 Go 1.21+ 标准库 `log/slog`
- 字段化:`provider` / `model` / `round_id` / `request_id` / `duration_ms` / `status` / `cached_input_tokens` / `cache_creation_input_tokens`
- 默认输出 JSON,可由 `LOG_FORMAT=text` 切换回人类可读
- 现有 `debugf` 调用点全部改造为 `slog.Debug` / `slog.Info` / `slog.Warn`

### P2 — 告警与主动通知

#### P2-1. SLO 告警阈值

**位置**:`internal/pkg/aiproxymetrics/slo.go`(新建)

**实现**:
- 启动时读取 config 中的 SLO 阈值
 - `cache_hit_rate_min`:默认 0.20
 - `upstream_error_rate_max`:默认 0.05
 - `p99_latency_max_ms`:默认 30000
- 每次滑动窗口收尾时检查
- 命中阈值 → 写 `slo_violation` event 到 interactions/{round_id}/ 中
- 提供 webhook 配置:`slo_violation_webhook: https://...`(可选)

#### P2-2. 实时 SSE 推送

**位置**:`internal/modules/application/adminapi/service/observability/stream.go`(新建)

**实现**:
- `GET /stats/stream` 返回 SSE 流
- 每 1s 推送一次聚合数据
- 用途:TUI 实时面板、外部监控自定义集成

#### P2-3. Stable prefix 指纹

**位置**:`internal/modules/application/proxyapi/service/proxy/handler.go` 与 `internal/pkg/aiproxyarchive/recorder.go`

**实现**:
- 在收到请求时,对 `request.json` 的 system 段或稳定前 N 字节计算 `sha256`
- 写入 `metadata.json` 新增字段 `request_fingerprint` 与 `stable_prefix_hash`
- 允许 workorch 端跨 run 比对(若 ai-proxy 与 workorch 共享同一 hash 算法,workorch 端 P1-2 的 invariant assertion 可直接复用)
- 新增检测策略:若发现连续 N 个请求的 stable_prefix_hash 与上一个不同,记 `stable_prefix_drift` 事件

### P3 — 完整可观测性栈

#### P3-1. OpenTelemetry 接入(可选)

**位置**:`internal/pkg/aiproxymetrics/otel.go`(新建)

**实现**:
- 引入 `go.opentelemetry.io/otel` SDK(可选,P3 阶段再决定是否引入)
- 输出 OTLP 到 collector
- 与 Prometheus 双轨(可由 `METRICS_BACKEND=otel|prometheus` 选择)

#### P3-2. 分布式 trace 关联

**位置**:`internal/pkg/aiproxymetrics/trace.go`(新建)

**实现**:
- 解析 `traceparent` / `tracestate` 头(W3C Trace Context)
- 透传到 upstream provider 的请求头
- 在 `metadata.json` 与 `usage.csv` 记录 trace_id
- 允许 workorch 端在 OTel collector 中按 trace_id 串接

## File-by-File Change List

| 文件 | 阶段 | 改动 |
|---|---|---|
| `internal/stats/recorder.go` | P0-1 | usage.csv 追加 cache 三列 |
| `internal/modules/application/proxyapi/service/proxy/handler.go` | P0-2 | 注入 request_id |
| `internal/pkg/aiproxyarchive/recorder.go` | P0-2 | Metadata 增加 request_id 字段 |
| `internal/pkg/aiproxymetrics/prometheus.go`(新建) | P0-3 | Prometheus /metrics 端点 |
| `internal/pkg/aiproxymetrics/stats.go`(新建) | P0-4 | /stats JSON 端点 |
| `cmd/ai-proxy/main.go` | P0-3, P0-4 | 注册新路由 |
| `internal/pkg/aiproxymetrics/rolling.go`(新建) | P1-1 | 滑动窗口聚合器 |
| `internal/pkg/aiproxymetrics/quantile.go`(新建) | P1-2 | 延迟分位数 |
| `internal/modules/application/proxyapi/service/proxy/debug.go` | P1-3 | 切到 slog |
| `internal/pkg/aiproxymetrics/slo.go`(新建) | P2-1 | SLO 告警 |
| `internal/modules/application/adminapi/service/observability/stream.go`(新建) | P2-2 | SSE 推送 |
| `internal/modules/application/proxyapi/service/proxy/handler.go` | P2-3 | stable prefix 指纹 |
| `internal/pkg/aiproxyarchive/recorder.go` | P2-3 | Metadata 增加 stable_prefix_hash 字段 |
| `internal/pkg/aiproxymetrics/otel.go`(新建) | P3-1 | OpenTelemetry 接入 |
| `internal/pkg/aiproxymetrics/trace.go`(新建) | P3-2 | 分布式 trace 关联 |
| `config.example.yaml` | P0~P3 | 增 SLO 阈值 / metrics 配置 |
| `README.md` | P0~P3 | 文档更新 |

## Verification Plan

### 单元测试

- `internal/pkg/aiproxymetrics/prometheus_test.go`:文本格式输出与基本 metric 计数
- `internal/pkg/aiproxymetrics/rolling_test.go`:滑动窗口数据正确性
- `internal/pkg/aiproxymetrics/quantile_test.go`:分位数误差
- `internal/stats/recorder_test.go`:usage.csv 新增列写入与解析

### 集成测试

- 端到端:发送 N 个请求,断言 `/metrics` 输出与实际请求数 / token 数一致
- cache 命中率:`/stats` 返回值与 `usage.csv` 累加一致
- SLO 告警:人为构造低命中率请求,断言 `slo_violation` 事件生成

### 验收指标

| 指标 | 基线 | P0 目标 | P1 目标 | P2 目标 |
|---|---|---|---|---|
| `/metrics` 端点 | 无 | 可用 | 可用 | 可用 |
| 外部 Prometheus 拉取成功率 | — | 100% | 100% | 100% |
| 实时 cache hit rate 可见性 | 仅事后 CSV | 实时 | 实时 | 实时 |
| 延迟分位数 | 无 | p50 / p95 | p50 / p75 / p90 / p95 / p99 | + p99.9 |
| SLO 告警 | 无 | 无 | 阈值配置 | 触发通知 |
| 结构化日志 | 纯文本 | 纯文本 | JSON | JSON + 索引 |
| 告警延迟(命中阈值到事件产生) | — | — | < 5min | < 1min |

## Migration Plan

### Phase 1(P0, 1 周)

1. usage.csv 补 cache 字段(纯结构改动,向后兼容)
2. request_id 注入(请求入口加 middleware)
3. 新建 `internal/pkg/aiproxymetrics/` 目录骨架
4. /metrics 与 /stats 端点
5. README 增 "/metrics" 章节

### Phase 2(P1, 1 周)

1. 滑动窗口聚合器
2. 延迟分位数
3. slog 切换
4. 单元测试覆盖率 ≥ 80%

### Phase 3(P2, 1-2 周)

1. SLO 阈值配置 + 告警事件生成
2. SSE 实时推送
3. stable prefix 指纹

### Phase 4(P3, 持续)

1. OpenTelemetry 接入(评估是否引入依赖)
2. 分布式 trace 关联

## Risk Assessment

### R1. usage.csv 字段扩展破坏既有解析

- **风险**:追加 cache 列后,旧版本解析脚本可能错位
- **缓解**:CSV 头注释说明;提供 `--csv-schema v2` 显式选择;保留旧版解析入口 6 个月

### R2. request_id 注入副作用

- **风险**:workorch 端若也注入 X-Request-ID,会出现冲突 / 截断
- **缓解**:透传优先(只补全缺失);冲突时记录 `request_id_conflict` event

### R3. 滑动窗口内存占用

- **风险**:多粒度 × 4 维度 × ring buffer 可能吃 100MB+
- **缓解**:固定上限 10000 样本,超过则降采样(每 10 样本取均值)

### R4. Prometheus 文本格式手写易错

- **风险**:手写 exposition format 与 Prometheus 解析器不兼容
- **缓解**:用标准库 `expfmt` 替代手写;或引入 `prometheus/client_golang`(权衡依赖大小)

### R5. SLO 告警噪音

- **风险**:阈值不合理时频繁触发,失去告警价值
- **缓解**:阈值可配置,且默认较宽松;提供 24h 静默期;告警事件本身不进告警链

## Open Questions

1. 是否引入 `github.com/prometheus/client_golang` 依赖(完整 client)还是手写 minimal exposition format?
2. request_id 命名空间是否区分 ai-proxy 自生成 vs workorch 透传(前缀如 `ai-` vs `wc-`)?
3. SLO 告警的事件落点:`slo_violation` event 应写入 interactions/ 还是单独的 alerts/?
4. stable prefix 指纹算法与 workorch 端 P1-2 保持一致(同样的 sha256 范围),还是 ai-proxy 端独立定义?
5. SSE 实时推送与 Prometheus pull 模式冲突吗?是否需要同时支持?

## Reference

- `internal/pkg/aiproxyconfig/config.go` L12-32
- `internal/pkg/aiproxyarchive/recorder.go` L17-100
- `internal/stats/recorder.go` L11-40
- `internal/modules/application/proxyapi/service/proxy/handler.go`（唯一 RouteOwner 上游执行）
- `internal/modules/application/proxyapi/service/proxy/debug.go` L63-67(debugf), L190(upstream alert)
- `cmd/ai-proxy/main.go`(启动入口)
- 配套 workorch 端设计:`workorch/docs/30-module-architecture/llm-cache-improvement-design.md`

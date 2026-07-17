# API Key 用量统计、DuckDB 持久化与 Web 展示收口方案

Status: implemented-for-live-validation

Type: closure-plan

Last Updated: 2026-07-17

## 1. 文档目的

本文定义 ai-proxy 基于客户端 API Key 统计调用次数与 Token 用量、使用 DuckDB 持久化明细、跨进程重启
持续累计，并在现有 Web 管理端展示统计数据时必须满足的最终合同。

本文是后续实现、测试、配置迁移、Web 验收和文档同步的统一依据。实现过程中不得再同时保留以下相互竞争的
统计或认证 authority：

- 不保留单一 `inbound_api_key` 配置及其兼容映射。
- 不继续把 `usage.csv` 作为运行期统计主存储。
- 不在 DuckDB 明细之外维护另一份不可重建的持久化累计权威。
- 不把客户端 API Key 与 provider 上游 API Key 混为同一身份或同一请求头来源。

本方案延续 ai-proxy 单进程、本地运行、单个服务二进制的产品形态，但产品约束应从“不使用数据库”调整为
“不依赖外部数据库服务”。DuckDB 作为进程内嵌分析存储运行，不需要独立部署数据库服务。

## 2. 已确认决策

本轮讨论已经确认以下决策，后续实现不得重新引入隐式兼容分支：

1. 客户端 API Key 用于调用方识别与用量归属。
2. 统计核心字段至少包含调用次数、输入 Token、输出 Token 和总 Token。
3. 未携带客户端 API Key 的请求归入内置 `default` 统计桶。
4. 携带已配置 Key 的请求归入对应的稳定 `api_key_id`。
5. 携带未知、禁用、格式错误或冲突 Key 的请求返回 401，不能归入 `default`。
6. `default` 是内置虚拟 ID，不是真实密钥，不能由配置覆盖，也不能通过请求显式选择。
7. OpenAI 客户端标准使用 `Authorization: Bearer <key>`。
8. Anthropic 客户端标准使用 `X-API-Key: <key>`。
9. 入站客户端 Key 只用于 ai-proxy 身份解析，不转发给上游。
10. 上游认证继续只使用 RouteOwner provider 配置中的 `api_key`。
11. 不支持 `server.inbound_api_key`，也不支持 `AI_PROXY_INBOUND_API_KEY`。
12. 客户端 Key 唯一配置 authority 为 `client_api_keys`。
13. DuckDB 替换运行期 CSV 记录，成为唯一持久化统计 authority。
14. Web 管理端增加独立的“使用统计”页签。
15. CSV 仅作为按条件动态导出的交换格式，不再作为在线写入存储。
16. 所有历史明细默认长期保留；初版不设置默认自动删除周期。

## 3. 当前状态与差距

当前实现具备以下基础能力：

- 单一入站 Key 校验，支持 `Authorization: Bearer` 和 `X-API-Key`。
- 请求完成后写入 `usage.csv`。
- CSV 包含 provider、model、operation、TransportPlan、Token、status、outcome 和 cache 字段。
- 进程内 metrics 支持按 provider/model 聚合请求和 Token。
- `/metrics`、`/stats` 和 `/stats/stream` 已提供实时观测。
- Web 管理端已支持 Provider 查看和配置，且仅允许 loopback 访问。
- 每轮请求有 request ID、round、归档 metadata 和结构化日志。

与目标合同相比，主要缺口为：

1. 只能校验一个 `InboundAPIKey`，无法解析多个稳定调用方 ID。
2. 未携带 Key 与携带错误 Key 没有独立统计语义。
3. CSV 明细没有 `api_key_id` 和稳定 `event_id`。
4. 进程重启后 `/stats` 聚合清零，不能持续累计。
5. CSV 不适合 Web 端按日期、Key、provider、model 做分页和聚合查询。
6. CSV schema 变化依赖文件轮转，难以做稳定 migration。
7. 请求已经访问上游但进程在 CSV 记录前崩溃时，用量可能完全缺失。
8. Web 页面没有调用次数、Token、趋势、Key 排名和历史明细展示。
9. 当前 PRD、README、示例配置仍把 `inbound_api_key` 和 `usage.csv` 描述为正式合同。

## 4. 范围与非目标

### 4.1 本次范围

- 多客户端 API Key 配置、校验、启停和稳定 ID。
- 未携带 Key 时归入内置 `default`。
- OpenAI 与 Anthropic 标准请求头解析。
- 调用次数及 Token 持久化。
- DuckDB schema、migration、事务、恢复、查询和备份边界。
- `/stats` 与 Prometheus 的客户端 Key 维度。
- Web 使用统计 Dashboard、趋势、Key 汇总和明细分页。
- CSV 动态导出。
- 旧 CSV 的显式一次性导入工具。
- 文档、测试和质量门禁同步。

### 4.2 非目标

- 不建设用户账号、登录、组织、角色或权限系统。
- 不提供账单、计费价格、余额、额度扣减或支付能力。
- 不允许客户端通过 Key 改变模型 RouteOwner。
- 不允许客户端 Key 覆盖 provider 上游密钥。
- 不支持多个 ai-proxy 实例共同写同一个 DuckDB 文件。
- 不提供远程 DuckDB SQL 查询接口。
- 不允许 Web 用户提交任意 SQL、任意服务器文件路径或 DuckDB extension。
- 初版不自动删除历史明细，不做在线分区归档。
- 初版不保证跨实例全局唯一统计；多实例仍应使用独立数据库并由外部系统汇总。

## 5. 术语与核心不变量

### 5.1 术语

| 术语 | 定义 |
| --- | --- |
| 客户端 API Key | 调用 ai-proxy 时携带、用于识别调用方的密钥 |
| `api_key_id` | 配置中稳定、可读、可聚合的调用方 ID，例如 `codex` |
| provider API Key | ai-proxy 调用 OpenAI、DeepSeek、Anthropic 等上游时使用的密钥 |
| `default` | 未携带客户端 Key 时使用的内置统计 ID |
| usage event | 一次已接受客户端请求的持久化调用记录 |
| started | 请求已经持久化登记，但尚未完成最终用量结算 |
| completed | 请求已经写入最终 status、outcome 和 Token |

### 5.2 核心不变量

1. 任何统计、日志、归档、Web API 和 DuckDB 行都只能保存 `api_key_id`，不能保存原始客户端密钥。
2. 客户端 Key 不得出现在上游请求头。
3. provider Key 不得作为客户端身份或统计维度。
4. `event_id` 全局唯一，一次调用最多产生一个持久化 usage event。
5. 调用次数在 event 成功写入 `started` 时成立。
6. Token 只在 event 从 `started` 结算为 `completed` 时增加。
7. DuckDB `usage_events` 是所有累计和趋势的最终可重建 authority。
8. Web、`/stats`、Prometheus 和导出使用同一统计口径。
9. 删除或禁用 Key 不删除其历史统计。
10. 相同 `api_key_id` 轮换密钥后继续累积到原统计项。

## 6. 目标架构

```text
OpenAI / Anthropic 客户端
          │
          ▼
客户端 Key 提取与身份解析
          │
          ├── 未携带 Key ───────────────► api_key_id=default
          ├── 已知 Key ─────────────────► api_key_id=<configured id>
          └── 未知/禁用/冲突 Key ───────► 401，不计 usage
          │
          ▼
UsageStore.Start(event_id, identity, endpoint)
          │
          ├── 持久化失败 ───────────────► 503，不访问上游
          ▼
现有 model catalog / TransportPlan / provider 请求链
          │
          ▼
UsageStore.Complete(status, outcome, tokens, route metadata)
          │
          ├── DuckDB usage_events
          ├── 进程内 usage 指标镜像
          ├── /stats 与 Prometheus
          └── /admin/api/usage/*
                         │
                         ▼
                    Web 使用统计页
```

## 7. 配置合同

### 7.1 目标配置

```yaml
server:
  listen_addr: 127.0.0.1:8080

client_api_keys:
  codex:
    api_key: ${CODEX_API_KEY}
    enabled: true
  workorch:
    api_key: ${WORKORCH_API_KEY}
    enabled: true

usage_store:
  path: usage.duckdb
  memory_limit: 256MB
  threads: 2
  query_cache_seconds: 15
```

`client_api_keys` 可以为空或完全省略。此时所有未携带 Key 的请求均归入 `default`。

### 7.2 删除的配置

以下配置和环境变量必须删除，不做兼容映射：

- `server.inbound_api_key`
- 顶层 `inbound_api_key`
- `AI_PROXY_INBOUND_API_KEY`
- `usage_file`
- `AI_PROXY_USAGE_FILE`

配置文件出现已删除字段时必须 fail-fast，不能静默忽略。

### 7.3 Client API Key 校验

每个 `client_api_keys.<id>` 必须满足：

- ID trim 后非空。
- ID 规范化为小写。
- ID 只允许 `[a-z0-9][a-z0-9._-]{0,63}`。
- `default` 为保留 ID，配置中禁止声明。
- ID 大小写折叠后必须唯一。
- enabled Key 的 `api_key` 展开后必须非空。
- 不同 ID 的展开后密钥不得相同。
- 密钥只允许通过配置值或 `${ENV}` 展开提供。
- 密钥不得出现在配置错误文本中。

禁用 Key 建议仍保留密钥，以便后续重新启用；禁用状态下请求必须返回 401。

### 7.4 Usage Store 校验

- `path` 默认 `usage.duckdb`。
- 路径必须指向本地普通文件，父目录必须存在或可创建。
- 多个进程不得共享同一路径。
- `memory_limit` 默认 `256MB`，设置上下限，禁止无限制透传用户 SQL 字符串。
- `threads` 默认 `2`，最小 1，最大值受 CPU 数量和配置上限共同限制。
- `query_cache_seconds` 默认 15，允许 0 关闭缓存，设置合理最大值。
- usage store 路径和核心资源参数变更需要重启，不做运行期热切换。
- `client_api_keys` 可在通过完整配置校验后热更新。

### 7.5 访问控制边界

本方案已经确认“未携带客户端 Key 的请求允许访问并归入 `default`”。因此 `client_api_keys` 是调用方识别与统计归属
机制，不是强制登录或强制访问控制机制。

由此产生的正式边界为：

- 删除当前“非 loopback 监听必须配置 inbound API Key”的启动校验。
- 即使配置了一个或多个 client API Key，未携带 Key 的请求仍然可以访问，只是归入 `default`。
- 携带未知 Key 返回 401 是为了避免把明确但错误的身份尝试归入 `default`；调用方移除 Header 后仍会按匿名调用处理。
- 生产环境若监听非 loopback，必须通过防火墙、反向代理、VPN、网络 ACL 或其他独立接入层限制访问。
- README、示例配置和 PRD 必须明确该边界，不能继续声称 client API Key 对远程监听提供强制保护。
- 如果未来需要“必须携带 Key 才允许访问”，应新增独立、明确的 access policy 设计，不得通过修改 `default` 统计语义隐式实现。

## 8. 客户端 Key 提取与身份解析

### 8.1 标准请求头

| 客户端协议/端点 | 首选 Header | 兼容 Header |
| --- | --- | --- |
| OpenAI `/v1/chat/completions` | `Authorization: Bearer <key>` | `X-API-Key: <key>` |
| OpenAI `/v1/responses` | `Authorization: Bearer <key>` | `X-API-Key: <key>` |
| OpenAI `/v1/completions` | `Authorization: Bearer <key>` | `X-API-Key: <key>` |
| OpenAI `/v1/embeddings` | `Authorization: Bearer <key>` | `X-API-Key: <key>` |
| OpenAI `/v1/models` | `Authorization: Bearer <key>` | `X-API-Key: <key>` |
| Anthropic `/v1/messages` | `X-API-Key: <key>` | `Authorization: Bearer <key>` |

不从 query、body、Cookie、model 名、User-Agent 或自定义 provider 头获取客户端 Key。

### 8.2 解析规则

1. 收集 `Authorization` 与 `X-API-Key` 中可识别的凭据。
2. `Authorization` 存在但不是合法 Bearer 形式时视为“提供了无效凭据”，返回 401。
3. 两个 Header 都为空时返回内置身份 `ClientIdentity{KeyID: "default", Builtin: true}`。
4. 仅一个有效 Header 时查找配置索引。
5. 两个 Header 同时存在且值相同时只解析一次。
6. 两个 Header 同时存在且值不同时返回 401。
7. Key 未找到或已禁用时返回 401。
8. 不在 401 响应中暴露 Key、候选 ID 或密钥摘要。

### 8.3 运行时索引

配置加载后生成只读索引：

```go
type ClientIdentity struct {
    KeyID   string
    Builtin bool
}

type ClientKeyIndex struct {
    ByDigest map[[32]byte]ClientIdentity
}
```

请求 Key 先计算 SHA-256，再用固定长度 digest 查表。digest 只存在内存，不进入日志、DuckDB 或 Web API。

配置热更新时构造新的完整索引，通过原子指针或与现有 Config 相同的请求边界锁整体切换。已经进入处理的请求继续使用
旧身份合同，新请求使用新索引。

### 8.4 请求上下文

身份解析成功后写入 request context：

```go
type clientIdentityContextKey struct{}

func WithClientIdentity(ctx context.Context, identity ClientIdentity) context.Context
func ClientIdentityFromContext(ctx context.Context) ClientIdentity
```

后续 Handler、UsageStore、归档和日志必须只消费 context 中已经解析的身份，禁止再次读取 Header 并重复判断。

## 9. 统计口径

### 9.1 调用次数

调用次数定义为：身份解析成功、命中受支持 `/v1/*` 端点，并成功持久化 `started` event 的请求数。

以下请求计入调用次数：

- `/v1/chat/completions`
- `/v1/messages`
- `/v1/responses`
- `/v1/completions`
- `/v1/embeddings`
- GET/POST `/v1/models`
- 请求体或 model 校验失败，但已经通过身份解析并命中受支持端点的请求
- provider 上游失败、超时、429、5xx
- 流式中断、客户端取消和协议错误

以下请求不计入调用次数：

- 未知、禁用、格式错误或冲突 Key 导致的 401
- `/healthz`
- `/metrics`
- `/stats` 和 `/stats/stream`
- `/admin/*`
- 不受支持的 `/v1/*` path
- UsageStore 在写入 `started` 前失败并返回的 503

### 9.2 Token

- `input_tokens`：上游 usage 或现有本地估算得到的输入 Token。
- `output_tokens`：上游 usage 或现有本地估算得到的输出 Token。
- `total_tokens = input_tokens + output_tokens`，不接受上游不一致总数作为第二 authority。
- `cached_input_tokens` 和 `cache_creation_input_tokens` 继续保留。
- `/v1/models` Token 固定为 0。
- 请求在访问上游前失败时 Token 为 0。
- 流式首包后失败时记录已经获取或估算的 Token，并使用真实 outcome。
- `estimated=true` 表示 Token 不是上游精确 usage。

### 9.3 成功与失败

- `success_count`：最终 outcome 为 `success`。
- `failed_count`：调用次数减去 success；页面可继续按具体 outcome 展开。
- HTTP 2xx 但流式 outcome 为 `upstream_failed`、`upstream_truncated` 等时计为失败。
- 客户端取消单独保留 outcome，默认计入非 success，但 Web 可独立展示。

### 9.4 时间语义

- DuckDB 保存 `TIMESTAMPTZ`。
- `usage_date` 使用 UTC 日期，避免服务器时区变化导致重复或缺失日期桶。
- Web 使用浏览器时区显示具体时间。
- 日期趋势接口明确返回 `timezone: "UTC"`，前端负责展示说明或本地化。

## 10. DuckDB 依赖与构建合同

### 10.1 Go Driver

首选 DuckDB 官方维护的 `database/sql` Driver：

```text
github.com/duckdb/duckdb-go/v2
```

截至 2026-07-17，官方仓库声明当前 DuckDB 版本为 1.5.4，对应 driver `v2.10504.0`。实施时必须固定精确版本，
禁止依赖未锁定分支或 latest 浮动版本。

官方参考：

- `https://github.com/duckdb/duckdb-go`
- `https://duckdb.org/docs/stable/clients/go`

### 10.2 构建影响

- 当前环境为 Go 1.24、Linux amd64、`CGO_ENABLED=1`，满足首轮集成验证条件。
- Driver 引入预编译 DuckDB bindings，二进制体积和依赖下载量会明显增加。
- 必须验证 Linux amd64、Linux arm64 和项目实际发布平台。
- 不启用 Arrow build tag，除非后续有明确 Arrow 需求。
- `make build`、容器构建和发布脚本必须同步验证动态/静态依赖边界。
- Phase 0 必须记录引入 DuckDB 前后的二进制体积、冷启动时间和空闲内存差异。

## 11. DuckDB Schema

### 11.1 Schema Migration 表

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        VARCHAR NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL
);
```

Migration 必须：

- 使用单调递增整数版本。
- 在事务内执行。
- 启动时发现未知更高版本必须 fail-fast，禁止旧程序写新库。
- migration 失败时不得启动 HTTP server。

### 11.2 Usage Events

```sql
CREATE TABLE usage_events (
    event_id                    VARCHAR PRIMARY KEY,
    round_id                    BIGINT,
    started_at                  TIMESTAMPTZ NOT NULL,
    completed_at                TIMESTAMPTZ,
    usage_date                  DATE NOT NULL,

    api_key_id                  VARCHAR NOT NULL,
    provider                    VARCHAR,
    model                       VARCHAR,
    operation                   VARCHAR,
    route                       VARCHAR,
    client_endpoint             VARCHAR,
    client_protocol             VARCHAR,
    upstream_protocol           VARCHAR,
    upstream_endpoint           VARCHAR,
    conversion_mode             VARCHAR,

    input_tokens                BIGINT NOT NULL DEFAULT 0,
    output_tokens               BIGINT NOT NULL DEFAULT 0,
    total_tokens                BIGINT NOT NULL DEFAULT 0,
    cached_input_tokens         BIGINT NOT NULL DEFAULT 0,
    cache_creation_input_tokens BIGINT NOT NULL DEFAULT 0,

    http_status                 INTEGER,
    outcome                     VARCHAR,
    error_code                  VARCHAR,
    duration_ms                 BIGINT,
    upstream_duration_ms        BIGINT,
    stream                      BOOLEAN NOT NULL DEFAULT FALSE,
    estimated                   BOOLEAN NOT NULL DEFAULT FALSE,
    state                       VARCHAR NOT NULL,

    CHECK (state IN ('started', 'completed')),
    CHECK (input_tokens >= 0),
    CHECK (output_tokens >= 0),
    CHECK (total_tokens >= 0),
    CHECK (cached_input_tokens >= 0),
    CHECK (cache_creation_input_tokens >= 0)
);
```

约束：

- `event_id` 使用请求 ID，不使用会在重启后复位的 round ID。
- `round_id` 只用于与本地归档对照。
- `total_tokens` 写入前由应用计算并校验等于 input + output。
- `api_key_id` 在 started 阶段必须确定。
- provider/model 可以在 started 阶段为空，并在路由或完成时补齐。
- `state=completed` 时必须存在 `completed_at`、`http_status` 和 `outcome`。

### 11.3 索引

```sql
CREATE INDEX idx_usage_events_started_at
ON usage_events(started_at);

CREATE INDEX idx_usage_events_key_time
ON usage_events(api_key_id, started_at);

CREATE INDEX idx_usage_events_date_key
ON usage_events(usage_date, api_key_id);

CREATE INDEX idx_usage_events_provider_model
ON usage_events(provider, model);
```

索引是否全部保留必须以 Phase 0/Phase 3 benchmark 为依据；DuckDB 擅长列式扫描，不应无证据复制 OLTP 数据库的索引策略。

### 11.4 聚合权威

初版不维护事务性 `usage_totals` 或 `usage_daily` 表。以下数据均从 `usage_events` 查询或短时缓存得到：

- all-time 汇总
- 今日汇总
- 日期趋势
- API Key 排名
- provider/model 分布
- outcome 分布

若基准测试证明全量聚合无法满足 Web 延迟目标，再引入可从 `usage_events` 全量重建的派生 rollup 表；派生表不得成为
第二份不可重建 authority。

## 12. UsageStore 接口

建议新增独立 package：

```text
internal/pkg/aiproxyusage
```

核心接口：

```go
type StartRecord struct {
    EventID       string
    RoundID       int64
    StartedAt     time.Time
    APIKeyID      string
    Operation     string
    Route         string
    ClientEndpoint string
    ClientProtocol string
}

type CompleteRecord struct {
    EventID                    string
    CompletedAt                time.Time
    Provider                   string
    Model                      string
    UpstreamProtocol           string
    UpstreamEndpoint           string
    ConversionMode            string
    InputTokens                int64
    OutputTokens               int64
    CachedInputTokens          int64
    CacheCreationInputTokens   int64
    HTTPStatus                 int
    Outcome                    string
    ErrorCode                  string
    Duration                   time.Duration
    UpstreamDuration           time.Duration
    Stream                     bool
    Estimated                  bool
}

type Store interface {
    Start(context.Context, StartRecord) error
    Complete(context.Context, CompleteRecord) error
    Dashboard(context.Context, UsageFilter) (Dashboard, error)
    Events(context.Context, EventFilter) (EventPage, error)
    ExportCSV(context.Context, UsageFilter, io.Writer) error
    RecoverInterrupted(context.Context, time.Time) (int64, error)
    Checkpoint(context.Context) error
    Close() error
}
```

测试可提供内存 fake Store；生产实现为 DuckDBStore。

## 13. 写入生命周期与事务语义

### 13.1 Start 阶段

请求处理顺序固定为：

1. 生成或透传 request ID。
2. 判断是否为受支持的业务端点。
3. 解析客户端身份。
4. 计算 operation/client endpoint/client protocol。
5. 创建 round。
6. 同步调用 `UsageStore.Start`。
7. Start 成功后才读取完整业务 body、解析 model 或访问上游。

Start SQL：

```sql
INSERT INTO usage_events (
    event_id,
    round_id,
    started_at,
    usage_date,
    api_key_id,
    operation,
    route,
    client_endpoint,
    client_protocol,
    state
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'started');
```

`event_id` 冲突表示程序重复记录同一请求，必须返回内部错误并记录告警，不能静默增加第二次调用次数。

### 13.2 Complete 阶段

所有已经成功 Start 的退出路径都必须尝试 Complete，包括：

- 正常非流式完成
- 正常流式完成
- 本地 body/model/operation 校验失败
- provider 连接失败或返回非 2xx
- 流式中途失败
- 客户端取消
- 响应大小、流大小或协议限制失败

Complete SQL 必须带状态保护：

```sql
UPDATE usage_events
SET
    completed_at = ?,
    provider = ?,
    model = ?,
    upstream_protocol = ?,
    upstream_endpoint = ?,
    conversion_mode = ?,
    input_tokens = ?,
    output_tokens = ?,
    total_tokens = ?,
    cached_input_tokens = ?,
    cache_creation_input_tokens = ?,
    http_status = ?,
    outcome = ?,
    error_code = ?,
    duration_ms = ?,
    upstream_duration_ms = ?,
    stream = ?,
    estimated = ?,
    state = 'completed'
WHERE event_id = ?
  AND state = 'started';
```

受影响行数必须为 1。为 0 时可能是 event 缺失或重复 Complete，必须记录内部一致性错误。

### 13.3 写入并发

- DuckDB 写入通过单一 writer 连接串行执行。
- 不允许每个 Handler goroutine 自行创建写事务。
- 读取可使用独立连接，但必须设置最大并发，避免 Dashboard 查询耗尽资源。
- 初版 Start 和 Complete 均同步提交，不使用可能丢失事件的纯内存异步队列。
- 只有 benchmark 证明同步写入不可接受时，才设计额外的本地 write-ahead journal；不能直接改成无持久保障批量队列。

### 13.4 写入失败

| 阶段 | 行为 |
| --- | --- |
| Store 打开或 migration 失败 | 启动失败，不监听端口 |
| Start 写入失败 | 返回 503，不访问上游 |
| Complete 写入失败且响应未写出 | 返回/转换为 500 或 503，按协议输出安全错误 |
| Complete 写入失败且流式响应已写出 | 无法修改 HTTP 状态；记录 error metric、日志并将 store 标记 degraded |
| Store degraded 后的新请求 | Start 必须真实探测写入；仍失败则继续 503，成功则恢复 healthy |

必须新增稳定错误码，例如 `usage_store_unavailable`，不得把数据库错误、路径或 SQL 暴露给客户端。

## 14. 崩溃恢复与 Shutdown

### 14.1 启动恢复

启动完成 migration 后执行：

```sql
UPDATE usage_events
SET
    completed_at = now(),
    http_status = 500,
    outcome = 'process_interrupted',
    error_code = 'process_interrupted',
    state = 'completed'
WHERE state = 'started';
```

这些请求已经计入调用次数，Token 保留已写值或 0。恢复数量必须记录日志和指标。

### 14.2 优雅关闭

Shutdown 顺序：

1. 停止接受新 HTTP 请求。
2. 等待在途 Handler 完成既有 Complete。
3. 停止 UsageStore 新写入。
4. 执行最终 checkpoint。
5. 关闭 reader/writer 连接。

如果 Shutdown 超时，未完成 event 将在下一次启动时按 `process_interrupted` 恢复。

## 15. DuckDB 运行与安全设置

建议启动后设置：

```sql
SET memory_limit = '256MB';
SET threads = 2;
SET enable_external_access = false;
SET autoinstall_known_extensions = false;
SET autoload_known_extensions = false;
```

约束：

- 应用不加载远程 extension。
- Web 导出通过查询结果流式编码，不允许 DuckDB 写任意服务器路径。
- 所有 SQL 使用固定模板和参数绑定。
- 不向 Web 暴露 SQL console。
- 数据库文件权限为 `0600`，父目录建议 `0700`。
- 数据库路径不得由 HTTP 请求指定。

是否需要额外 checkpoint 配置应以所选 DuckDB 版本官方文档和故障测试为准，不在应用中猜测未知 PRAGMA。

## 16. 查询模型

### 16.1 Summary

```sql
SELECT
    count(*) AS requests,
    count(*) FILTER (WHERE outcome = 'success') AS success_requests,
    count(*) FILTER (WHERE state = 'completed' AND outcome <> 'success') AS failed_requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens
FROM usage_events
WHERE started_at >= ?
  AND started_at < ?;
```

筛选 Key、provider、model 或 outcome 时追加固定参数条件，不拼接用户输入列名或 SQL 片段。

### 16.2 API Key 汇总

```sql
SELECT
    api_key_id,
    count(*) AS requests,
    count(*) FILTER (WHERE outcome = 'success') AS success_requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens,
    max(started_at) AS last_used_at
FROM usage_events
WHERE started_at >= ?
  AND started_at < ?
GROUP BY api_key_id
ORDER BY total_tokens DESC, api_key_id ASC;
```

### 16.3 日期趋势

```sql
SELECT
    usage_date,
    count(*) AS requests,
    coalesce(sum(input_tokens), 0) AS input_tokens,
    coalesce(sum(output_tokens), 0) AS output_tokens,
    coalesce(sum(total_tokens), 0) AS total_tokens
FROM usage_events
WHERE started_at >= ?
  AND started_at < ?
GROUP BY usage_date
ORDER BY usage_date;
```

缺失日期由应用层补 0，保证 SVG 图表横轴连续。

### 16.4 明细分页

使用 cursor 分页，不使用无限增大的 OFFSET：

```sql
SELECT ...
FROM usage_events
WHERE (started_at < ? OR (started_at = ? AND event_id < ?))
ORDER BY started_at DESC, event_id DESC
LIMIT ?;
```

cursor 对 `(started_at, event_id)` 做不透明编码。`page_size` 默认 50，最大 100。

### 16.5 查询缓存

- Dashboard 查询缓存默认 15 秒。
- Cache Key 包含 from、to、api_key_id、provider、model、outcome 和 estimated 筛选。
- Event 明细默认不缓存或只做极短缓存。
- 写入完成后不做全量 cache purge，让短 TTL 自然失效。
- 缓存有明确容量上限和 LRU 淘汰，禁止任意筛选组合无限增长。

## 17. Admin API

所有新接口继续复用现有 `/admin/*` loopback-only 访问控制和安全响应头。

### 17.1 Dashboard

```http
GET /admin/api/usage/dashboard
    ?from=2026-07-01T00:00:00Z
    &to=2026-07-18T00:00:00Z
    &api_key_id=codex
    &provider=deepseek
    &model=deepseek-v4-flash
    &outcome=success
    &estimated=false
```

响应：

```json
{
  "scope": {
    "from": "2026-07-01T00:00:00Z",
    "to": "2026-07-18T00:00:00Z",
    "timezone": "UTC"
  },
  "summary": {
    "requests": 1250,
    "success_requests": 1228,
    "failed_requests": 22,
    "input_tokens": 2010000,
    "output_tokens": 450000,
    "total_tokens": 2460000,
    "average_tokens_per_request": 1968,
    "success_rate": 0.9824
  },
  "daily": [
    {
      "date": "2026-07-17",
      "requests": 120,
      "input_tokens": 180000,
      "output_tokens": 42000,
      "total_tokens": 222000
    }
  ],
  "by_api_key": [
    {
      "api_key_id": "codex",
      "status": "active",
      "requests": 35,
      "success_requests": 34,
      "failed_requests": 1,
      "input_tokens": 18000,
      "output_tokens": 4200,
      "total_tokens": 22200,
      "last_used_at": "2026-07-17T09:30:00Z"
    }
  ]
}
```

Key 状态由当前配置与历史数据组合得到：

- `default` → `builtin`
- 当前配置 enabled → `active`
- 当前配置 disabled → `disabled`
- 历史存在但当前配置不存在 → `deleted`

### 17.2 Events

```http
GET /admin/api/usage/events?page_size=50&cursor=...
```

单条明细不得返回请求正文、响应正文、客户端密钥、provider 密钥或完整错误堆栈。

### 17.3 CSV 导出

```http
GET /admin/api/usage/export.csv?from=...&to=...&api_key_id=codex
```

- 应用层通过 `encoding/csv` 流式编码查询结果。
- 设置固定 Content-Disposition 文件名。
- 导出复用相同筛选校验。
- 导出设置最大时间范围或最大行数；超过限制返回明确 400。
- 不允许用户指定服务器文件路径。

Parquet 导出可以作为后续增强，不纳入首轮 Web DoD。

### 17.4 参数边界

- 默认范围为今日 UTC。
- 快捷范围支持 today、7d、30d。
- 自定义 `from < to`。
- 非 all-time 查询默认最大跨度 366 天。
- `api_key_id`、provider、model 必须做长度和控制字符校验。
- 查询 context 设置超时，初版建议 5 秒。
- 数据库超时返回 503 或 admin 专用稳定错误，不暴露 SQL。

## 18. Web 使用统计页面

### 18.1 导航

现有 Web 增加一级页签：

```text
AI Proxy
├── Provider 管理
└── 使用统计
```

Provider 管理保持默认入口。前端使用 hash 保存页签和筛选状态，例如：

```text
#/providers
#/usage?range=7d&api_key_id=codex
```

### 18.2 页面布局

```text
┌──────────────────────────────────────────────────────────────┐
│ 使用统计       今日 | 7天 | 30天 | 自定义      刷新  导出CSV │
├────────────┬────────────┬────────────┬────────────────────────┤
│ 调用次数   │ 总Token    │ 输入Token  │ 输出Token / 成功率     │
├──────────────────────────────┬───────────────────────────────┤
│ 调用次数趋势                 │ Token 趋势                    │
├──────────────────────────────────────────────────────────────┤
│ API Key 使用量汇总                                            │
├──────────────────────────────────────────────────────────────┤
│ 最近调用明细                                                  │
└──────────────────────────────────────────────────────────────┘
```

### 18.3 核心卡片

- 调用次数
- 输入 Token
- 输出 Token
- 总 Token
- 成功率
- 平均每次调用 Token

显示使用 K/M/B 紧凑格式，tooltip 展示准确整数。

### 18.4 趋势图

- 调用次数使用柱状或折线 SVG。
- Token 使用输入/输出堆叠 SVG。
- 不引入 ECharts 等大型外部依赖。
- 图表支持空数据、单日数据和大数值。
- 图例、颜色和交互遵循当前 Ant Design 风格 token。

### 18.5 API Key 汇总表

| 字段 | 展示 |
| --- | --- |
| Key ID | `codex`、`workorch`、`default` |
| 状态 | active、disabled、deleted、builtin |
| 调用次数 | 时间范围内 event 数 |
| 成功/失败 | success 与非 success |
| 输入 Token | 累计输入量 |
| 输出 Token | 累计输出量 |
| 总 Token | input + output |
| 平均 Token | total / requests |
| 最后使用 | 浏览器本地时间 |

`default` 显示为“内置 / 未携带 Key”，但数据库值保持稳定的 `default`。

### 18.6 最近调用明细

字段：

- 时间
- Event ID，可复制
- API Key ID
- Provider
- Model
- Operation
- 输入/输出/总 Token
- HTTP status
- Outcome
- Duration
- Estimated 标签

点击记录打开抽屉，仅显示安全元数据，不显示请求正文和密钥。

### 18.7 刷新与状态

- 默认每 15 秒自动刷新 Dashboard。
- 支持手动刷新。
- 筛选输入使用 300ms debounce。
- 刷新时保留旧数据并显示局部 loading，避免整页闪烁。
- 明确展示 empty、loading、query timeout、store unavailable 状态。
- 页面显示统计存储 healthy/degraded 状态。
- 移动端允许表格横向滚动，核心卡片自适应折行。

## 19. `/stats` 与 Prometheus

### 19.1 `/stats`

增加持久化 usage 视图：

```json
{
  "usage": {
    "scope": "all_time",
    "store": {
      "engine": "duckdb",
      "healthy": true
    },
    "by_api_key": {
      "default": {
        "requests": 100,
        "input_tokens": 80000,
        "output_tokens": 12000,
        "total_tokens": 92000
      }
    }
  }
}
```

现有 provider/model 请求指标保持兼容，不强制把 `api_key_id` 追加到所有高维 label。

### 19.2 Prometheus

新增独立低维度指标：

```text
ai_proxy_client_requests_total{api_key_id="codex"}
ai_proxy_client_input_tokens_total{api_key_id="codex"}
ai_proxy_client_output_tokens_total{api_key_id="codex"}
ai_proxy_client_tokens_total{api_key_id="codex"}
```

同时增加存储运行指标：

```text
ai_proxy_usage_store_write_errors_total{phase="start|complete"}
ai_proxy_usage_store_query_errors_total
ai_proxy_usage_store_recovered_events_total
ai_proxy_usage_store_healthy
ai_proxy_usage_store_query_duration_seconds
```

客户端 Key 数量由配置约束，`default` 仅增加一个保留 label，因此 label 基数有界。未知 Key 不生成 label。

进程启动时从 DuckDB 查询 all-time Key 汇总初始化持久化 counter 镜像；Start/Complete 成功后同步更新内存镜像。
DuckDB 始终是最终 authority，镜像只用于快速 exposition。

## 20. CSV 替换与历史导入

### 20.1 在线写入切换

切换完成后：

- 删除 `internal/stats.CSVRecorder` 在线写入链。
- Handler 只依赖 `usage.Store`。
- 不再生成或追加 `usage.csv`。
- `usage_file` 配置出现时启动失败。
- README 和示例配置删除 CSV 主存储说明。

### 20.2 显式导入工具

旧 CSV 不在服务启动时自动导入，避免启动延迟和意外重复导入。提供一次性运维入口，例如：

```text
ai-proxy-usage-import \
  -source usage.csv \
  -database usage.duckdb \
  -api-key-id default
```

导入规则：

- 旧 CSV 没有调用方身份，默认归入 `default`。
- 可由运维显式指定另一个合法 ID，但不能指定原始密钥。
- 使用“源文件稳定标识 + 行号 + 行规范化摘要”生成确定 event ID。
- event ID 冲突按已导入处理，不重复入账。
- 在事务批次中写入，批次大小有界。
- 严格校验 header 和数值字段。
- malformed 行写入脱敏报告并导致非零退出，默认不部分成功后宣称完成。
- 导入后输出读取行数、写入行数、重复行数和失败行数。
- 工具不自动删除或重命名源 CSV。

历史 CSV 行已经完成，因此导入为 `state=completed`。缺少 request ID 时使用生成的 event ID。

## 21. 数据保留、备份与维护

### 21.1 保留策略

初版默认永久保留 `usage_events`，原因是 all-time 统计必须始终可从唯一 authority 重建。

后续如需删除原始明细，必须先设计并实现：

- 可验证的持久化日 rollup。
- rollup 全量重建工具。
- 删除前一致性校验。
- all-time 查询同时覆盖 rollup 和未归档明细。

在上述能力完成前，不提供 `retention_days > 0` 的自动删除行为。

### 21.2 Checkpoint

- 优雅关闭前执行 checkpoint。
- 是否周期 checkpoint 依据 DuckDB 官方版本行为和 WAL 增长 benchmark 决定。
- checkpoint 失败必须记录日志与指标，但不得输出数据库内部敏感路径给远程客户端。

### 21.3 备份

建议提供进程内维护命令或运维步骤：

1. 阻止新 UsageStore 写入。
2. 等待当前写事务结束。
3. 执行 checkpoint。
4. 复制数据库文件到目标路径。
5. 恢复写入。

禁止在未知 WAL 状态下直接复制正在写入的数据库文件并宣称备份一致。

## 22. 安全与隐私

- DuckDB 永远不存客户端原始 Key。
- DuckDB 永远不存 provider Key。
- `api_key_id` 视为运维元数据，不在公开代理响应中返回。
- `/admin/api/usage/*` 仅允许 loopback。
- `/stats` 和 `/metrics` 继续遵守现有访问控制。
- 日志可记录 `api_key_id`，但不能记录 digest 或 Header 内容。
- 401 错误只返回通用 authentication failure。
- 导出文件不包含请求/响应正文和任何密钥。
- 配置 API 只返回 Key 是否已配置，不返回 Key 值。
- SQL 全部参数化，所有排序列使用固定枚举。
- 禁用 DuckDB external access 和自动 extension 安装。
- 数据库文件和导出临时文件使用受限权限。

## 23. 代码落点

建议新增：

```text
internal/pkg/aiproxyclientauth/
  identity.go
  index.go
  resolver.go
  resolver_test.go

internal/pkg/aiproxyusage/
  store.go
  model.go
  filter.go
  duckdb.go
  migrations.go
  queries.go
  export.go
  duckdb_test.go

cmd/ai-proxy-usage-import/
  main.go
  main_test.go
```

主要修改：

| 文件/目录 | 修改 |
| --- | --- |
| `internal/pkg/aiproxyconfig/config.go` | client_api_keys、usage_store、删除 inbound/usage_file |
| `internal/modules/application/proxyapi/service/proxy/handler.go` | 身份解析、Start/Complete、删除 CSV Recorder |
| `internal/modules/application/proxyapi/service/proxy/anthropic.go` | Anthropic 路径身份保持与完成记账 |
| `internal/modules/application/proxyapi/service/proxy/models.go` | `/v1/models` 调用次数和零 Token 结算 |
| `internal/pkg/aiproxyarchive/recorder.go` | metadata 增加 `api_key_id`、`event_id` |
| `internal/pkg/aiproxymetrics/*` | Key 维度和 store health 指标 |
| `internal/modules/application/adminapi/service/admin/handler.go` | usage 查询、导出 API |
| `web/admin/index.html` | 页签、Dashboard、趋势和明细 |
| `internal/services/aiproxy/runtime.go` | magicCommon application lifecycle 与 RouteRegistry Initiator 的 listener 等待 |
| `internal/modules/blocks/configruntime/module.go` | 当前配置快照与 Provider 热更新的 EventHub owner |
| `internal/modules/blocks/usageruntime/module.go` | DuckDB Store 的 migration、checkpoint 与关闭顺序 |
| `internal/modules/blocks/metricsruntime/module.go` | metrics/SLO 装配；通过 EventHub 读取 Usage Block |
| `internal/initiators/routeregistry/` | magicEngine 路由注册、HTTP listener 生命周期与关闭信号 |
| `cmd/ai-proxy/main.go` | 显式加载 Block / Application Module |
| `config.example.yaml` | 新配置合同 |
| `README.md` | Key、DuckDB、Web 使用说明 |
| `prd.md` | Goals、Non-Goals 和 DoD 收口 |

`prd.md` 当前工作区已有未提交修改，实施时必须在理解现有差异后增量更新，禁止覆盖用户已有内容。

## 24. 测试计划

### 24.1 配置测试

- client_api_keys 正常加载和 `${ENV}` 展开。
- `default` 保留 ID 拒绝。
- ID 大小写折叠冲突拒绝。
- 重复密钥拒绝且错误不泄密。
- enabled 空密钥拒绝。
- `inbound_api_key` 拒绝。
- `AI_PROXY_INBOUND_API_KEY` 不再生效。
- `usage_file` 拒绝。
- usage_store 默认值、上下限和路径校验。

### 24.2 身份解析测试

- OpenAI Bearer → 对应 ID。
- Anthropic X-API-Key → 对应 ID。
- 两协议兼容 Header。
- 无 Header → `default`。
- 空 Header → `default`。
- malformed Authorization → 401。
- 未知 Key → 401。
- disabled Key → 401。
- 两 Header 同值 → 正常。
- 两 Header 不同值 → 401。
- query/body 中的 key 不参与解析。
- 原始 Key 不出现在日志和错误中。

### 24.3 DuckDB Store 测试

- migration 首次创建。
- migration 幂等。
- 未知更高 schema 版本 fail-fast。
- Start 插入一行。
- 重复 event ID 拒绝。
- Complete 只更新 started 行。
- 重复 Complete 拒绝。
- total = input + output。
- restart 后数据仍存在。
- RecoverInterrupted 正确结算 started 行。
- Dashboard、daily、by-key 聚合正确。
- cursor 分页稳定且无重复/遗漏。
- CSV 导出字段和筛选正确。
- store close/checkpoint 幂等。
- 多 goroutine Start/Complete 经 writer 串行后数据一致。

### 24.4 Proxy 集成测试

- default 请求成功并记录调用/Token。
- OpenAI Key 归属正确。
- Anthropic Key 归属正确。
- provider Key 不作为 api_key_id。
- 入站 Key 不转发上游。
- 401 不创建 usage event。
- 本地 model validation 失败仍完成零 Token event。
- provider 网络失败记录调用次数和零 Token。
- 非流式精确 usage。
- 非流式估算 usage。
- 流式精确 usage。
- 流式中途失败和客户端取消。
- `/v1/models` 调用次数 + 零 Token。
- Start 失败时不访问上游。
- Complete 失败时 health/metric 正确。

### 24.5 Admin API 与 Web 测试

- remote 地址访问 usage API 返回 403。
- Dashboard 默认今日范围。
- 7d/30d/自定义范围。
- Key/provider/model/outcome 筛选。
- 时间范围、page size 和 cursor 校验。
- 导出不包含密钥和正文。
- Web 页签切换。
- default/active/disabled/deleted 标签。
- 大数值格式化和 tooltip。
- 空数据、错误、loading 状态。
- SVG 单点、多点和全零数据。
- 自动刷新不会产生无限并发请求。
- 页面 JavaScript 语法检查。

### 24.6 构建和性能测试

- `go test ./...`
- `go vet ./...`
- `gofmt` / diff check
- Linux amd64 build
- Linux arm64 build 或明确的交叉构建验证
- 二进制体积差异记录
- 冷启动和 migration 时间
- 1 万、10 万、100 万 usage 行 Dashboard 查询基准
- 同步 Start/Complete p50/p95/p99 延迟
- Web 15 秒轮询下的 CPU/内存影响
- 长时间运行后的 DuckDB/WAL 文件增长

## 25. 分阶段实施计划

### Phase 0：DuckDB 技术门禁

目标：证明 Driver、构建、资源和小事务满足项目要求。

交付：

- 引入固定版本 `github.com/duckdb/duckdb-go/v2` 的实验分支。
- 最小 DuckDB open/migrate/insert/update/query/close 测试。
- Linux amd64 构建。
- 二进制体积、启动时间、空闲内存记录。
- 1/10/50 并发请求下串行 writer benchmark。

退出条件：

- 无 crash、数据损坏或不可接受构建依赖。
- 单次同步 Start/Complete 延迟满足本地代理目标。
- Dashboard 百万行聚合满足 Web 查询目标。

### Phase 1：配置与身份合同

- 新增 client_api_keys。
- 删除 inbound_api_key 和相关环境变量。
- 新增 clientauth index/resolver。
- request context 注入身份。
- 配置、解析、脱敏测试完成。

### Phase 2：DuckDB UsageStore 与代理接线

- 正式 schema/migration。
- Start/Complete/RecoverInterrupted。
- 删除运行期 CSV Recorder。
- 所有代理退出路径结算。
- metrics/store health。
- 全量 proxy 测试通过。

### Phase 3：查询 API 与 Web

- Dashboard/query cache。
- Events cursor 分页。
- CSV 导出。
- Web 使用统计页、SVG 趋势、汇总表、明细抽屉。
- loopback 安全和查询边界测试。

### Phase 4：迁移、文档与发布

- 一次性 CSV import 工具。
- config.example、README、PRD 和 observability 文档同步。
- 实际部署配置迁移。
- 备份和恢复演练。
- 全量质量门禁与 live 验证。

## 26. Definition of Done

### 26.1 身份合同

- [x] 只有 client_api_keys 是客户端 Key 配置 authority。
- [x] inbound_api_key 和对应环境变量完全删除。
- [x] 未携带 Key 稳定归入 default。
- [x] 已知 Key 稳定归入配置 ID。
- [x] 未知、禁用、格式错误、冲突 Key 返回 401。
- [x] 入站 Key 不转发上游。
- [x] 原始 Key 不进入任何日志、存储、指标或 API。

### 26.2 持久化

- [x] DuckDB 是唯一在线 usage 主存储。
- [x] usage.csv 不再在线追加。
- [x] 每个已接受调用在访问上游前持久化 started。
- [x] 所有正常和异常退出路径尝试 completed。
- [x] event ID 防止重复入账。
- [x] 重启后累计数据不丢失。
- [x] started 遗留记录可恢复为 process_interrupted。
- [x] Store 写入、查询和 checkpoint 错误可观测；启动失败会阻止 HTTP server 启动。

### 26.3 统计口径

- [x] 调用次数、输入 Token、输出 Token、总 Token 口径统一。
- [x] total 始终等于 input + output。
- [x] 精确与估算 Token 可区分。
- [x] status 与 outcome 同时保留。
- [x] default、active、disabled、deleted 历史均可查询。
- [x] Web、Admin API、`/stats` 和 Prometheus 均从 DuckDB 口径初始化并在成功结算后更新。

### 26.4 Web

- [x] Provider 管理和使用统计页签可切换。
- [x] 今日、7d、30d、自定义范围可用。
- [x] 核心卡片正确展示。
- [x] 调用与 Token 趋势正确展示。
- [x] API Key 汇总表正确展示。
- [x] 最近调用明细支持 cursor 分页。
- [x] CSV 导出可用且不泄密。
- [x] 页面保持轻量、响应式和 loopback-only。

### 26.5 工程质量

- [x] DuckDB Driver 固定精确版本。
- [ ] 目标构建平台验证通过。
- [x] 全量 test/vet/format/diff check 通过。
- [ ] 性能与资源基准有记录。
- [x] 配置、README、PRD、示例和运维文档同步。
- [ ] 实际部署完成备份、迁移、重启与 live 验证。

## 27. 风险与控制

| 风险 | 控制措施 |
| --- | --- |
| DuckDB 对高频小事务不如 SQLite | 单 writer、同步小事务、Phase 0 benchmark |
| Driver 增加二进制体积 | 记录体积基线，不启用 Arrow，验证发布接受度 |
| CGO/平台构建复杂 | 固定官方 Driver，验证目标 OS/arch 构建矩阵 |
| Dashboard 查询抢占代理资源 | threads/memory 限制、查询超时、短缓存、有限并发 |
| Complete 失败造成 started 遗留 | store degraded、指标告警、启动恢复 |
| 数据库损坏 | checkpoint、受控备份、启动 fail-fast、恢复演练 |
| Key 数量导致指标基数增长 | Key ID 只来自有界配置，未知 Key 不创建 label |
| 删除 Key 后历史不可识别 | 历史按稳定 ID 保存，Web 标记 deleted |
| 匿名访问绕过 Key 统计 | 明确归入 default；Key 是归属机制，不是强制登录体系 |
| 旧 CSV 重复导入 | 确定 event ID、唯一约束、显式导入工具 |

## 28. 发布与回滚

### 28.1 发布前

1. 备份现有配置、usage.csv 和交互归档。
2. 准备 client_api_keys 配置。
3. 删除 inbound_api_key 与 usage_file。
4. 在非生产目录完成 CSV import 演练。
5. 验证 DuckDB 文件权限和磁盘空间。
6. 运行 probe、全量测试和 Web 手工验收。

### 28.2 发布后验证

- 无 Key 的 OpenAI 请求归入 default。
- 有 Key 的 OpenAI 请求归入对应 ID。
- 有 Key 的 Anthropic 请求归入对应 ID。
- 未知 Key 返回 401。
- DuckDB 中 started/completed 状态正确。
- Web 今日统计与直接 SQL/测试查询一致。
- 重启后 all-time 统计保持。
- Prometheus 与 `/stats` 数值一致。
- CSV 导出与查询范围一致。

### 28.3 回滚

本方案明确不保留运行期双写 CSV，因此回滚必须以发布前备份和版本化配置为基础：

- 回滚旧二进制前恢复旧配置。
- 新 DuckDB 文件保留，不删除。
- 如需把新数据带回旧系统，只能通过显式 CSV 导出，不做在线双写。
- 不允许旧二进制打开或修改未知 DuckDB schema。

## 29. 最终收口边界

完成本方案后，ai-proxy 的客户端用量链路必须只有一条：

```text
标准协议 Header
  → client_api_keys / default 身份解析
  → DuckDB started event
  → 唯一 RouteOwner 上游调用
  → DuckDB completed usage
  → /stats、Prometheus、Admin API、Web Dashboard、CSV 导出
```

任何新增统计维度都必须从 `usage_events` 可重建，并同步更新 schema migration、查询 DTO、Web 展示、指标基数评估、
测试和 PRD。不得重新引入 CSV 在线 authority、单一 inbound API Key 或未经配置的动态 Key label。

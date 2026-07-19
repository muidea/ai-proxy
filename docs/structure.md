# 代码结构与运行时边界

## 目标

`ai-proxy` 使用 `github.com/muidea/magicCommon/framework/application` 管理进程生命周期。项目保持单 Go module、单 HTTP gateway 进程：路由与 listener 属于 Initiator，技术运行时属于 Block，入站业务聚合属于 Application Module。

生命周期为：

```text
cmd/ai-proxy（显式 side-effect import）
  → internal/services/aiproxy.Runtime
  → magicCommon application.Startup → Initiator.Setup → Module.Setup
  → magicCommon application.Run     → Initiator.Run   → Module.Run
  → magicCommon application.Shutdown → Module.Teardown → Initiator.Teardown
```

进程 service 使用 `framework/service.DefaultService` 驱动 plugin 生命周期。framework 负责 application、EventHub 和 BackgroundRoutine；各 plugin 负责自己的资源。

## 目录职责

```text
cmd/
  ai-proxy/               版本注入、framework 组件显式加载和进程退出码
  ai-proxy-probe/         独立 probe 入口
  ai-proxy-usage-import/  独立历史导入入口

internal/services/
  aiproxy/                主 gateway 的配置、信号处理、framework lifecycle shell 与 listener 等待
  probe/                  Provider live probe 进程服务
  usageimport/            CSV → DuckDB 一次性导入进程服务

internal/modules/
  base/biz/               Module/Block 的共享 EventHub、observer 与 BackgroundRoutine 基座
  blocks/configruntime/   Provider 配置 Block；biz/ 拥有启动快照与热更新后的当前配置
    pkg/events/                 Config Block 自有的启动快照与配置激活合同
  blocks/usageruntime/    DuckDB 用量 Block；biz/ 管理 migration、checkpoint 与关闭
    pkg/events/                 Usage Block 的逐命令 typed 合同
  blocks/metricsruntime/  metrics/SLO Block；biz/ 管理 Registry 与 SLO 生命周期
    pkg/events/                 Metrics Block 的记录与查询 typed 合同及 EventHub-backed Port
  application/proxyapi/         OpenAI/Anthropic Application Module
    biz/                         EventHub 配置更新与运行期依赖
    pkg/events/                  Proxy Module 自有的配置更新合同
    service/proxy/               入站鉴权、路由、转换、转发与归档钩子
  application/adminapi/         Provider 管理与 usage Application Module
    biz/                         EventHub-backed 配置、Usage 与 Metrics 依赖
    service/admin/               loopback-only Provider 管理与 usage HTTP adapter
    service/observability/       metrics、stats 与 stats SSE HTTP adapter

internal/pkg/
  aiproxybootstrap/       process service → framework 启动基础设施的单次启动快照桥接
  aiproxyconfig/          YAML 配置、环境变量展开与启动期 route 校验
  aiproxyarchive/         interaction round 归档
  aiproxyclientauth/      客户端 API Key 身份索引与解析（由 Proxy Module 持有）
  aiproxyusage/           DuckDB usage store、查询、导出与迁移
  aiproxymetrics/         Registry、Prometheus 投影、SLO evaluator（无 HTTP route）
  aiproxymetricsport/     Metrics Block 的 EventHub-backed 读写端口

internal/initiators/
  routeregistry/          magicEngine RouteRegistry、HTTP listener 与关闭信号基础设施
web/admin/                嵌入二进制的管理页
```

## 边界决策

- `cmd/*` 不承载业务装配、HTTP server、存储或 CLI 业务逻辑；它们只调用对应 process service。
- `internal/services/aiproxy` 是进程级 service，不是 plugin module：它只驱动 application lifecycle，并通过 RouteRegistry Initiator 等待 HTTP listener 退出。
- Config Block 是启动配置与 Provider 热更新后的当前配置 owner。`routeregistry` Initiator 是 magicEngine RouteRegistry 与 listener 的进程级基础设施 owner；它只暴露窄的 `RouteRegistryHelper`，不承载任何业务状态。listener 由 process service 在所有 Module 路由注册后启动，避免启动窗口 404。
- Usage 与 Metrics/SLO 是独立技术 Block，不暴露 HTTP route 或可变资源对象。Metrics Block 经 `MetricsPort` 接收记录事件和返回只读投影；Proxy 直接持有其唯一使用的 Client API Key 索引与 interaction archive。
- Proxy API 与 Provider Admin 是有状态业务聚合 Module：它们通过 EventHub 获取 Block 依赖，并在 `Setup` 中注入 `RouteRegistryHelper`、在 `Run` 中注册各自路由。Admin 请求 Config Block 激活新配置，Config Block 同步命令 Proxy 应用新快照。Proxy 仅注册协议白名单路径，不依赖 Module Weight 确保路由优先级。
- `internal/pkg/aiproxyarchive`、`internal/pkg/aiproxyclientauth`、`internal/pkg/aiproxyconfig`、`internal/pkg/aiproxyusage`、`internal/pkg/aiproxymetrics` 是对应运行单元使用的 focused package；它们不拥有 HTTP route 或 framework 生命周期。
- EventHub topic、Command 与 Result 由投递 owner 的 `pkg/events` 定义：Config、Usage、Metrics 和 Proxy 各自拥有其合同；`aiproxymetricsport` 仅定义 Metrics 的窄端口，生产实现由 Metrics owner-local EventHub client 提供。
- 新增 magicCommon plugin module 的前提是：具备独立 Setup/Run/Teardown、正式状态 owner、route/listener 或 EventHub 订阅，并由 `cmd` 显式加载；不得仅为缩短文件而创建 module。

## 变更规则

1. 新的可执行能力先判断是否为 process service；若是，落在 `internal/services/<entry-name>`，`cmd/<entry-name>` 只保留入口。
2. 新的 HTTP handler 放在既有 Application Module 的 `service/` 下；Module 在 `Setup` 中经 `initiator.GetEntity` 注入 `RouteRegistryHelper`，并在 `Run` 中由 service 的 `RegisterRoutes` 声明到 magicEngine RouteRegistry。不要让 `cmd`、process service 或 focused package 直接承载 route。
3. 跨 owner 的正式状态读写使用稳定 port 或 EventHub command/response；不要通过全局 service facade 反向访问 owner 实现。
4. 若新增 plugin module，入口必须显式选择和注册它，且补充 Setup/Run/Teardown、依赖失败 fast、测试和本文档。

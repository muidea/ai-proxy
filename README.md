# ai-proxy

轻量级本地 LLM API 网关。它提供 OpenAI 和 Anthropic 标准入站端点，严格按请求中的 exact `model` 路由到唯一上游 Provider；用量明细持久化至进程内嵌 DuckDB，并提供本地 Web 管理页。

## 快速开始

要求 Go 1.24+。先从示例创建配置，填入 Provider 和模型目录，再启动服务：

```bash
cp config.example.yaml config.yaml
export OPENAI_API_KEY=sk-... # 供 config.yaml 中的 ${OPENAI_API_KEY} 展开
make run
```

默认地址为 `http://127.0.0.1:8080`。启动后可访问：

- [Provider 管理与使用统计](http://127.0.0.1:8080/admin/)（默认仅 loopback；可启用账号密码登录后远程访问，见配置参考）
- `GET /healthz`
- `GET /metrics`、`GET /stats`（默认仅 loopback）

客户端使用裸模型名与标准地址：

```text
OpenAI API base:    http://127.0.0.1:8080/v1
Anthropic API base: http://127.0.0.1:8080
```

所有数据端点都要求客户端 API Key：OpenAI 客户端使用 `Authorization: Bearer <key>`，Anthropic 客户端使用 `X-API-Key: <key>`。缺失、未知或禁用的 Key 返回 401，且不产生用量记录。

## 常用命令

```bash
make run                         # 使用 config.yaml 启动
make check                       # 格式、vet、全量测试
make build                       # 构建当前平台二进制
make release-package VERSION=v1.2.3
ai-proxy admin password-hash     # 交互式生成 Admin Argon2id 密码哈希
ai-proxy admin set-credentials --username ops-admin --config config.yaml # 创建或重置 Admin 登录凭据
```

完整多平台发布由推送 `vX.Y.Z` tag 的 GitHub Actions 完成；详情见[运维与发布说明](docs/operations.md#构建与发布)。

## 文档

| 主题 | 文档 |
| --- | --- |
| 配置、客户端 Key、Provider 管理 | [配置参考](docs/configuration.md) |
| 客户端 Key 管理收口 | [客户端 API Key 管理设计](docs/client-api-key-management-design-2026-07-20.md) |
| Admin 账号密码登录 | [Admin 登录安全设计](docs/admin-login-security-design-2026-07-23.md) |
| 协议端点、模型路由、转换与错误合同 | [Provider Capability Contract](docs/provider-capability-contract-design-2026-07-15.md) |
| DuckDB 用量、Admin API、Web 统计 | [API Key 用量与 DuckDB 收口方案](docs/api-key-usage-duckdb-web-closure-plan-2026-07-17.md) |
| 运行、监控、归档、探针、备份与发布 | [运维与发布](docs/operations.md) |
| 目录职责与 magicCommon 生命周期 | [代码结构](docs/structure.md) |
| Provider 现场能力审计 | [Provider Capability Audit](docs/provider-capability-audit-2026-07-15.md) |
| Provider profile 维护 | [Provider Profile Contract Register](docs/provider-profile-contracts-2026-07-15.md) |

`config.example.yaml` 是可复制的完整配置起点；所有 Provider 必须显式写入配置文件，不能由环境变量创建。

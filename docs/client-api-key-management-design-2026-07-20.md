# 客户端 API Key 管理设计

Status: proposed-for-implementation

Type: design-and-closure-plan

Last Updated: 2026-07-20

## 1. 目的与已确认决策

本设计在既有 `client_api_keys` 身份解析、用量归属和配置热更新能力之上，增加本地 Admin 管理端的客户端 API Key 创建、启停、轮换和删除能力。

本次确认的决策：

1. `client_api_keys` 仍是客户端 Key 的唯一持久化 authority；不新建账号库、密钥表或外部 Secret 服务。
2. Admin 创建或轮换的 Key 由服务端生成高熵随机值；明文只在成功响应中返回一次。
3. Admin 托管的 Key 在 YAML 中只保存不可逆 `api_key_hash`，不保存明文；现有 `api_key: ${ENV}` 形式继续兼容，标记为外部配置凭据。
4. Key 的稳定 ID 是用量归属 ID，不能原地改名。轮换保持同一 ID，历史和未来用量继续聚合到同一 `api_key_id`。
5. 禁用与删除都使携带该 Key 的新请求返回 401；二者均不删除 DuckDB 中的历史用量。日常暂停使用禁用，永久废弃才删除。
6. `default` 仍是未携带客户端 Key 的内置身份，不是实际凭据，不能管理或显式选择。
7. 不新增 framework Module、Block 或 Initiator。HTTP 管理能力扩展既有 `application/adminapi`；配置激活继续走 `adminapi -> configruntime Block -> proxyapi` 的 EventHub 合同。

本设计不改变 Provider 上游认证。`providers.<name>.api_key` 仅用于 ai-proxy 调用上游；这里管理的 Key 仅用于客户端调用 ai-proxy。

## 2. 问题与边界

当前项目已经支持：

- YAML 中配置多个 `client_api_keys`；
- `Authorization: Bearer <key>` 和 `X-API-Key: <key>` 身份解析；
- 启用、禁用、重复 Key 校验和 `default` 内置身份；
- Proxy Handler 在完整请求边界之间原子切换配置并重建认证索引；
- DuckDB 仅记录稳定 `api_key_id`，不记录原始 Key。

当前缺口是 Admin 无法安全管理这些 Key。直接把可复制的明文 Key 写入管理 API 或列表会扩大泄露面；把 Key 作为 Provider 管理的一部分也会混淆入站身份与上游凭据。

本次范围：

- 托管 Key 的创建、列表、启用、禁用、轮换、删除；
- 外部配置 Key 的只读识别和启用、禁用；
- YAML 原子更新、完整校验、运行时激活与失败回滚；
- Admin Web 页签、一次性复制密钥交互；
- 配置、认证、Admin API、热更新和安全回归测试。

非目标：

- 用户、组织、角色、计费、额度、过期时间、限流配额或审计系统；
- 多实例共享配置或共享 DuckDB；
- 从管理端导入任意明文 Key；
- 在 Admin API、配置导出、日志、归档或用量库中回显原始 Key；
- 将 client Key 作为非 loopback 服务的完整安全边界。

## 3. 最终配置合同

### 3.1 YAML 形态

```yaml
client_api_keys:
  # 外部配置：兼容已有方式，值可由环境变量展开。
  codex:
    api_key: ${CODEX_API_KEY}
    enabled: true

  # Admin 托管：只保存 SHA-256 摘要。
  ci-agent:
    api_key_hash: sha256:8f6c8d59e54b16e2064ecf94a03d9f2f2f17df7e889e34b401dbd16bb2b18eb5
    enabled: true
```

`api_key` 与 `api_key_hash` 必须二选一；二者同时存在或均不存在均为配置错误。唯一例外是禁用的旧条目：为了兼容当前配置，可暂时允许它无凭据，但该条目不能被重新启用，直到补齐一个有效凭据。Admin 永远不会创建这种条目。

`api_key_hash` 只接受固定格式 `sha256:` 加 64 位小写十六进制字符。它是对完整客户端 Key UTF-8 字节的 `SHA-256` 摘要。服务端生成的 Key 具有至少 256 bit 随机熵，故摘要泄露不应能被离线猜测；不得允许 Admin 指定自选 Key 或自选摘要来绕过这一前提。

Key ID 继续遵循现有规则：规范化为小写，匹配 `[a-z0-9][a-z0-9._-]{0,63}`，`default` 为保留 ID。原始 Key 与摘要在所有条目间均必须唯一；不同来源不得对应同一个认证凭据。

### 3.2 密钥生成与轮换

创建和轮换统一使用：

```text
api_key = "aip_" + base64.RawURLEncoding(crypto/rand 32 bytes)
api_key_hash = "sha256:" + hex(sha256(api_key))
```

生成结果不写日志、不写 usage、不放入错误信息，也不保存在浏览器 localStorage、URL、HTML 属性或 Toast 文本中。创建/轮换响应设置 `Cache-Control: no-store`。

轮换会在一次配置激活中用新摘要替换旧凭据。激活完成后：

- 新进入的请求仅接受新 Key；
- 已经取得旧配置快照的在途请求允许完成；
- 旧 Key 不存在宽限期；
- 稳定 `api_key_id` 不变，用量连续。

需要宽限期的场景不属于本轮；将来应采用同一 ID 的多个有效摘要及明确的 `valid_until` 合同，不能靠复制一个新 ID 伪造轮换。

## 4. 身份解析与用量不变量

`aiproxyclientauth` 的运行时索引应只存摘要到 `ClientIdentity` 的映射。对外部配置条目，启动/激活时先计算 `sha256(api_key)`；对托管条目，解析已校验的摘要。两种来源最终进入同一索引，Header 解析规则不变。

必须继续满足：

1. 无身份 Header 或空 Header返回内置 `default`；携带未知、禁用、格式错误或冲突 Header 返回 401。
2. `Authorization` 与 `X-API-Key` 均不转发给上游。
3. 401 在 `UsageStore.Start` 之前返回，不产生 usage event。
4. 所有持久化、指标、日志、归档和 Admin 查询只出现 `api_key_id`，不出现明文 Key、摘要或 Header 值。
5. Key 禁用、删除或轮换不删除既有 `usage_events`，也不重写历史 `api_key_id`。

这也解释了常见的 `/v1/models` 401：客户端携带了未知或已禁用 Key。创建并启用客户端 Key 后，应将该返回的 Key 配置为客户端的 `OPENAI_API_KEY` / Anthropic client key；它不能替代 Provider 的上游 API Key。

## 5. Admin HTTP 合同

所有以下端点保持既有约束：仅 loopback 请求可访问；所有写操作必须带 `X-AI-Proxy-Admin: 1`；请求和响应均为 JSON；响应设置 `Cache-Control: no-store`。写操作还必须带最近一次列表响应的 `If-Match: <revision>`，以避免过期浏览器视图无意覆盖手工配置变更。

`revision` 是当前配置文件原始字节的 SHA-256 强校验值，作为不透明字符串返回。任一写入读取到不同 revision 时返回 HTTP 409，且不写文件、不激活运行时；Web 端重新加载列表后再由操作者确认。

### 5.1 列表

```text
GET /admin/api/client-api-keys
```

```json
{
  "revision": "sha256:...",
  "writable": true,
  "hot_reload": true,
  "client_api_keys": [
    {
      "id": "ci-agent",
      "enabled": true,
      "credential_source": "managed",
      "key_configured": true
    },
    {
      "id": "codex",
      "enabled": true,
      "credential_source": "external",
      "key_configured": true
    }
  ]
}
```

列表按 ID 排序。`credential_source` 仅能为 `managed` 或 `external`；不暴露摘要、密钥长度、尾号、环境变量名或创建时间。`default` 不在此列表内。

### 5.2 创建

```text
POST /admin/api/client-api-keys
If-Match: <revision>
X-AI-Proxy-Admin: 1
Content-Type: application/json

{"id":"ci-agent","enabled":true}
```

成功返回 HTTP 201：

```json
{
  "id": "ci-agent",
  "enabled": true,
  "api_key": "aip_<only-returned-here>",
  "message": "Copy this API key now. It cannot be displayed again."
}
```

ID 已存在或为 `default` 返回 409；格式错误返回 400；没有可写配置文件返回 409。服务端不接受 `api_key`、`api_key_hash` 或任意未知字段。

### 5.3 启用、禁用

```text
PATCH /admin/api/client-api-keys/{id}
If-Match: <revision>
X-AI-Proxy-Admin: 1

{"enabled":false}
```

只允许修改 `enabled`。外部与托管条目都可禁用；外部无凭据旧条目重新启用时应由配置校验拒绝。成功返回更新后的非敏感视图和新 revision。

### 5.4 轮换

```text
POST /admin/api/client-api-keys/{id}/rotate
If-Match: <revision>
X-AI-Proxy-Admin: 1
```

轮换仅作用于已存在且启用的条目。它总是转换为 Admin 托管凭据：移除现有 `api_key`（包括 `${ENV}` 表达式），写入新的 `api_key_hash`，并一次性返回新明文 Key。Web 必须在提交前对外部配置来源提示“将不再由环境变量管理”。

### 5.5 删除

```text
DELETE /admin/api/client-api-keys/{id}
If-Match: <revision>
X-AI-Proxy-Admin: 1
```

删除配置条目后返回 HTTP 204。未知 ID 返回 404，`default` 返回 400。历史用量保持可查询；使用统计页中的历史 Key 筛选项继续标注为不在配置中。

所有 Admin 错误沿用既有 envelope：

```json
{"error":{"message":"safe human-readable message"}}
```

错误信息不得包含明文 Key、摘要、完整配置文件内容或环境变量展开值。

## 6. 配置写入与激活事务

Provider 管理现有的“写临时文件 -> Load 校验 -> rename -> Activate”流程在激活失败后可能造成磁盘配置已替换而运行时仍是旧配置。客户端 Key 管理实现时必须提炼为 Admin 共用的配置事务，并同时迁移 Provider 写入路径。

事务在 `admin.Handler.updateMu` 保护下执行：

1. 读取当前 YAML 原始字节、文件权限和 revision；验证 `If-Match`。
2. 解析 YAML document，只定点修改 `client_api_keys` 或 `providers` 的目标 mapping；不重写无关 section。未改动的原始 `api_key: ${ENV}` 标量必须保留。
3. 将候选 YAML 以原文件权限写入同目录临时文件，调用 `config.Load(tempPath)` 做完整校验、环境变量展开和 model route 解析。
4. 原子 rename 候选文件覆盖正式配置；随后通过既有 `RuntimeConfig.UpdateConfig` 激活 Config Block 与 Proxy Handler。
5. 若激活成功，返回新 revision 和非敏感结果。
6. 若激活失败，用预先保存的原始字节和原权限写入新的同目录临时文件并原子 rename 回滚；返回 500。若回滚也失败，必须记录不含 secret 的严重错误并返回明确的“configuration activation rollback failed”。

在单进程管理 API 中 `updateMu` 防止并发写；revision 处理服务外手工编辑与已打开的浏览器之间的竞争。该机制不承诺多进程共享同一 YAML 文件，运行文档必须继续声明单实例配置写入边界。

候选配置在 `config.Load` 完成前绝不激活；激活前出现的任一失败都不改变正式文件或运行时配置。`usage_store` 路径和资源参数仍不可热切换；因为 Admin Key 管理只修改 `client_api_keys`，候选配置不会触发该变更。

## 7. Web 管理端

导航新增“客户端 Key”页签，位于 Provider 管理和使用统计之间。页面包含：

- 表格：ID、凭据来源（托管/外部配置）、状态、操作；
- 新建按钮：仅输入 ID 与初始启用状态；
- 启用/禁用开关；
- 托管 Key 的轮换按钮；外部 Key 的“转换并轮换”按钮；
- 删除按钮，要求输入或确认目标 ID；
- 创建/轮换成功后的专用一次性密钥对话框：可复制、清晰提示关闭后不可恢复，且不把密钥写入 URL、hash、页面状态持久化或普通通知。

前端每次成功写入后重新读取列表和 revision。409 时保留表单输入但不重试，提示“配置已由其他操作修改，请刷新确认后重试”。当 `writable=false` 时所有写入控件禁用，仍可查看非敏感列表。

使用统计页不改变其 DuckDB 查询合同；它继续通过现有筛选选项显示已删除、已禁用或未配置的历史 `api_key_id`。

## 8. 代码落点

| 位置 | 变更职责 |
| --- | --- |
| `internal/pkg/aiproxyconfig` | 解析 `api_key_hash`，校验来源互斥、摘要格式与跨条目唯一性；导出已解析摘要的稳定条目视图。 |
| `internal/pkg/aiproxyclientauth` | `KeyEntry` 同时接受原始 Key 或已解析摘要；索引仅以 `[32]byte` 摘要进行查找。 |
| `internal/modules/application/proxyapi/service/proxy` | 将配置条目转换为新的认证索引输入；保持现有原子 `UpdateConfig` 请求边界。 |
| `internal/modules/application/adminapi/service/admin` | 增加 client-key handler、请求 DTO、只读 view、加密随机生成、revision 校验和通用配置事务；不得直接访问 Proxy Handler。 |
| `internal/modules/application/adminapi/service/admin/routes.go` | 为 `/admin/**` 增加 `POST`、`PATCH`、`DELETE` 方法注册。 |
| `web/admin/index.html` | 增加页签、列表、表单、一次性密钥对话框和 revision/409 交互。 |
| `docs/configuration.md`、`config.example.yaml`、`README.md`、`prd.md` | 代码收口时同步最终配置样例、操作说明、端点入口与 DoD。 |

不新增 module/block 的理由：Key 管理没有独立生命周期或资源 owner；它是既有 Admin Module 对 Config Block 的一次配置变更用例。Admin service 继续只负责 HTTP request/response，跨 owner 激活继续由 `adminapi.biz.UpdateConfig` 触发 Config Block 的 typed EventHub 合同。Proxy Handler 仍是 client Key 索引的唯一运行时持有者。

## 9. 实施顺序

1. 扩展配置数据结构、YAML 解析、规范化和校验；补 `api_key_hash`、冲突、泄露防护与兼容测试。
2. 扩展 `clientauth.KeyEntry` 与索引构建，使原始 Key 和摘要的解析行为一致；补认证单测。
3. 提炼 Admin 配置事务并先将 Provider 更新迁移到它，验证成功激活与激活失败回滚。
4. 实现 client-key Admin API、route method 注册和 handler 测试。
5. 实现管理页并补浏览器可验收的 DOM/API 交互测试或最小端到端测试。
6. 更新示例、配置说明、README、PRD DoD；运行格式、静态检查与全量测试。

每一步都应保持已有 `api_key` 配置和 Provider 管理可用；不得通过一次不可回退的配置格式迁移阻塞已有部署。

## 10. 验收标准（DoD）

- [ ] `api_key_hash` 支持严格格式校验，与 `api_key` 互斥，并在不同 ID 间做原始凭据/摘要去重。
- [ ] 现有 `client_api_keys.<id>.api_key: ${ENV}` 启动和热更新行为保持兼容。
- [ ] Admin 创建生成高熵 `aip_` Key，持久化后 YAML、运行时快照、日志、DuckDB、归档和列表均不含该明文。
- [ ] 创建/轮换的成功响应只在该次响应包含明文 Key，并使用 `Cache-Control: no-store`。
- [ ] 托管、外部、禁用、删除、未知、冲突 Header 与 `default` 的认证结果符合第 4 节不变量。
- [ ] 启用、禁用、轮换、删除后，下一新请求在无需重启的情况下使用新身份索引；在途请求不发生数据竞争或 panic。
- [ ] 同 ID 轮换后用量继续写入同一 `api_key_id`；禁用或删除不删除历史 usage。
- [ ] Admin API 严格限制 loopback、写方法、JSON body 大小、未知字段和 `X-AI-Proxy-Admin`；缺失或过期 revision 不改变配置。
- [ ] 任何候选配置校验失败、激活失败或回滚路径都不会将未校验配置留为生效配置；激活失败时磁盘与运行时配置恢复一致。
- [ ] Provider 管理迁移到同一事务后，保留现有 secret 不回显与 `${ENV}` 保留行为。
- [ ] Web 不持久化、不在普通通知中展示、不在 DOM 历史记录中保留明文 Key；外部凭据轮换有破坏性确认。
- [ ] 直接包测试、`go test ./... -count 1`、`make check` 和相关文档链接检查通过。

## 11. 运行与安全说明

Admin API 仅允许 loopback 是降低误暴露面的必要条件，不是本机多用户环境中的强身份认证。将服务监听到非 loopback 时，client Key 仍是调用方归属机制，必须由反向代理、防火墙或私有网络承担访问控制。

配置文件和 DuckDB 都应由运行用户保护。尽管新建的托管 Key 不再以明文进入 YAML，Provider API Key 和外部配置 client Key 仍可能以明文或环境变量引用存在，因而本轮不应宣称“配置文件不含任何秘密”。

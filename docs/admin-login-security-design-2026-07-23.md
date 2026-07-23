# Admin 登录安全设计

Status: implemented

Type: design

Last Updated: 2026-07-23

## 1. 目的与已确认决策

本设计为 ai-proxy 的 Admin 页面增加可选的账号密码登录能力和可配置的 Admin basePath。操作者显式开启安全管理参数后，Admin 不再限制为 loopback；任何来源都必须先完成登录并持有有效会话，才能访问配置的 Admin 页面及其 API。

本次确认的决策：

1. 新增 `server.admin_auth_enabled` 作为唯一开关，默认 `false`，保持现有本地开发与自动化脚本的访问行为不变。
2. 开关为 `true` 时，必须配置一个管理员账号和不可逆密码哈希；缺少、格式错误或弱参数均使进程在监听端口前启动失败。
3. 认证关闭时，Admin 保持当前仅 loopback 可访问的兼容行为；认证开启时取消该来源限制，支持 HTTP 或 HTTPS 登录。`admin_session_cookie_secure` 可选择使已认证会话仅随 HTTPS 请求发送。登录不改变数据 API 的客户端 Key 认证。
4. `server.admin_base_path` 定义 Admin 的 basePath，默认 `/admin`；页面、认证端点、业务 API、Cookie Path 和路由注册都从同一配置派生。
5. 采用服务端内存会话、HttpOnly Cookie 和 CSRF token，不采用 localStorage、URL token、HTTP Basic Auth 或仅前端判断。
6. 管理员为单账号，不引入用户表、角色、注册、找回密码、多实例共享会话或外部身份提供方。
7. 密码只接受 Argon2id PHC 哈希，不接受 YAML 明文密码、环境变量明文密码或可逆加密值。提供本地交互式哈希生成子命令以降低配置门槛。
8. 不新增 framework Module、Block 或 Initiator；认证中间件和会话存储归属既有 `application/adminapi`。

## 2. 现状与问题

当前 Admin Handler 对固定的 `/admin` 与 `/admin/api/*` 做 loopback 校验。读取接口无需认证；写接口仅检查 `X-AI-Proxy-Admin: 1`。该 Header 可以由任意本机进程或浏览器脚本伪造，因此它只适合表达浏览器请求意图，不能作为身份凭据。

Admin 页面能够修改 Provider 和客户端 API Key，并可读取用量数据。因此当本机存在其他用户、浏览器扩展、恶意本地进程，或通过 SSH 端口转发访问时，需要一个可配置的额外访问边界。仅在 HTML 中增加登录弹窗不能保护已知的 Admin API URL、CSV 导出和写接口，故认证必须首先在服务端路由入口生效。

本次范围：

- 配置开关、管理员账号、Argon2id 密码哈希与启动期校验；
- 登录、登出、会话状态、受保护的页面与 Admin API；
- Cookie 会话、CSRF、会话过期、登录限速和不泄露账号存在性的错误处理；
- Admin Web 登录页与认证失效交互；
- 配置示例、运行文档及回归测试。

非目标：

- 在应用内终止 TLS、信任 `X-Forwarded-For` / `X-Forwarded-Proto`，或按来源 IP 做访问控制；
- 给 OpenAI/Anthropic 数据 API 增加账号密码登录；
- 管理多个账号、角色/权限、审计事件持久化、MFA、SSO 或密码自助重置；
- 将会话跨进程重启、多实例或主机共享；
- 用 `X-AI-Proxy-Admin` 替代认证。

## 3. 配置合同

### 3.1 YAML 与环境变量

新增以下 `server` 配置；`server` 下的字段继续与顶层等价，以保持当前解析规则一致。

```yaml
server:
  # 默认 false；true 后 Admin 需要先登录。
  admin_auth_enabled: true

  # Admin 对外入口；默认 /admin。无论认证是否开启，所有 Admin URL 均以此为前缀。
  admin_base_path: /ops/ai-proxy

  # 单管理员账号，区分大小写，不应使用空白或控制字符。
  admin_username: ops-admin

  # 推荐用环境变量注入，不允许填写明文密码。
  admin_password_hash: ${AI_PROXY_ADMIN_PASSWORD_HASH}

  # true 时浏览器仅会在 HTTPS 请求中携带会话 Cookie；默认 false。
  admin_session_cookie_secure: true

  # 绝对会话有效期，默认 28800（8 小时），范围 300~86400 秒。
  admin_session_ttl_seconds: 28800
```

环境变量覆盖规则与现有 `server` 配置一致：

| 环境变量 | 作用 |
| --- | --- |
| `AI_PROXY_ADMIN_AUTH_ENABLED` | 覆盖 `admin_auth_enabled`。 |
| `AI_PROXY_ADMIN_BASE_PATH` | 覆盖 `admin_base_path`。 |
| `AI_PROXY_ADMIN_USERNAME` | 覆盖 `admin_username`。 |
| `AI_PROXY_ADMIN_PASSWORD_HASH` | 覆盖 `admin_password_hash`。只接受哈希，禁止明文。 |
| `AI_PROXY_ADMIN_SESSION_COOKIE_SECURE` | 覆盖 `admin_session_cookie_secure`。 |
| `AI_PROXY_ADMIN_SESSION_TTL_SECONDS` | 覆盖会话绝对有效期。 |

显式 `admin_auth_enabled: false` 时，账号、密码哈希与 TTL 不参与认证，也不得意外使认证启用。为避免配置漂移，若该模式下存在不为空的账号或密码哈希，启动记录一条不含敏感值的 warning；不作为错误。

启用时的校验：

- `admin_base_path` 默认 `/admin`。它必须以单个 `/` 开始、长度不超过 128、不以 `/` 结束，并且每个路径段只能使用 RFC 3986 的未保留字符；禁止空路径、`//`、`.`、`..`、`%`、`?` 和 `#`。例如 `/ops/ai-proxy` 合法，`admin`、`/admin/`、`/ops/../admin` 非法。
- `admin_username` 长度为 1~64，禁止前后空白、控制字符和 `:`；账号区分大小写，登录比较使用常量时间比较。
- `admin_password_hash` 必须是唯一允许格式的 Argon2id PHC 字符串，且只允许本设计声明的参数：`m=65536,t=3,p=1`、salt 至少 16 bytes、输出至少 32 bytes。
- `admin_session_ttl_seconds` 为 300~86400 的整数；未配置使用 28800。
- 配置解析、校验错误和日志都不得回显密码、哈希原文或环境变量的展开结果。

管理员账号、密码哈希或开关的热更新只在新配置完整校验且 `RuntimeConfig.UpdateConfig` 成功激活后生效。Admin Handler 必须将现有的配置激活调用收口为一个内部方法：先激活，再把新的 Admin 认证配置应用到 `SessionStore`。上述配置发生变化时，该方法清空全部内存会话和登录限速记录：旧 Cookie 随即失效；新配置关闭认证时恢复 loopback-only 行为。任何将来新增的 Admin 配置写入路径都必须复用该方法。

`admin_base_path` 是**启动期路由配置**，变更后必须重启进程才生效；不得尝试通过热更新同时撤销旧路由、注册新路由或让同一 Cookie 在两个 Path 下继续有效。Provider 与客户端 Key 的既有 Admin 写入路径不会修改该字段。

### 3.2 密码哈希生成

实现 `ai-proxy admin password-hash` 子命令。它只从交互式 TTY 两次读取密码（关闭回显），确认一致后输出一行 Argon2id PHC 字符串到 stdout；密码不得作为命令行参数、环境变量、日志或错误消息的一部分。

推荐操作方式：

```bash
ai-proxy admin password-hash
export AI_PROXY_ADMIN_PASSWORD_HASH='argon2id$v=19$m=65536,t=3,p=1$...'
```

实现采用 `golang.org/x/crypto/argon2`，随机 salt 使用 `crypto/rand`。验证时解析 PHC 后使用 `subtle.ConstantTimeCompare` 比较派生结果。哈希参数将来如需升级，必须新增兼容校验和“登录成功后提示重新生成”的迁移方案，不能静默降低现有参数。

## 4. 认证与会话模型

### 4.1 请求处理顺序

Admin Handler 的入口按以下顺序处理：

```text
请求
  -> 认证开关判断
       -> 关闭：执行 loopback 校验，再沿用既有路由合同
       -> 开启：不按来源 IP 拒绝；仅放行 auth/login、auth/logout、auth/session 与登录页面
                其它 <admin_base_path> 与 <admin_base_path>/api/* 验证 Cookie 会话
  -> 对认证后的变更请求验证 CSRF
  -> 既有业务路由与配置事务
```

认证开启时，所有来源一律进入登录流程，不保留 loopback 特权，也不基于 `RemoteAddr`、`X-Forwarded-For` 或 CIDR 决定是否可以跳过认证。认证关闭时，任意远程请求返回 `403 admin access is loopback-only`；认证端点与页面均不得绕过该限制。

### 4.2 内存会话

`admin.Handler` 持有一个并发安全的 `SessionStore`。登录成功后：

1. 使用 `crypto/rand` 生成 32 bytes 的会话 ID 和 32 bytes 的 CSRF token，以 base64 Raw URL 编码；
2. 服务端保存 `{sessionID, username, issuedAt, expiresAt, csrfToken}`；
3. 仅将不透明 session ID 写入 Cookie；CSRF token 通过会话接口的 JSON 返回给同源页面；
4. 会话达到绝对 TTL 即失效，不因访问延长；每次读取和写入均检查过期；
5. 登出、认证配置成功热更新及进程重启均删除会话。

会话存储上限为 64 个活动会话。达到上限时，登录请求删除已过期会话；仍满时拒绝新登录并返回 503，不驱逐仍活跃的操作者。每次登录、验证和登出机会性清理过期条目，无需后台 goroutine 或新的生命周期 owner。

Cookie 合同：

```text
Name:     ai_proxy_admin_session
Path:     <admin_base_path>
HttpOnly: true
SameSite: Strict
Max-Age:  与 session TTL 相同
Secure:   <admin_session_cookie_secure>
```

`Secure` 的值由 `admin_session_cookie_secure` 决定，默认 `false`。开启时浏览器仅会在 HTTPS 请求中携带会话 Cookie，因而已认证 Admin 只能通过 HTTPS 使用；关闭时兼容 HTTP 和 HTTPS。应用不读取或信任任何 forwarded header。生产环境推荐 HTTPS；认证关闭时不创建此 Cookie，仍可通过 `http://127.0.0.1` 使用既有本地 Admin。不会将 token 放入 `localStorage`、`sessionStorage`、URL、hash、页面持久化状态或日志。

### 4.3 CSRF 与登录滥用防护

启用认证时，所有状态变更请求（Provider、Client API Key、探针、登出）同时要求：

- 合法的未过期会话 Cookie；
- `X-AI-Proxy-CSRF: <session csrf token>`；
- 对浏览器会发送 `Origin` 的请求，`Origin` 必须精确等于 `http://Host` 或 `https://Host`；缺失 Origin 的非浏览器客户端允许继续使用 CSRF token。代理部署时必须保留外部 `Host`，应用不读取 `X-Forwarded-Proto`。

`X-AI-Proxy-Admin: 1` 在安全模式下不再构成安全条件；为了兼容现有 Web 代码和自动化，在本轮保留但不校验其值。认证关闭时，既有写接口继续要求该 Header，避免无意扩大原有攻击面。

登录失败使用统一错误 `invalid username or password`，不区分账号不存在、密码错误或哈希异常。按 `RemoteAddr` 的实际对端地址实施内存限速：连续 5 次失败后，15 分钟内返回 `429` 和 `Retry-After`；成功登录清除该地址的失败记录。反向代理部署中不信任 forwarded header，因此多个用户可能共享代理地址的限速桶；这是一项可预期的保守限制。限速状态随进程重启或认证配置热更新清空。响应和日志不得包含提交的账号、密码、Cookie 或 CSRF token。

## 5. HTTP 合同

所有 JSON 响应继续使用 `Cache-Control: no-store`。认证开启后，受保护 API 未认证或会话失效返回 JSON `401`，而不是重定向 HTML；这样前端可以明确跳回登录页，脚本也不会误把 HTML 当 JSON。

### 5.1 登录页与会话查询

| 路径 | 方法 | 认证 | 行为 |
| --- | --- | --- | --- |
| `<basePath>/login` | GET, HEAD | 否 | 返回仅含账号、密码和提交按钮的登录页；已登录时重定向 `<basePath>/`。 |
| `<basePath>/api/auth/login` | POST | 否 | 校验账号密码，成功创建会话与 Cookie。 |
| `<basePath>/api/auth/session` | GET | 是 | 返回当前登录状态和 CSRF token。 |
| `<basePath>/api/auth/logout` | POST | 是 + CSRF | 删除会话并清除 Cookie。 |

以下以 `<basePath>` 表示经校验的 `admin_base_path`，例如 `/ops/ai-proxy`；默认值仍为 `/admin`。`GET <basePath>` 与 `GET <basePath>/` 在认证开启且未登录时，返回 `303 Location: <basePath>/login`；认证开启且已登录时返回原有控制台。`HEAD <basePath>` 未登录时返回 401，不重定向。未认证的 `<basePath>/api/*` 返回：

```json
{
  "error": {
    "code": "admin_authentication_required",
    "message": "admin login is required"
  }
}
```

登录请求为严格 JSON（1 MiB 上限、拒绝未知字段与多个 JSON 值）：

```http
POST <basePath>/api/auth/login
Content-Type: application/json

{"username":"ops-admin","password":"<password>"}
```

成功返回 `200`、设置 Cookie，并返回：

```json
{
  "username": "ops-admin",
  "expires_at": "2026-07-23T16:00:00Z",
  "csrf_token": "<opaque token>"
}
```

密码字段不得出现在浏览器 toast、错误详情、访问日志或 trace。登录失败返回 `401`；触发限速返回 `429`。账户名仅在认证成功后的 `session` / `login` 响应中出现。

`GET <basePath>/api/auth/session` 返回相同的 `username`、`expires_at`、`csrf_token`。它不返回 session ID、密码哈希或任何配置凭据。登出成功返回 `204`，同时以相同 Path 和 `Max-Age=0` 覆盖 Cookie。

### 5.2 既有 Admin API 的变化

安全模式开启时，下列所有现有 Admin API 都必须走统一认证与 CSRF 中间件，不能按“只读”遗漏导出或筛选接口：

- `<basePath>/api/providers` 及 Provider probe；
- `<basePath>/api/client-api-keys` 及创建、启停、轮换、删除；
- `<basePath>/api/usage/dashboard`、`events`、`filter-options`、`export.csv`；
- 后续增加的任意 `<basePath>/api/**` 路径。

受保护 GET/HEAD 仅需会话；POST、PUT、PATCH、DELETE 还需 CSRF。认证关闭时合同不变：读取可访问，写入仍需 `X-AI-Proxy-Admin: 1`。

## 6. Web 管理端

`web/admin/index.html` 增加独立登录视图或拆分为最小 `login.html` 嵌入资源。Handler 渲染页面时以安全 JSON 字面量注入唯一的 `window.__AI_PROXY_ADMIN_BASE_PATH__`；前端基于它构造所有页面跳转和 API URL，不能保留硬编码 `/admin`。登录页：

- 只包含用户名、密码、提交按钮和通用错误提示；密码输入框使用 `type=password`、`autocomplete=current-password`；
- 提交仅调用同源 `<basePath>/api/auth/login`，不接受 `return_to` 等可控跳转参数；
- 成功后使用 `location.replace('<basePath>/')`，避免把登录页留在历史记录；
- 失败、429、网络错误均不显示服务端敏感细节；429 显示 `Retry-After` 倒计时；
- 设置 `Referrer-Policy: no-referrer`，并沿用 `Cache-Control: no-store` 和严格 CSP。

控制台启动时调用 `<basePath>/api/auth/session` 取得 CSRF token；每个状态变更请求附加 `X-AI-Proxy-CSRF`。任意请求收到 `401` 时，内存中的 CSRF token 立即清除，并用 `location.replace('<basePath>/login')` 返回登录页。登出入口使用 `POST <basePath>/api/auth/logout`，成功后同样 replace。

页面不得把账号、密码、Cookie、CSRF token 或响应对象写到 local/session storage、URL、console、可复制 Toast 或错误详情。Web 请求默认 `credentials: same-origin`。

## 7. 代码落点与职责

| 位置 | 变更职责 |
| --- | --- |
| `internal/pkg/aiproxyconfig/config.go` | 添加 `AdminAuthConfig` 至 `Config`，解析 YAML/环境变量、默认 basePath/TTL、basePath 规范化、Argon2id PHC 格式和启用关联校验；不输出敏感内容。 |
| `internal/pkg/aiproxyconfig/config_test.go` | 覆盖关闭兼容、开启缺参、basePath 合法/非法形态、参数边界、非法 PHC、环境变量覆盖与热更新配置差异。 |
| `internal/modules/application/adminapi/service/admin/auth.go`（新增） | `SessionStore`、可选 Secure Cookie、Argon2 验证、登录限速、CSRF/Origin 校验和按认证开关决定的来源 gate。 |
| `internal/modules/application/adminapi/service/admin/handler.go` | 在路由分派前执行 auth gate（关闭时 loopback-only，开启时不限制来源）；从 `admin_base_path` 路由认证与业务端点；向 HTML 安全注入 basePath；将 `RuntimeConfig.UpdateConfig` 收口为“成功激活后应用认证设置”的内部方法。 |
| `internal/modules/application/adminapi/service/admin/client_keys.go` | `requireAdminWrite` 改为复用认证上下文与 CSRF gate，不能绕过全局认证。 |
| `internal/modules/application/adminapi/service/admin/routes.go` | 基于启动快照注册 `<basePath>` 与 `<basePath>/**`，不再硬编码 `/admin`；basePath 变更要求重启。 |
| `web/admin/index.html`、`web/embed.go` | 登录、会话初始化、CSRF header、401 跳转、登出和安全响应头。 |
| `cmd/ai-proxy` / `internal/services/aiproxy` | 增加受限的交互式 `admin password-hash` 子命令，不启动 HTTP gateway。 |
| `config.example.yaml`、`docs/configuration.md`、`docs/operations.md`、`README.md` | 补充开关、哈希生成、启用/禁用行为和本地边界说明。 |

不新增 Module/Block 的理由：账号验证、会话和 Web 路由均是 Admin HTTP application service 的局部职责，没有独立启动资源或跨模块业务 owner。配置仍由既有 Config Runtime 统一校验并原子激活。

## 8. 实施顺序

1. 在配置层实现 `AdminAuthConfig`、环境变量覆盖、严格 PHC 解析和启动期测试；同时添加密码哈希生成命令。
2. 编写独立、可注入 clock/random source 的 `SessionStore` 与 Argon2/限速单测，确保比较与 token 生成不可预测。
3. 在 Admin Handler 添加认证路由与全局 gate；把现有所有路由纳入同一 gate，并保留关闭模式回归测试。
4. 为配置热更新接通会话清理，验证开关、账号与密码哈希变更不会保留旧会话。
5. 更新 Web 控制台，接入 CSRF 和 401 处理；使用浏览器级或 `httptest` cookie jar 覆盖完整登录流。
6. 更新示例和运维文档，执行 `go test ./...`、格式检查和人工 smoke test。

## 9. 验收标准与测试矩阵

| 场景 | 期望结果 |
| --- | --- |
| 默认未配置开关 | loopback 默认 `/admin`、读取 API 和现有写 Header 行为完全兼容；远程来源为 403。 |
| 认证开启的任意来源 | 不因 `RemoteAddr` 或 forwarded header 被拒绝；未登录时页面跳登录、API 返回 401，登录后可正常访问。 |
| 自定义 basePath `/ops/ai-proxy` | 仅该前缀下页面、登录、API、CSV 与 Cookie 生效；旧 `/admin` 返回 404；所有前端请求和跳转不含硬编码 `/admin`。 |
| 非法 basePath 或运行中变更 basePath | 前者启动失败；后者明确要求重启，不产生双路由或残留 Cookie。 |
| 开启但账号/哈希不完整或哈希非法 | 服务在监听端口前启动失败，错误不含敏感值。 |
| 未登录访问 `<basePath>/` | 303 到固定 `<basePath>/login`；不能由 query 影响跳转目的地。 |
| 未登录访问任意 Admin API 或 CSV | 401 JSON，且不返回页面、Provider、Key 或 usage 数据。 |
| 正确账号密码 | 返回 HttpOnly SameSite=Strict Cookie、session JSON 和 CSRF token；可访问所有读取 API。 |
| 错误账号或密码 | 相同 401 body 与近似处理路径；连续失败触发 429。 |
| 有会话但无效/缺失 CSRF 的 POST/PUT/PATCH/DELETE | 403；Provider 保存、Key 轮换和 probe 均不发生副作用。 |
| 有效会话+CSRF | 既有 Admin 操作成功；认证关闭时仍要求原有写 Header。 |
| 认证开启且 `admin_session_cookie_secure=false` | HTTP 与 HTTPS 均可建立会话。 |
| 认证开启且 `admin_session_cookie_secure=true` | 响应 Cookie 带 `Secure`；浏览器仅在 HTTPS 请求中携带会话。 |
| 过期、登出、重启、成功热更新账号/密码/开关 | 旧 Cookie 失效，后续 API 为 401。 |
| 仅热更新 Provider 等无关配置 | 已登录会话保持有效。 |
| 日志、错误、HTML、API、DuckDB | 不出现密码、密码哈希、session ID 或 CSRF token。 |
| 并发登录、会话上限与过期清理 | 不出现 data race；超限不驱逐未过期会话；过期条目可被回收。 |

## 10. 运维说明与风险边界

启用方式是在部署前生成密码哈希、通过受保护的环境变量或权限为 `0600` 的配置文件注入，并把 `admin_auth_enabled` 设为 `true`。对外可通过 HTTP 或 HTTPS 访问；生产环境应使用 HTTPS，并可将 `admin_session_cookie_secure` 设为 `true`。代理应转发到 ai-proxy 并保留外部 `Host`，应用不使用任何 forwarded header 作身份或协议判断。`admin_base_path` 需要在代理路径规则、对外链接与配置中保持一致，变更后重启进程。修改密码哈希会在成功配置激活时立即使所有 Admin 浏览器登出。

此能力允许远程访问受登录保护的 Admin，但不替代 TLS、反向代理访问控制、主机账户隔离、文件权限或恶意进程防护。尤其是 Provider API Key、客户端 API Key 哈希、Admin 密码哈希、DuckDB 文件和配置文件仍必须由运行用户保护。若未来需要可信代理源 IP、CIDR 白名单、多用户权限或持久化会话，应另立设计，重新评审审计日志、账户治理与多实例一致性。

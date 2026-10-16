# AgentHub 设计规格（定稿）

日期：2026-10-16。本文档是 v1 施工蓝图，所有决策已拍板。

## 1. 产品定位

多用户 AgentDock 汇聚网关：解决 AgentDock 无公网 IP 无法直连的问题。用户的 AgentDock 通过出站 WebSocket 注册到 hub，用户的 Agent 平台以 MCP 方式接入 hub，调用被路由到该用户自己的节点。**Hub 只做"带身份的路由器"**——工具/技能的编排语义归 Agent 平台，hub 不做 Recall、不做 Workflow 模板、不做全局工具目录。

角色分两种：

- **管理员**：建号、禁用用户、重置密码、看全局统计与元数据调用流水。
- **普通用户**：登录、生成配对码、管理自己的节点、查看/重置自己的 MCP Token、看自己的调用记录。

## 2. 总体架构

```
AgentDock ──出站 WSS──▶ ┌─────────┐ ◀──MCP (Streamable HTTP)── Agent 平台
  /v1/nodes/pair        │ AgentHub │      单一 /mcp，Bearer token 即身份
  /v1/nodes/connect     │  (Go)    │
                        └─────────┘
                     SQLite 单文件，反代终结 TLS
```

- 协议直接 import `github.com/uvwt/agentdock-protocol`（`PairRequest/PairResponse`、`Message` 封套、`node.hello/node.ready`、`tool.invoke/tool.result/tool.cancel`、heartbeat、`PublicURL`）。
- Bridge/连接管理层参考 NexusDock `internal/agentdock` 实现，拷贝改造（其 internal 包不可外部引用）。
- Fleet 合并**完整照搬** NexusDock：语义哈希归一化、兼容变体合并、variants 历史集合、调用前契约复核（对应 NexusDock `published_contracts.go` + `contracts_bridge.go` + `tool_contract.go` 约 800 行）。

## 3. 对外协议面

### 3.1 节点侧（AgentDock 零改动）

- `POST /v1/nodes/pair`：一次性配对码换 device token，token 只存哈希。
- `WS /v1/nodes/connect`：`Authorization: Bearer <device_token>`，连上后 `node.hello` → `node.ready`（带 PublicURL）。
- **同一 device_id 新旧连接竞态：新连接赢**，服务端主动关闭旧连接。
- **节点禁用** = 拒绝新连接 + 踢掉现有连接（保留记录，可恢复）；**节点删除** = 吊销 token + 踢线，不可逆，回来必须重新配对。
- **协议版本门槛**：`node.hello` 的 `protocol_version` 低于 hub 最低版本要求 → 拒连，错误信息给出升级指引；高于 hub 认识的版本按已知最新处理。

### 3.2 平台侧（MCP）

- **单一 `/mcp` 路径**，仅 Streamable HTTP transport，`Authorization: Bearer <用户 MCP Token>` 定位用户。
- token 错误返回 401 + `WWW-Authenticate`，认证失败按 IP 限流。
- 每用户暴露：该用户所有**在线**节点的工具（**裸名**，用户命名空间内经 fleet 合并保证一致）+ 中心工具 `nodes_context`（返回节点列表：node_id、名称、版本、能力、在线状态）。
- 每个透传工具的 inputSchema 强制注入必填 `node_id`（照抄 NexusDock `nodeInputSchema`），调用时校验目标节点提供该工具后定向转发。
- "显式指定节点" = 请求里的 `node_id` 参数，由平台根据 `nodes_context` 结果选择。

### 3.3 运行时语义（定稿）

- **节点离线**：节点断线即从 MCP tool list 撤下其工具（照搬 NexusDock `ReconcileNames` 语义）；对离线节点的调用立即返回结构化错误 `NODE_OFFLINE`（retryable=true）。**不做排队、不做离线消息缓存**。
- **请求超时**：`tool.invoke` 统一默认 120 秒（config 常量），超时向平台返回结构化超时错误。
- **取消传播**：MCP 侧取消透传为协议 `tool.cancel` 发给节点。
- **并发上限**：每用户/每节点并发在途请求上限 16，超限立即 429。
- **消息大小上限**：单条消息 10MB，超限报错（不做 artifact 机制，文件类内容由平台侧处理）。

## 4. 功能清单

### 4.1 用户侧（3 页面，纯中文）

1. **登录**：首登强制改密。
2. **我的节点**：在线状态、生成配对码（10 分钟有效、单次、绑定本人）、禁用/删除节点、MCP Token 查看/重置（重置后旧 token 立即失效）、MCP endpoint 地址展示。
3. **调用记录**：时间、节点、工具名、成败、耗时、错误码。

### 4.2 管理员侧（2 页面）

1. **用户管理**：列表、创建（管理员设初始密码，用户名用邮箱形态）、禁用、重置密码。
2. **全局概览**：每用户节点数/近期调用量统计 + 全局最近调用流水（仅元数据）。

### 4.3 管理员语义

禁用用户 = 立即踢掉该用户全部节点 WS + MCP token 即刻 401 + 配对码作废。管理员看统计与元数据流水，不看用户节点明细和调用内容。

## 5. 数据模型（SQLite，WAL，单文件）

- `users`（邮箱形态用户名、密码哈希、must_change_password、status）
- `user_sessions`（Web 会话，存哈希，绝对过期）
- `user_mcp_tokens`（用户级 MCP token，存哈希，可重置即失效）
- `nodes`（含 **owner_user_id**——与 NexusDock 的关键差异）
- `pairing_codes`（哈希、过期、单次、**绑定用户**）
- `published_tool_contracts` + `published_tool_variants`（**按用户分区**，tool_name 在用户内唯一）
- `tool_contracts` / `bridge_capabilities`（节点级 hello 快照）
- `audit_events`（append-only，trigger 防改删；actor = user/device/admin）
- `invoke_logs`（仅元数据：时间/用户/节点/工具/成败/耗时/错误码，30 天滚动 + 条数上限）
- `login_throttles`（账号+IP 限流，同时用于 `/v1/nodes/pair` 的按 IP 配对码爆破防护：10 次/分钟，连续失败递增延迟）

备份策略：停机复制 SQLite 单文件，不做在线备份。

## 6. 安全模型

- 管理员由本地 CLI `agenthub admin` 初始化/重置，不走环境变量密码。
- 三类秘密独立：Web session、每用户 MCP token、每节点 device token——全部只存哈希。
- 配对码绑定用户 + 短时 + 单次；AgentDock 永不接触用户凭据。
- 默认配额（config 常量，不做界面可调）：每用户节点数上限 10、MCP 调用 60 req/min（429）、每用户/每节点在途请求 16、配对码 10 分钟、`/v1/nodes/pair` 按 IP 10 次/分钟、登录限流（账号+IP）、WS 心跳超时踢线、单条消息 10MB、invoke 超时 120 秒。
- 调用记录不记 arguments/result——hub 是中继不是落地，避免敏感参数沉淀。

## 7. 部署与交付

- 单二进制，`go:embed` 内嵌 Web 产物（React + Vite，纯中文，不做 i18n）。
- TLS 由反代终结，hub 只听 HTTP；`PublicURL` 是唯一必填部署配置。
- 观测：结构化 slog（连接/断开、invoke 成败、认证失败）+ `/healthz`；协议 `traceparent` 字段透传（零成本）；暂不做 metrics/OTel。
- 仓库：`github.com/uvwt/agenthub`，数据目录 `HUB_DATA_DIR` 默认 `~/.agenthub`。

## 8. v1 范围边界（明确不做）

Recall、Workflow 模板、Skill/MCP 编排、i18n、邮件通知、自助注册、每用户路径 endpoint、Prometheus/OTel、管理员内容级审计、artifact 下载、离线排队。将来若加 Recall/Workflow，数据模型已按用户分区（owner_user_id 贯穿），扩展点现成。

## 9. 后续规划：Hub 作为 OAuth 授权服务器（MCP client 接入 /mcp）

**状态：规划中，v1 未实现。** 触发条件：出现"自带 OAuth 能力的外部 MCP client"（Claude Desktop、ChatGPT connector 等通用客户端，或第三方系统）需要接入用户工具面的真实需求时启动。现有静态 MCP Token 保留，两者长期共存、互不影响——与 NexusDock 的双轨模式一致（见其 `internal/httpx/oauth.go`，可整体参考）。

### 9.1 定位与范围

- Hub 充当 **OAuth 2.0 授权服务器**，保护资源为 `/mcp`（每用户的 MCP 工具面）。
- 服务对象：支持 MCP 规范认证（RFC 8414 服务器元数据 + RFC 9728 受保护资源元数据 + 授权码流程 + PKCE）的 client。
- **不做** client credentials 机器流（自有系统继续用静态 Token）；**不做**节点侧认证改动（节点→hub 保持 device token，设备无人值守，授权码流程不适用，此方向永不引入 OAuth）。
- 与静态 Token 的关系：同一 `/mcp` 路径，`Authorization: Bearer` 头同时接受两种凭证——OAuth 签发的 access token 与用户静态 MCP Token。认证中间件先查静态 token 哈希表，未命中再走 OAuth token 校验。

### 9.2 协议面（需要新增的 endpoint）

```
GET  /.well-known/oauth-protected-resource/mcp   RFC 9728 资源元数据（401 响应里已预留 WWW-Authenticate 指向此处）
GET  /.well-known/oauth-authorization-server     RFC 8414 服务器元数据（issuer、authorize/token/registration endpoint）
GET  /oauth/authorize                            授权端点（浏览器跳转，用户登录 + 同意页）
POST /oauth/authorize                            同意提交
POST /oauth/token                                token 端点（authorization_code + refresh_token 两类 grant）
POST /oauth/register                             动态客户端注册（DCR，MCP 生态惯例；也可做成界面手动注册，DCR 优先）
GET  /oauth/consent                              （若不做 DCR）已注册 client 列表/撤销，并入用户设置页即可
```

### 9.3 数据模型（新增表，沿用现有 SQLite 与 user_version 迁移机制）

```sql
oauth_clients        (id PK, owner_user_id FK→users, client_name, redirect_uris_json,
                      token_endpoint_auth_method CHECK(='none'), created_at, revoked_at)
oauth_authorization_codes (code_hash UNIQUE PK, client_id FK, owner_user_id FK,
                      redirect_uri, code_challenge, code_challenge_method, expires_at, used_at)
oauth_grants         (id PK, client_id FK, owner_user_id FK, scope='mcp',
                      access_token_hash UNIQUE, refresh_token_hash UNIQUE,
                      expires_at, revoked_at, created_at)
oauth_refresh_token_history (refresh_token_hash PK, replaced_by_hash, rotated_at)  -- 轮换审计
```

关键点：**grant 归属 owner_user_id**——token 解析出用户后，工具面装配逻辑与静态 token 完全共用（`registerUserTools` / `callNodeTool` 无需任何改动）。`oauth_clients` 带 owner 是与 NexusDock 的差异：client 属于某个用户，注册时即绑定，防止跨用户 client 伪造。

### 9.4 实现要点（照搬 NexusDock 的部分）

- **PKCE 强制**：`code_challenge_method` 只接受 `S256`，public client 无 client secret。
- **token 形态**：不透明 token（随机串）+ 哈希落库，不签 JWT——与现有三类秘密的哈希存储模型一致，省掉密钥管理。access token 有效期建议 1h，refresh token 30d，刷新即轮换（旧 refresh token 进 history 表，重放检测→撤销整条 grant）。
- **同意页**：复用 Web 会话认证（未登录先跳登录），展示 client 名称与"将获得该用户全部节点工具的调用权限"，允许/拒绝。首次同意建 grant，静默续期。
- **401 处理**：现有 `writeMCPError` 的 `WWW-Authenticate` 已带 `resource_metadata="/.well-known/mcp"`，实现时保持该头并将路径改为真实的 RFC 9728 端点。
- **参考实现**：NexusDock `internal/httpx/oauth.go`（约 800 行，含同意页模板与 token 端点）、`mcp_oauth_callback.go`（回调中转，若将来做节点侧第三方 MCP 的回调转发可一并参考）。拷贝改造时删除与 Nexus Apps（recall/workflow）耦合的 scope 逻辑，scope 固定 `mcp`。

### 9.5 工作量估计与验收

约 2-3 天：元数据端点 + DCR 半天，authorize/token + PKCE 一天，同意页 + 客户端管理界面半天，SDK 真实 client（go-sdk 的 `mcp.NewClient` 走 OAuth round-trip）e2e 半天。验收标准：Claude Desktop 或任一标准 MCP client 配置 hub URL 后，无手工 token 即可完成授权并调用到对应用户的工具；重放 refresh token 导致整条 grant 撤销；撤销后该 client 的 token 立即 401。

### 9.6 明确不采用的方向（避免翻旧账）

- 节点→hub 方向的任何 OAuth（见 9.1）。
- JWT / 对称签名 token：引入密钥轮换问题，与 hub"秘密只存哈希"的模型冲突。
- 每用户独立 issuer/多租户隔离的授权服务器：单一 issuer，client 按 owner_user_id 分区即可。

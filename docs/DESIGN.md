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

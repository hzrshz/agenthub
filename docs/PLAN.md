# AgentHub v1 实施计划

按阶段推进，每阶段产出可验证的成果；`[P]` 表示有 Go 单测，`[B]` 表示有浏览器手工验证路径。

## 阶段 1：项目骨架与数据层

- [ ] 1.1 Go module 初始化（github.com/uvwt/agenthub）、目录结构（cmd/agenthub、internal/{config,core,auth,nodes,bridge,mcp,httpx,web}）、Makefile（check/ci/web/contracts 对标 nexusdock 惯例）
- [ ] 1.2 config：HUB_DATA_DIR、listen 地址、PublicURL（必填校验）、全部配额常量（节点 10、60 req/min、在途 16、配对码 10min、pair 限流 10/min、心跳、消息 10MB、invoke 超时 120s）
- [ ] 1.3 SQLite schema（WAL）：users、user_sessions、user_mcp_tokens、nodes、pairing_codes、published_tool_contracts+variants（按 owner_user_id 分区）、tool_contracts、bridge_capabilities、audit_events（append-only trigger）、invoke_logs、login_throttles；PRAGMA user_version 迁移机制（对标 nexusdock core）
- [ ] 1.4 存储层与单测：各表 CRUD、哈希存储（session/mcp token/device token/pairing code）、审计触发器、invoke_logs 滚动清理（30 天+条数上限）[P]

## 阶段 2：账号体系与 Web 基础

- [ ] 2.1 `agenthub admin` CLI：初始化/重置管理员（本地交互，不走环境变量密码）
- [ ] 2.2 管理员/用户认证：登录（限流：账号+IP）、Web session（哈希、绝对过期）、首登强制改密、登出 [P]
- [ ] 2.3 管理员用户管理 API：创建（邮箱形态用户名+初始密码）、禁用（联动：踢 WS+token 失效+配对码作废）、重置密码 [P]
- [ ] 2.4 用户 MCP Token：查看/重置（旧 token 立即失效），token→用户定位中间件 [P]
- [ ] 2.5 Web 前端骨架：React+Vite、纯中文、go:embed 嵌入、登录页 [B]

## 阶段 3：节点注册与 Bridge

- [ ] 3.1 `POST /v1/nodes/pair`：用户侧生成配对码（绑定用户、10min、单次）→ 兑换 device token（只存哈希）；按 IP 爆破限流 [P]
- [ ] 3.2 `WS /v1/nodes/connect`：Bearer 认证、协议版本门槛、node.hello→node.ready（PublicURL）、心跳超时踢线、同 device_id 新连接赢（踢旧连接）[P]
- [ ] 3.3 节点管理 API：用户侧列表/禁用/删除（删除=吊销 token+踢线）；每用户节点数上限 [P]
- [ ] 3.4 hello 快照持久化：tool_contracts、bridge_capabilities、capabilities [P]
- [ ] 3.5 `agentdock nexus pair` 真机联调：AgentDock 配对上线、断线重连、竞态 [B]

## 阶段 4：Fleet 工具合并（照搬 NexusDock）

- [ ] 4.1 语义哈希与契约比较：tool_contract.go 语义 schema 归一化、ToolContractHash [P]
- [ ] 4.2 PublishedToolBridge：ObserveNodeHello、reconcile（按 owner_user_id 分区）、variants 合并、retire、持久化先行 [P]
- [ ] 4.3 调用前契约复核：ToolContractMismatch 结构化差异 [P]

## 阶段 5：MCP 网关

- [ ] 5.1 `/mcp` Streamable HTTP：token 认证中间件（401+WWW-Authenticate、IP 限流）、用户速率限制（60/min→429）[P]
- [ ] 5.2 工具注册：按用户聚合在线节点工具（裸名）、inputSchema 注入必填 node_id、节点离线撤下工具（ReconcileNames 语义）[P]
- [ ] 5.3 中心工具 nodes_context：节点列表（node_id/名称/版本/能力/在线状态）[P]
- [ ] 5.4 调用链路：capability 校验→定向 tool.invoke 转发→tool.result 回传；NODE_OFFLINE 立即错误；120s 超时；MCP 取消透传 tool.cancel；并发在途 16 上限；单条消息 10MB 上限；traceparent 透传 [P]
- [ ] 5.5 MCP 客户端端到端联调：list tools→带 node_id 调用→离线错误→取消 [B]

## 阶段 6：调用记录与界面完成

- [ ] 6.1 invoke_logs 记录（仅元数据）+ 用户侧查询 API + 管理员全局流水/统计 API [P]
- [ ] 6.2 用户界面：我的节点页（在线状态/生成配对码/禁用/删除/Token 管理/endpoint 展示）[B]
- [ ] 6.3 用户界面：调用记录页 [B]
- [ ] 6.4 管理员界面：用户管理页 + 全局概览页 [B]

## 阶段 7：加固与交付

- [ ] 7.1 结构化 slog 全覆盖（连接/断开、invoke 成败、认证失败）+ /healthz
- [ ] 7.2 安全自测：越权访问（用户 A 的 token/node_id 触达 B 资源一律 404/403）、禁用即时性、配对码复用拒绝、路径穿越（无文件面，确认无）、消息上限 [P]
- [ ] 7.3 e2e 测试：管理员建号→用户配对→MCP 调用全链路（对标 nexusdock tests/e2e）[P]
- [ ] 7.4 README（部署：反代 TLS+PublicURL+备份说明）、make ci 全绿、单二进制产物验证

## 验收标准（对照设计）

全部完成后逐条核对 docs/DESIGN.md：协议面 3.1-3.3 每条语义有测试或联调记录；功能 4.1/4.2 每个页面可用；安全 6 中每类秘密只存哈希、每条配额生效；交付 7 单二进制可跑。

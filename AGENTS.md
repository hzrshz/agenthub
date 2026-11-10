# AgentHub 开发约束

## 产品边界

AgentHub 是多用户 AgentDock 汇聚网关：用户的 AgentDock 通过 Device Token 主动建立出站 WebSocket（不要求 AgentDock 具备公网入口），用户的 Agent 平台以 MCP（Streamable HTTP）接入，调用按用户路由到该用户自己的节点。Hub 只做"带身份的路由器"：不做 Recall、Workflow 模板、Skill/MCP 编排，不恢复离线排队或 artifact 下载。多租户是根本属性：nodes/pairing_codes/published_tool_contracts 等全部按 owner_user_id 分区，任何用户侧查询不得跨 owner 访问，跨用户资源一律返回 NotFound（不泄露存在性）。节点侧认证保持 Device Token 模式，不引入 OAuth（规划中的 OAuth 仅用于 MCP client 接入 `/mcp`，见 docs/DESIGN.md §9）。

## 代码原则

- 修改前先理解现有目录、数据模型、接口契约、错误处理和测试方式。
- 主流程保持连贯，优先具体类型和清楚的数据流；不要为形式上的分层提前增加 interface、helper 或通用抽象。
- 动态 JSON 只停留在 HTTP/MCP 边界，进入核心流程后尽快转成明确结构。
- 错误必须显式返回并包含定位上下文，不吞错，不只记录日志后继续。
- 非显然业务约束、兼容原因和安全边界使用中文注释说明"为什么"。
- 测试描述真实业务行为，优先覆盖状态变化、错误路径、安全边界（跨租户隔离、禁用即时性、token 失效）和曾经出现过的问题。

## 认证与秘密

- 三类秘密（Web session / 每用户 MCP Token / 每节点 Device Token）与配对码、口令全部只存哈希（SHA-256 或 argon2id），任何接口不得回显秘密明文；Token 明文只在签发响应中出现一次。
- 管理员通过本地 `agenthub admin init/reset` 初始化或重置，不走环境变量密码；授权服务器（OAuth）若实现，仅覆盖 MCP client 接入，不得影响静态 MCP Token 与 Device Token 两条既有路径。
- 配对码绑定用户、短时、单次；用户禁用 = 节点连接断开 + MCP Token 失效 + Web 会话吊销 + 配对码作废，全部必须即时生效。
- 调用记录只存元数据（时间/用户/节点/工具/结果/耗时/错误码），工具 arguments/result 绝不落库。
- 配额与安全常量集中在 internal/config（每用户节点数、速率、在途上限、消息上限等），不做运行时可调。

## 等保二级红线（发布门禁）

依据 docs/LEVEL2-DEV-REQUIREMENTS.md §3（等保 2.0 高风险判定指引），以下问题一旦存在即判高风险，任何合入/发布不得携带：

- 口令与秘密只存哈希（argon2id 或加盐强哈希），禁止明文、MD5/SHA1 无盐落库，禁止写入日志、配置或代码。
- 口令必须过统一复杂度策略（长度+字符类+弱口令黑名单）并定期更换；不得有可登录的默认口令，初始化必须强制改密。
- 所有外部输入（HTTP 参数、JSON 体、上传、节点上报）必须服务端校验：SQL 参数化、输出编码防 XSS、上传白名单+内容校验+隔离存储。
- 越权即拦截：每个资源请求在服务端做归属校验，跨用户资源一律 NotFound（不泄露存在性）；未登录/低权限访问一律 403/404。
- 登录与鉴权接口只走 TLS ≥1.2，Cookie/令牌标记 HttpOnly、Secure、SameSite；鉴别信息不得明文传输。
- 所有接口默认需鉴权，放行必须白名单式声明；不得出现无鉴权的管理/数据路由。
- 发布前门禁：SAST + 依赖漏洞扫描高危清零，默认凭据与调试开关零残留，测试数据不得混入生产。

完整条款（含测评证据清单）见 [docs/LEVEL2-DEV-REQUIREMENTS.md](docs/LEVEL2-DEV-REQUIREMENTS.md)。

## 验证与提交

- 常规验证执行 `make check`；完整交付执行 `make ci`。
- 修改前端后必须执行 `make web` 重新构建并提交 `internal/httpx/web_dist` 嵌入式产物；CI 会校验产物与源码一致（漂移即红）。
- Fleet 工具契约合并语义（semantic hash、variants、退役条件）照搬 NexusDock，改动必须保持与 nexusdock `internal/agentdock` 对应行为一致，并有测试锁定。
- 提交标题使用 `type(scope): 中文说明`；发布走 `v*` tag（GitHub Actions 自动构建静态二进制与多架构镜像推送 GHCR）。

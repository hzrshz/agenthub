# AgentHub 等保二级整改建议

> 审查日期：2026-11-10
> 审查范围：全部 Go 源码（46 文件 ≈ 10.7k 行，含 cmd/internal/tests/e2e）、前端 web/src、Makefile、Dockerfile、docs/DESIGN.md 相关章节。
> 审查依据：docs/LEVEL2-DEV-REQUIREMENTS.md（§1 技术控制点、§3 高风险红线、§2 开发管理）逐条对照当前实现。
> 结论：**未发现等保高风险红线级缺陷**（无明文口令、无默认口令、注入/越权/明文传输均有防线）；发现 **1 项中等整改（发布门禁缺 SAST/依赖扫描，与红线 7 不一致）、8 项推荐增强（R）、3 项低/文档项、若干部署与测评流程待办**。
> **实施状态：LD-01 / LD-05 / LD-06 已于 2026-11-10 修复**；详见第六节实施记录。

---

## 一、问题清单（编号 → 文件 → 整改建议）

### 严重度说明
- **M**：与 AGENTS.md「等保二级红线」或等保要求不一致，应在一个迭代内修。
- **R**：等保建议项/增强项，非当前测评必扣，但会显著提升符合度与可证明性。
- **Low**：文档/一致性/可维护性，顺手可修。

### LD-01 [M] 发布门禁缺 SAST 与依赖漏洞扫描，与红线 7 不一致 —— ✅ 已修（2026-11-10）
- **状态**：已修复，见第六节。
- **位置**：`Makefile:41-47`（check/ci 仅 `go vet` + race + tsc + web-check）
- **问题**：AGENTS.md 红线第 7 条要求"发布前门禁：SAST + 依赖漏洞扫描高危清零"，但 `make check`/`make ci` 没有 govulncheck、gosec/staticcheck 或任何依赖扫描步骤。发布流水线（.github/workflows/release.yml）同样不校验。测评要求 §1.4e"发现已知漏洞并及时修补"目前只能靠人工升级，不可举证。
- **整改建议**：
  1. `check` 增加 `govulncheck ./...`（Go 官方漏洞数据库，检出依赖 CVE，如 golang.org/x/ 系列）；`go vet` 之外加 `gosec ./...` 或至少 `staticcheck`。
  2. CI（release workflow）增加一步：对 `go.mod` 全量依赖做 `govulncheck`，有未豁免高危即失败。
  3. 输出物归档到发布记录（测评证据：漏洞扫描报告）。

### LD-02 [M] 口令"定期更换"未强制，与红线 2 后半句不一致
- **位置**：`internal/auth/service.go:412-457`（ChangePassword）、`internal/core/schema.go`（users/user_credentials 无口令有效期字段）
- **问题**：红线 2 写"并定期更换"，但实现只有复杂度与首登/重置强制改密（must_change_password），没有口令有效期（如 90 天到期）、到期提示与**历史口令防复用**。高风险判定不以此判红，但测评对照"鉴别信息具有复杂度要求并定期更换"时只能判"部分符合"。
- **整改建议**：
  1. user_credentials 增加 `password_changed_at`；登录/会话校验时判定是否超过有效期（建议 90 天，常量进 internal/config）。
  2. 到期后：会话仍可用但在 `/v1/auth/me` 标记 `password_expired`，前端引导改密（复用 must_change_password 的拦截思路，password_guard 加同一判断）。
  3. 历史口令防复用：新增 `password_history` 表存最近 N 次哈希（如 5），ChangePassword/Reset 校验；或产品决策不强制时，在文档中明确"改由管理制度落实"。
  - 需要产品决策：有效期长度、到期后是强制改还是提示。**此项列为待决策未修项**，如产品不做强制，请修改 AGENTS.md 红线 2 措辞为"（更换周期由管理制度落实）"。

### LD-03 [R] Web 会话无空闲超时，仅 24h 绝对过期
- **位置**：`internal/config/config.go:46`（WebSessionAbsoluteTTL）；`internal/auth/service.go:353-357`（createSession）、`:391-395`（仅绝对期判定）
- **问题**：等保身份鉴别 b"登录连接超时自动退出"。当前会话从登录起 24h 内有效，中途无任何空闲操作也会保持；共享终端/被盗 cookie 窗口偏大。绝对过期已有，但空闲超时是常见测评项补充。
- **整改建议**：user_sessions 增加 `last_used_at`（AuthenticateSession 时顺带更新，或滑动判定）；空闲超过 30 分钟（常量进 config）即吊销。注意 MCP/节点 token 不受此影响（机器对机器入口已有独立限流）。

### LD-04 [R] audit_events 无保留策略/清理/备份机制
- **位置**：`internal/audit/store.go:180`（只有 invoke_logs 的 Cleanup）；`cmd/agenthub/serve.go:66-83`（后台清理任务只调 invokeStore.Cleanup）
- **问题**：等保安全审计 c 要求"审计记录定期备份，避免未预期的删除/修改/覆盖"。audit_events 只增不删，既无归档导出也无保留上限——长期运行体积无限增长，且"定期备份"无落实载体。
- **整改建议**：
  1. 为 audit_events 提供保留策略（建议 180 天 + 条数上限，与 invoke_logs 同构，进 internal/config）。
  2. 提供导出命令（如 `agenthub admin audit-export <since> <until>`，只导出元数据，无秘密）供运维定期归档——同时就是测评要求的"审计记录备份"证据。
  3. 至少把方案写进部署文档（Caddy 部署篇），当前可先在 README 运维节说明。

### LD-05 [R] MCP 调用记录（invoke_logs）缺来源 IP —— ✅ 已修（2026-11-10）
- **状态**：已修复，见第六节。
- **位置**：`internal/mcpgw/gateway.go:384-402`（recordInvoke 只记 user/node/tool/outcome/duration/error_code）；`internal/audit/store.go:69`（RecordInvoke 无 IP 字段）
- **问题**：等保审计要求记录"访问的来源"（IP）。/mcp 是 Bearer 入口，调用记录无来源 IP，安全事件溯源时无法回答"这个调用来自哪个地址"。
- **整改建议**：Gateway.Handler 已有 `bearerToken` 流程但未取 IP；在 Handler 入口用 `config.ClientIP`（同一套可信反代语义）取 IP 放入 context，recordInvoke 额外记录 `client_ip` 列。invoke_logs 表加列 + schema 迁移 v3。

### LD-06 [R] 登录失败审计的 actor 是用户名哈希前缀，管理员无法识别被爆破账号 —— ✅ 已修（2026-11-10）
- **状态**：已修复，见第六节。
- **位置**：`internal/auth/service.go:313-317`（`actor_id`/`object_id` 用 `accountKey[:16]`）
- **问题**：防枚举用了哈希（正确），但审计记录只留 16 位哈希前缀，管理员在审计页无法知道"哪个账号在被爆破"。等保审计要求"审计覆盖到每个用户"，可追溯性打折扣。
- **整改建议**：audit_events 的 metadata_json 增加 `username` 字段（用户名非秘密，审计可明文）；查询审计时按 username 过滤。哈希键仍用于限流，审计侧只增加可读性，两者不冲突。

### LD-07 [R] 工具契约/工具面无显式规模上限
- **位置**：`internal/nodes/store.go:441-444`（UpdateHello 直接 json.Marshal(hello.Tools) 落库）、`internal/fleet/bridge.go:217-277`（reconcileOne 全量合并）、`internal/mcpgw/gateway.go:153-182`（每请求全量装配工具面）
- **问题**：单条消息有 10MB 上限（MaxMessageBytes），但**单工具 schema 大小、每用户工具总数**没有显式上限。恶意/受损节点可上报超大 schema，拖慢每次 /mcp 请求的注册与合约合并（CPU/内存面）。
- **整改建议**：hello 阶段校验：单工具 descriptor JSON ≤ 64KB、每用户工具总数 ≤ 200（常量进 config），超限拒收该工具（记日志）而非全盘接受；与 nexusdock 行为差异需测试锁定（AGENTS.md 约束）。

### LD-08 [Low] core/id.go 注释与实现不一致（配对码哈希方式）
- **位置**：`internal/core/id.go:33-35`
- **问题**：注释称"配对码……由 auth 层用 bcrypt 处理"，实际配对码在 `internal/nodes/store.go:129` 用 `core.HashToken`（SHA-256）落库——24 字节随机串，SHA-256 足够安全，注释纯属陈旧误导。
- **整改建议**：修正注释为"配对码是高熵随机串，SHA-256 足够；口令才用 argon2id"。

### LD-09 [Low] MCP 网关关闭 DNS rebinding 防护（已权衡，需记录在案）
- **位置**：`internal/mcpgw/gateway.go:124-131`（DisableLocalhostProtection: true）
- **问题**：出于"反代之后、内网部署"的考虑关闭了 SDK 的 Host 头校验。有注释说明取舍，但若未来直接暴露公网或未配反代，此防护缺失。
- **整改建议**：保持现状（合理取舍），但在部署文档中明确"必须置于可信反代之后，不得直连公网"；若出现直连部署形态，改为基于 HUB_PUBLIC_URL 主机名做白名单校验。

### LD-10 [Low] user_sessions 只增不删
- **位置**：`internal/auth/service.go:353-357`（每次登录新建会话行）、`:391-395`（过期时才标记 revoked，不删行）
- **问题**：长期运行 user_sessions 行数持续增长（无 retention）。非安全缺口，但会缓慢膨胀控制库。
- **整改建议**：与 LD-04 同一清理任务中顺带裁剪（revoked_at 超过 30 天或行数上限）。

---

## 二、已符合项（对照检查，无需整改）

| 等保条款 | 现状 | 位置 |
|---|---|---|
| 1.1a 口令唯一/复杂度/定期更换 | 复杂度极强（≥12 位、3 类、弱口令/连续/重复/含用户名全拦）；首登与管理员重置强制改密 | auth/password_policy.go、service.go:136-170 |
| 1.1b 登录失败处理 | 账号+IP 双维度阶梯封锁（最终 15 分钟）、登录超时自动退出（会话绝对过期）、限流键防 XFF 伪造 | auth/service.go:289-335、config.go:163-173 |
| 1.1c 鉴别信息防窃听 | Cookie HttpOnly/Secure/SameSite=Lax；HSTS 仅在可信 HTTPS 下发；PublicURL 公网域名强制 https | httpx/server.go:347-356、security_headers.go |
| 1.1（双因素为三级 d 项，现有形态提前具备） | Web 会话 = 口令验签 + 服务端签名令牌；MCP/Device token 用密码学哈希承载 | auth/service.go、nodes/store.go |
| 1.2a/b/c/d 访问控制 | RBAC（admin/user）+ 数据级归属校验；跨租户一律 NotFound（不泄露存在性）；默认拒绝；无默认口令；账号可停用且即时生效 | nodes/store.go:353-361、httpx/server.go:230-239、nodes_http.go |
| 1.3a/b/c 安全审计 | 登录成败/改密/管理操作/节点操作全覆盖；字段含时间/用户/动作/对象/结果/来源 IP；append-only；不落 arguments/result | auth/service.go:795-807、nodes/store.go:85-104、core/schema.go |
| 1.4a/d/e 最小安装/数据有效性/漏洞修补 | scratch 镜像+非 root；输入统一 decodeJSON 1MB 上限+参数化 SQL+PairInput 字段校验+schema 注入防御+panic 防护；go1.26.10（2026-11 修复 GO-2026-6809）依赖与标准库漏洞已清零（govulncheck 门禁） | Dockerfile、httpx/server.go:297-304、mcpgw/gateway.go:187-275 |
| 1.6/1.7 完整性与保密性 | 传输由反代 TLS 终结（部署契约）；应用层不落任何明文秘密；敏感数据（哈希）存储；库文件权限 0600/0700 | README、core/id.go、0e136a9 |
| 1.8 数据备份恢复 | 单文件 SQLite 控制库，数据目录卷挂载（部署侧需补演练记录，见 §四） | Dockerfile |
| 1.9 剩余信息保护 | 秘密只存哈希、明文只出现一次；日志/审计无明文 Token/口令；节点 DeviceToken 用 `json:"-"` 屏蔽序列化 | core/id.go、nodes/store.go:51-54 |
| 3 红线 | 口令哈希（argon2id）、复杂度、默认口令、注入、越权、明文传输、默认鉴权——全部有对应防线；本次未发现任一红线级实现缺陷 | 见上表 |
| 开发管理 2 | admin init/reset 口令走本地交互（terminal 不回显），无环境变量/参数形态 | cmd/agenthub/admin.go:20-21 |
| Slowloris/连接耗尽 | ReadHeaderTimeout 15s + IdleTimeout 120s；WS 读写 deadline；注意不设 Read/WriteTimeout 是有注释的取舍 | cmd/agenthub/serve.go:93-103、bridge/hub.go:163-164,368 |
| 前端 XSS 面 | 无 dangerouslySetInnerHTML/innerHTML/eval；CSP 严格（script-src 'self'、object-src 'none'、base-uri 'none'、frame-ancestors 'none'） | security_headers.go:20-45、web/src（grep 无命中） |
| e2e 安全回归 | 跨租户隔离、禁用即时性、配对码伪造/并发单次使用均有 e2e 锁定 | tests/e2e/security_test.go |

---

## 三、未修/待决策项（显式列出）

1. **LD-02（口令定期更换）**：需要产品决策"是否强制 90 天到期改密 + 历史口令防复用"。若不做，需同步修改 AGENTS.md 红线 2 措辞，保持"文档约束 = 代码实现"一致。
2. **LD-04（审计归档/备份）**：备份载体是运维动作（导出归档），代码侧先补保留与导出命令；若暂不做导出命令，部署文档需写明外部备份方案。
3. **已知历史待决策项（沿用）**：心跳测试断言、provenance 关闭（见记忆 agenthub-deferred-fixes），非本次新增。

## 四、部署与测评流程待办（非代码，但测评必查）

- [ ] 反代侧强制 TLS ≥1.2 + HSTS（Caddy 反代为 HTTPS 终结，`agenthub` 应用层 HSTS 与之同值；详见 docs/LEVEL2-DEV-REQUIREMENTS §1.1c 与 Caddy 部署篇）。
- [ ] `HUB_TRUSTED_PROXY_CIDRS` 在生产配置中显式设置（记忆：容器内不能监听回环、必须设可信反代网段）。
- [ ] 主机/容器侧恶意代码扫描（等保 1.5 依赖主机层，应用层无载体）。
- [ ] 数据备份恢复演练记录（等保 1.8：测评抽查）。
- [ ] 渗透测试/安全测试验收报告（等保 §2 测试验收）。
- [ ] 审计日志外部备份与定期导出（LD-04 落地前先人工执行）。

## 五、建议落地顺序

1. ~~LD-01（Makefile/CI 加 govulncheck + gosec）~~ ✅ 已修（2026-11-10）。
2. ~~LD-05/LD-06（审计补来源 IP、登录失败可读性）~~ ✅ 已修（2026-11-10）。
3. 与产品确认 LD-02（口令有效期）、LD-04（审计保留周期），决策后实现或改文档措辞。
4. LD-03/LD-07/LD-10 按版本顺手做；LD-08/LD-09 随文档修订处理。

## 六、实施记录（2026-11-10，LD-01/LD-05/LD-06）

**LD-01 发布门禁（SAST + 依赖漏洞扫描）**
- `Makefile`：新增 `vuln`/`gosec` 目标（固定版本 `govulncheck@v1.9.0`、`gosec@v2.30.0`，用 `go run pkg@version` 调用，不依赖 `$GOPATH/bin` 在 PATH 上）；`check` 纳入 `vuln`，`ci` 纳入 `gosec`。
- `.github/workflows/ci.yml`：test job 增加 `make vuln` + `make gosec` 两步（每次 push/PR 执行）。
- `.github/workflows/release.yml`：新增独立 `security-gate` job（binaries 依赖它），tag 发布前扫描必须清零。
- **整改伴随的依赖升级**：govulncheck 首次跑通即发现 `GO-2026-6809`（crypto/x509，go1.26.9 受影响、**go1.26.10 修复**）与 `GO-2026-5932`（x/crypto v0.58.0，修复尚未打 tag）。已将工具链升到 go1.26.10（`go.mod` 增 `toolchain go1.26.10`，ci/release setup-go 同步）、`x/crypto` 升到修复提交 `v0.58.1-0.20261009181718`。验证：`make vuln` → "No vulnerabilities found"，`make gosec` → 0 告警。
- **gosec 4 项发现处置**：G302（数据目录 0700，目录需属主可执行位，`#nosec` + 注释）；G101（口令策略文案非凭据，`#nosec` + 注释）；G115（argon2 参数长度已在上文校验 ∈[16,64]，行内 `#nosec` + 注释）；G118（心跳 Touch 用 `context.Background()`）→ **真修**：心跳落库改 5 秒超时 ctx，防止慢库写拖垮读循环，并在 `go h.readLoop` 处注释说明连接级 goroutine 故意脱离请求生命周期。

**LD-05 MCP 调用记录来源 IP**
- `internal/core/schema.go`：`CurrentSchemaVersion` 2→3；`currentSchema` 的 invoke_logs 增 `client_ip TEXT NOT NULL DEFAULT ''`；新增迁移 v3（对已存在库 ALTER 加列，新库走 currentSchema）。
- `internal/audit/store.go`：`InvokeLog` 增 `ClientIP`；`RecordInvoke` 增 `clientIP` 参数并落库；查询 SELECT/扫描带出该列。
- `internal/mcpgw/gateway.go`：`InvokeRecorder` 接口增 `clientIP` 参数；`Handler()` 从请求上下文取来源 IP 并显式挂入 reqCtx（生产上 httpx 的 auditIPInjector 已注入，双保险）；`recordInvoke` 读取 `core.AuditSourceIP` 落库。
- 测试：新增 `TestRecordInvokeStoresClientIP`（round-trip）、`TestInvokeRecorded` 断言记录带来源 IP（网关测试 mux 加 `withAuditIP` 注入模拟生产中间件）；v3 迁移在 `TestMigrationV2AddsSourceIPToExistingAuditTable` 的 v1 旧库夹具补 invoke_logs 并断言 client_ip 列。

**LD-06 登录失败审计可读性**
- `internal/auth/service.go` `Login`：登录失败与成功的审计记录在 `metadata_json` 附带明文 `username`（actor_id 仍用哈希键不变；用户名非秘密，审计可明文）；CLI 自举等无来源路径不受影响。
- 当前无审计浏览接口，按 DB 层 JSON 字段可查（`LIKE '%"username":"..."%'` 或未来审计 UI 读 metadata 字段）；如后续建审计页，直接读该字段过滤即可。

**验证：** `go build ./...`、`go test ./...`、`go vet ./...` 全绿；关键包 race 通过；`make vuln`/`make gosec` 退出码 0。
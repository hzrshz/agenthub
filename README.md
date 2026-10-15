# AgentHub

多用户 [AgentDock](https://github.com/uvwt/agentdock) 汇聚网关。AgentDock 通过**出站 WebSocket** 注册到 hub（无需公网 IP、无需暴露端口），Agent 平台以 **MCP**（Streamable HTTP）方式接入，调用按用户路由到该用户自己的节点。

```
AgentDock ──出站 WSS──▶ ┌─────────┐ ◀──MCP (Streamable HTTP)── Agent 平台
  /v1/nodes/pair        │ AgentHub │      单一 /mcp，Bearer token 即身份
  /v1/nodes/connect     │  (Go)    │
                        └─────────┘
                     SQLite 单文件，反代终结 TLS
```

设计规格：[docs/DESIGN.md](docs/DESIGN.md)（全部运行时语义与安全配额的定义）。实施计划：[docs/PLAN.md](docs/PLAN.md)。

## 核心概念

- **管理员**：本地 CLI 初始化；登录后创建用户（邮箱形态用户名 + 初始密码，首登强制改密）、禁用/启用、重置密码、看全局调用统计。
- **普通用户**：登录 → 生成一次性配对码（10 分钟）→ 在设备上执行 `agentdock nexus pair --endpoint <本站> --code <码>`；界面管理节点（在线状态/禁用/删除）与 MCP Token（重置即旧 token 失效）。
- **Agent 平台**：以 MCP client 连 `<PublicURL>/mcp`，Bearer Token 为用户的 MCP Token。工具面 = 该用户在线节点的裸名工具（同名工具按 fleet 语义合并）+ `nodes_context` 中心工具；所有透传工具要求 `node_id` 参数（先调 `nodes_context` 获取）。

## 部署

前置：一个能终结 TLS 的反向代理（caddy/nginx）指向本机监听地址。

```bash
# 1. 初始化管理员（交互输入口令，不回显）
HUB_DATA_DIR=/var/lib/agenthub HUB_PUBLIC_URL=https://hub.example.com \
  ./agenthub admin init

# 2. 启动服务（只听 HTTP，TLS 交给反代）
HUB_DATA_DIR=/var/lib/agenthub HUB_PUBLIC_URL=https://hub.example.com \
  HUB_LISTEN=127.0.0.1:8787 ./agenthub serve
```

### 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `HUB_PUBLIC_URL` | 是 | hub 的公网 HTTPS origin（无路径）。节点据此推导回调地址，必须 `https://` |
| `HUB_DATA_DIR` | 否 | 数据目录（SQLite 单文件 + 日志），默认 `~/.agenthub` |
| `HUB_LISTEN` | 否 | 监听地址，默认 `127.0.0.1:8787` |

### 备份

停机复制 `HUB_DATA_DIR/control.db` 单文件即可，不做在线备份。

## 构建

```bash
make ci     # 完整验证：构建前端产物 + go 测试 + vet
make check  # 常规验证：go 测试 + 前端类型检查
```

单二进制构建：`go build -o agenthub ./cmd/agenthub`（Web 界面已 `go:embed` 进二进制）。

### 国产化环境（静态编译）

全依赖为纯 Go 实现（SQLite 用 `modernc.org/sqlite`，无 CGO），产物为**完全静态链接**的单二进制，`ldd` 显示 `not a dynamic executable`，可直接复制到目标机运行——无需安装任何运行时或共享库。

支持目标（`make release-linux` 本地构建，或推 `v*` tag 走 GitHub Actions 自动发布）：

| GOOS/GOARCH | 覆盖环境 |
|---|---|
| `linux/amd64` | x86_64（海光、兆芯等） |
| `linux/arm64` | 鲲鹏、飞腾（Phytium）等 ARM64 |
| `linux/loong64` | 龙芯 3A5000/3A6000（LoongArch） |
| `linux/riscv64` | RISC-V 生态 |

```bash
make release-linux   # 产出 dist/agenthub-linux-<arch> + checksums.txt
```

GitHub Actions：`ci.yml` 在每次推送时做测试 + vet + loong64 静态编译冒烟（守门，防止日常提交破坏国产化目标）；`release.yml` 在打 `v*` tag 时构建全部目标、校验静态链接、自动创建 Release 并附 sha256 校验和。

## 安全模型（摘要）

- 三类秘密（Web session / 每用户 MCP Token / 每节点 Device Token）全部只存 SHA-256 哈希；口令 argon2id。
- 登录与 `/v1/nodes/pair` 均有限流与失败递增封锁。
- 配对码绑定用户、10 分钟、单次；用户禁用 = 节点断线 + MCP Token 失效 + 会话吊销，全部即时。
- 调用记录只存元数据（时间/用户/节点/工具/结果/耗时），工具参数与返回内容**绝不落库**；30 天滚动清理。
- 默认配额：每用户 10 节点、MCP 60 req/min、每节点在途 16、单条消息 10MB、invoke 120s 超时。

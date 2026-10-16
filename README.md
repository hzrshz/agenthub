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
| `HUB_PUBLIC_URL` | 是 | hub 对外访问的 origin（无路径）。**生产必须 `https://`；测试允许 `http://localhost` 或 `http://<局域网IP>`**，公网 http 域名会被拒绝 |
| `HUB_DATA_DIR` | 否 | 数据目录（SQLite 单文件），默认 `~/.agenthub`（容器内 `/data`） |
| `HUB_LISTEN` | 否 | 监听地址，默认 `127.0.0.1:8787`（容器内 `0.0.0.0:8787`） |

### HUB_PUBLIC_URL 的生效范围与修改

`HUB_PUBLIC_URL` 是纯启动配置，**不落库**，改完重启即生效。它影响三处，修改后的传导各不相同：

| 影响点 | 修改后是否自动跟随 |
|---|---|
| Web 界面展示的 MCP endpoint | ✅ 重启即跟随新值 |
| `node.ready` 下发给节点的回调地址（OAuth redirect） | ✅ 节点下次重连即拿到新值 |
| **已配对节点的 WebSocket 连接目标** | ❌ 节点连的是**配对时输入的 endpoint**（存于节点侧），改 hub 不断开已连节点 |

因此"先用 IP 测试、之后迁移到 HTTPS 域名"完全支持：改环境变量重启后，Web/平台侧立即用新地址；**已配对的节点仍按旧地址连接**（继续工作，只是走旧地址）。若希望节点也切到新域名，删掉节点重新配对即可（Device Token 随删除吊销）。

### 本地测试步骤（无 HTTPS，先跑通全链路）

前置：本机已构建 `agenthub` 二进制（`go build -o agenthub ./cmd/agenthub`），局域网内有一台设备可以运行 AgentDock。以下假设本机局域网 IP 为 `192.168.1.10`（`ipconfig getifaddr en0` 查询），AgentDock 与本机同一网段。

```bash
# 1. 初始化管理员（用局域网 IP 作为 PublicURL；端口与 HUB_LISTEN 一致）
HUB_DATA_DIR=~/.agenthub-test HUB_PUBLIC_URL=http://192.168.1.10:8787 \
  ./agenthub admin init

# 2. 启动服务（监听 0.0.0.0 才能让局域网设备访问）
HUB_DATA_DIR=~/.agenthub-test HUB_PUBLIC_URL=http://192.168.1.10:8787 \
  HUB_LISTEN=0.0.0.0:8787 ./agenthub serve
```

```bash
# 3. 浏览器打开 http://192.168.1.10:8787，管理员登录，创建一个普通用户
#    （建号响应里会返回该用户的 MCP Token 与 endpoint，记下）

# 4. 以新用户登录，页面点"生成配对码"，在 AgentDock 设备上执行：
agentdock nexus pair --endpoint http://192.168.1.10:8787 --code <配对码>

# 5. 界面上节点变为"在线"后，Agent 平台以 MCP client 接入：
#    endpoint = http://192.168.1.10:8787/mcp
#    Authorization: Bearer <该用户的 MCP Token>
#    先调 nodes_context 拿 node_id，再带 node_id 调用具体工具。
```

注意：AgentDock 节点侧对 endpoint 的校验与 hub 一致——`http://` 只接受 localhost 或 IP 地址，所以测试期必须用 `http://<IP>:8787` 形态，不能写内网主机名。

**迁移到 HTTPS 生产站点**：

```bash
# 1. 停服务，改环境变量为 https 域名（反代已终结 TLS 并指向 HUB_LISTEN）
HUB_DATA_DIR=/var/lib/agenthub HUB_PUBLIC_URL=https://hub.example.com \
  HUB_LISTEN=127.0.0.1:8787 ./agenthub serve

# 2. Web 界面与平台侧 MCP endpoint 立即变为新地址；
# 3. 测试期配对的节点若也要切换，在界面删除节点 → 用新域名重新配对：
agentdock nexus pair --endpoint https://hub.example.com --code <新配对码>
```

数据目录沿用同一个 `HUB_DATA_DIR` 即可保留账号与节点记录；如果想从干净环境开始，换一个新的 `HUB_DATA_DIR` 重新 `admin init`。

### 备份

停机复制 `HUB_DATA_DIR/control.db` 单文件即可，不做在线备份。

## Docker 部署

镜像发布到 GHCR（不推 Docker Hub），多架构覆盖 amd64/arm64/loong64/riscv64，运行层为 distroless static（无 shell，非 root）：

```bash
# tag 推送后自动构建；拉取（把 OWNER 换成你的 GitHub 用户/组织名，小写）
docker pull ghcr.io/<owner>/agenthub:latest

# 首次初始化管理员（挂载数据卷，容器内执行）
docker run --rm -it -v agenthub-data:/data \
  -e HUB_PUBLIC_URL=https://hub.example.com \
  ghcr.io/<owner>/agenthub:latest admin init

# 启动服务（容器内默认 HUB_LISTEN=0.0.0.0:8787，HUB_DATA_DIR=/data）
docker run -d --name agenthub -p 8787:8787 \
  -v agenthub-data:/data \
  -e HUB_PUBLIC_URL=https://hub.example.com \
  ghcr.io/<owner>/agenthub:latest serve
```

本地构建镜像：`docker build -t agenthub .`（多阶段：node 构建前端 → golang 静态编译 → distroless 运行层）。

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

## 路线图

- **v1（当前）**：静态 MCP Token 接入 `/mcp`（面向自有平台机器对机器调用）。
- **规划中**：Hub 作为 OAuth 授权服务器，支持带 OAuth 能力的外部 MCP client（Claude Desktop 等）自助授权接入 `/mcp`，与静态 Token 双轨共存。设计方案（协议面/数据模型/实现要点）见 [docs/DESIGN.md §9](docs/DESIGN.md)。节点侧认证保持 device token 模式不变。

## 安全模型（摘要）

- 三类秘密（Web session / 每用户 MCP Token / 每节点 Device Token）全部只存 SHA-256 哈希；口令 argon2id。
- 登录与 `/v1/nodes/pair` 均有限流与失败递增封锁。
- 配对码绑定用户、10 分钟、单次；用户禁用 = 节点断线 + MCP Token 失效 + 会话吊销，全部即时。
- 调用记录只存元数据（时间/用户/节点/工具/结果/耗时），工具参数与返回内容**绝不落库**；30 天滚动清理。
- 默认配额：每用户 10 节点、MCP 60 req/min、每节点在途 16、单条消息 10MB、invoke 120s 超时。

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
| `HUB_TRUSTED_PROXY_CIDRS` | 否 | 可信反代网段（逗号分隔的 CIDR），**用了反向代理就应设置**。只有请求来自这些网段时 hub 才采信 `X-Forwarded-For` / `X-Forwarded-Proto`，用于按 IP 限流与 HSTS 判定；未设置则一律不采信（客户端可伪造这些头绕过限流）。宿主机反代填 `127.0.0.1/32,::1/128`，compose 网段按实际网段填 |

### HUB_PUBLIC_URL 的生效范围与修改

`HUB_PUBLIC_URL` 是纯启动配置，**不落库**，改完重启即生效。它影响三处，修改后的传导各不相同：

| 影响点 | 修改后是否自动跟随 |
|---|---|
| Web 界面展示的 MCP endpoint | ✅ 重启即跟随新值 |
| `node.ready` 下发给节点的回调地址（OAuth redirect） | ✅ 节点下次重连即拿到新值 |
| **已配对节点的 WebSocket 连接目标** | ❌ 节点连的是**配对时输入的 endpoint**（存于节点侧），改 hub 不断开已连节点 |

因此"先用 IP 测试、之后迁移到 HTTPS 域名"完全支持：改环境变量重启后，Web/平台侧立即用新地址；**已配对的节点仍按旧地址连接**（继续工作，只是走旧地址）。若希望节点也切到新域名，删掉节点重新配对即可（Device Token 随删除吊销）。

### 本地测试步骤（含局域网 AgentDock 配对：需要本地 HTTPS）

**先说一个关键限制**（源码确认，配对报"公网 NexusDock endpoint 必须使用 HTTPS"即此原因）：AgentDock 侧对配对 endpoint 的校验只放行三种形态——

1. `https://任意地址`（含局域网 IP，自签证书见下文）
2. `http://localhost:<port>` / `http://127.0.0.1:<port>`（仅 AgentDock 与 hub 同机时可用）
3. 其他一律拒绝：`http://<局域网IP>` 也不行，**写了 `/mcp` 路径也不行**（endpoint 必须是不带路径的 origin）

因此局域网内另一台设备上的 AgentDock 要配对到你的 hub，**必须在 hub 前面架一个本地 HTTPS 反代**（哪怕用自签证书），配对地址写 `https://<局域网IP>:8443`。同机测试可以跳过反代直接用 `http://127.0.0.1:8787`。

#### 方案 A：同机快速测试（无 TLS，5 分钟）

AgentDock 与 hub 在同一台机器上时可用。

```bash
go build -o agenthub ./cmd/agenthub

# 1. 初始化并启动（PublicURL 与 AgentDock 的配对地址一致）
HUB_DATA_DIR=~/.agenthub-test HUB_PUBLIC_URL=http://127.0.0.1:8787 \
  ./agenthub admin init
HUB_DATA_DIR=~/.agenthub-test HUB_PUBLIC_URL=http://127.0.0.1:8787 \
  HUB_LISTEN=0.0.0.0:8787 ./agenthub serve

# 2. 浏览器打开 http://127.0.0.1:8787 → 管理员登录 → 创建普通用户
# 3. 用户登录 → "生成配对码" → AgentDock 侧填：
#    地址 = http://127.0.0.1:8787   （不要带 /mcp）
#    一次性配对码 = <界面上的码> → 点"配对并重启"
# 4. Agent 平台接入：endpoint = http://127.0.0.1:8787/mcp，
#    Authorization: Bearer <建号时返回的 MCP Token>
```

#### 方案 B：局域网设备配对（caddy 自签 HTTPS）

hub 在本机（假设局域网 IP 为 `192.168.1.10`，`ipconfig getifaddr en0` 查询），AgentDock 在局域网另一台设备上。

```bash
# 1. 安装 caddy（brew install caddy）并用它生成本地 CA 自签证书
mkcert -install                                    # 或: caddy 的 local CA 亦可
mkcert 192.168.1.10                                # 生成 192.168.1.10+2.pem/.key
mkdir -p ~/.agenthub-certs && mv 192.168.1.10+*.pem 192.168.1.10+*-key.pem ~/.agenthub-certs/

# 2. caddy 反代：4443(HTTPS) → 8787(hub)，Caddyfile 内容见下
cat > ~/.agenthub-certs/Caddyfile <<'EOF'
{
  auto_https off
}
https://192.168.1.10:4443 {
  tls ~/.agenthub-certs/192.168.1.10+2.pem ~/.agenthub-certs/192.168.1.10+2-key.pem
  reverse_proxy 127.0.0.1:8787
}
EOF
caddy start --config ~/.agenthub-certs/Caddyfile

# 3. hub 用 https origin 启动（PublicURL 与 AgentDock 将填的地址一致）
HUB_DATA_DIR=~/.agenthub-test HUB_PUBLIC_URL=https://192.168.1.10:4443 \
  HUB_LISTEN=0.0.0.0:8787 ./agenthub serve

# 4. AgentDock 设备上：信任 mkcert 根证书（mkcert -install 在设备上执行，或手动导入
#    ~/Library/Application Support/mkcert/rootCA.pem），然后配对：
#    地址 = https://192.168.1.10:4443   （不要带 /mcp）
#    一旦 AgentDock 报证书错误，说明设备未信任 mkcert 根 CA。

# 5. Agent 平台接入：endpoint = https://192.168.1.10:4443/mcp
#    （平台侧若报证书错误，同样需要信任 mkcert 根 CA，或平台跳过校验）
```

> Windows 的 AgentDock 设备：把 mkcert 生成的 `rootCA.pem` 双击导入"受信任的根证书颁发机构"即可。

测试结束后 `caddy stop`。

#### 迁移到 HTTPS 生产站点

```bash
# 1. 停服务，改环境变量为 https 域名（反代已终结 TLS 并指向 HUB_LISTEN）
HUB_DATA_DIR=/var/lib/agenthub HUB_PUBLIC_URL=https://hub.example.com \
  HUB_LISTEN=127.0.0.1:8787 ./agenthub serve

# 2. Web 界面与平台侧 MCP endpoint 立即变为新地址；
# 3. 测试期配对的节点若也要切换，在界面删除节点 → 用新域名重新配对：
agentdock nexus pair --endpoint https://hub.example.com --code <新配对码>
```

数据目录沿用同一个 `HUB_DATA_DIR` 即可保留账号与节点记录；如果想从干净环境开始，换一个新的 `HUB_DATA_DIR` 重新 `admin init`。

### 用 Caddy 做反代（推荐生产部署）

Caddy 自动申请并续期证书，只需一个 Caddyfile。**注意 Caddy 默认不下发
HSTS，需要显式配置**（实测 v2.6.4 确认）：

```caddyfile
hub.example.com {
  encode zstd gzip

  # HSTS：Caddy 不会自动下发，必须显式声明。
  # max-age 一年。不加 includeSubDomains / preload——反代未必覆盖全部子域，
  # 预加载指令一旦提交到浏览器列表就难以撤销。
  header Strict-Transport-Security "max-age=31536000"

  reverse_proxy 127.0.0.1:8787
}
```

Caddy 会自动代理 `X-Forwarded-Proto`，因此 hub 能正确识别这是 HTTPS 请求并
自行下发 HSTS（两者同值，重复无害）。应用层的其余安全响应头
（CSP / X-Frame-Options / nosniff 等）由 hub 直接下发，经反代后依然存在
——已实测确认 Caddy 的 `reverse_proxy` 不会覆盖后端响应头。

配合 hub 侧需要设置可信反代来源，否则 hub 会忽略 `X-Forwarded-Proto`
与 `X-Forwarded-For`（防止客户端伪造绕过限流）：

```bash
HUB_TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128 \
HUB_LISTEN=127.0.0.1:8787 \
HUB_PUBLIC_URL=https://hub.example.com \
  ./agenthub serve
```

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
#
# 端口绑定到宿主【回环】：8787 不发布到公网，只有本机的反代（Caddy/nginx）能
# 访问。容器内必须保持监听 0.0.0.0 —— 容器有独立网络命名空间，改成 127.0.0.1
# 会导致端口映射与同网络的其他容器（反代）都连不上。
docker run -d --name agenthub -p 127.0.0.1:8787:8787 \
  -v agenthub-data:/data \
  -e HUB_PUBLIC_URL=https://hub.example.com \
  -e HUB_TRUSTED_PROXY_CIDRS=127.0.0.1/32 \
  ghcr.io/<owner>/agenthub:latest serve
```

若反向代理也跑在容器里，让两者处于同一 Docker 网络用服务名互访即可，
此时可以不发布端口（比发布到回环更严格）：

```yaml
# compose 片段
services:
  agenthub:
    image: ghcr.io/<owner>/agenthub:latest
    # 不写 ports：同网络内用服务名互访，宿主完全访问不到
    environment:
      HUB_PUBLIC_URL: https://hub.example.com
      HUB_TRUSTED_PROXY_CIDRS: 172.16.0.0/12   # 覆盖 compose 网段
    volumes: [agenthub-data:/data]
  caddy:
    image: caddy:2-alpine
    ports: ["80:80", "443:443"]
    volumes: ["./Caddyfile:/etc/caddy/Caddyfile:ro"]
volumes:
  agenthub-data:
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

### 国密算法：暂未实现（后续处理）

当前**全部使用国际算法**，没有任何国密（SM2/SM3/SM4）实现：

| 用途 | 当前算法 |
|---|---|
| 口令哈希 | argon2id |
| Token / 配对码 / 会话哈希 | SHA-256 |
| 传输加密 | 由反代终结 TLS（RSA/ECDSA + AES-GCM） |

**这是否构成等保二级的问题？** 等保二级（GB/T 22239-2019）本身**未强制要求
国密算法**；强制要求国密通常出现在等保三级及以上，或由行业主管部门的
信创/密码应用要求单独提出。因此：

- 按等保二级测评，目前的实现不因"没有国密"被判定为不合规。
- 若上级有独立的密码应用要求（信创清单、密评），则需要在后续版本补齐。

**若要补齐，改动范围**（供后续排期参考，非当前承诺）：

1. **SM3 替代 SHA-256**：影响 `core.HashToken`（token/配对码/会话哈希），
   以及 `nodes.device_token_hash`、`user_sessions.token_hash`、
   `user_mcp_tokens.token_hash`、`published_tool_variants.semantic_hash`。
   注意语义哈希的**跨版本稳定性**——已入库的哈希值无法反推，切换算法需要
   一次性迁移或强制重置全部凭据。
2. **SM4 替代对称加密**：当前 hub 不做对称加密（敏感值一律只存哈希），
   若新增"可解密的字段加密"需求（如配置项加密存储）才需要。
3. **SM2**：仅在需要签发/验签 X.509 证书时使用；当前 hub **不签发证书**
   （TLS 完全交给反代），因此这一项大概率不需要动。

因此更务实的路径是：先确认密码应用的具体要求范围（第 2 层是"传输与
存储的密码算法合规"，还是仅要求"通信机密性"），再决定是全面替换还是
仅在 TLS 层由反代接入国密网关/TLCP。

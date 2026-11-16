# 开发改动影响面检查清单

> 配套 AGENTS.md「改动影响面联动」：任何功能改动提交前，按本文档逐项自查受影响面，
> 并一并交付。**漏改侧翼（文档/示例/部署产物）是缺陷不是遗漏。**

## 1. 本次教训（2026-11-16，多数据库适配）

适配 MySQL/PostgreSQL 时只改了 Go 代码、测试、CI 与阅读面文档，**漏掉了容器部署文档**：
- Dockerfile `/data` 注释仍写"SQLite 单文件"（过时）；
- README Docker 节没有服务器后端的口令文件挂载/TLS 配置示例；
- 顺带揪出两处**既有**错误描述：运行层实际是 scratch 装配（文档写 distroless）、
  本地镜像构建需先 `make release-linux`（文档写多阶段构建）。

根因：改动时只围绕"功能主体"（代码）思考，没把部署产物/示例/注释纳入交付范围。
对策：本清单 + 提交前自查动作（见第 3 节）。

## 2. 标准联动表

| 改动主体 | 必须同步检查/修改的关联位置 |
|---|---|
| 新增/修改环境变量或配置 | `internal/config/config.go`（解析+校验+测试）、`.env.example`、README 环境变量表、docs/DESIGN.md §7、容器部署示例（docker run/compose，如容器可见）、`scripts/run.sh` 白名单（如需要注入） |
| 数据库 / SQL / schema | `internal/core/schema.go` 三端 DDL（currentSchema / currentSchemaMySQL / currentSchemaPostgres）、迁移（schema_migrations / user_version）、三端真库测试（SQLite/MySQL/PG，`make test-db`）、CI 真库 job（ci.yml）、备份策略文档（LEVEL2 §1.8） |
| HTTP / API / 路由 / 协议 | web 前端（改后必须 `make web` 并提交 `internal/httpx/web_dist`）、README/等保文档、反代文档（Caddy/nginx 示例，见 docs/DESIGN.md §7 与 README） |
| 部署形态（端口/路径/卷/镜像/启动方式/脚本） | Dockerfile、Makefile build/release 目标、README Docker 节与 compose 示例、`scripts/run.sh`、`.env.example` |
| 安全 / 认证 / 秘密 / 配额 | 等保文档（docs/LEVEL2-REVIEW-REMEDIATION.md）、审计与限流测试、秘密存储与权限相关注释 |
| 依赖 / 工具链 | go.mod/go.sum、Makefile 版本固定、CI 矩阵（ci.yml/release.yml）、README 版本要求、govulncheck/gosec 门禁 |

## 3. 提交前自查动作（3 步，<1 分钟）

1. `git diff --stat`：列出改动文件，对照第 2 节表，逐行确认每个受影响面都有改动或"确认无需改"。
2. `grep -rn "<被改的旧表述>" .`：全局搜旧变量名/路径/过时描述（如 `SQLite 单文件`、`distroless`、被重命名或删除的标识符），确认无残留引用。
3. 环境变量级改动时，数一下落点是否齐：config 解析 → 校验测试 → `.env.example` → README 表 → DESIGN → 容器示例 → run.sh。（新增变量建议在 `.env.example` 中就写好注释说明默认行为，避免"缺省值是什么"靠猜。）

## 4. 常见遗漏点清单（历史踩坑）

- 改了代码但**注释/示例**还是旧行为（Dockerfile 注释、README 示例、compose 片段）。
- 新增配置项但 `.env.example` / run.sh 注入白名单没跟上（容器与裸机行为不一致）。
- 改了 schema 但三端 DDL 只改了一端（schema.go 里三份 currentSchema 必须同步）。
- 改了备份/数据落点但 docs/LEVEL2-REVIEW-REMEDIATION.md §1.8 还是旧策略。
- 改了默认值/配额但 README「默认」描述没更新（文档语句与 config 常量漂移）。
- 改了前端但没跑 `make web`（CI 漂移守门会红，但本地自查应先发现）。
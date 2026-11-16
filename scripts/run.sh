#!/usr/bin/env bash
# run.sh — 从 .env 注入 HUB_* 环境变量并管理数据库口令文件，然后启动 agenthub。
#
# 子命令：
#   ./scripts/run.sh init [--force]   初始化数据库口令文件（交互式不回显写入）
#   ./scripts/run.sh serve [...]      启动服务（自动校验口令文件存在与权限）
#   ./scripts/run.sh admin init|...   管理命令（同样注入 .env）
#   ./scripts/run.sh                  打印本帮助
#
# 用法：
#   cp .env.example .env                # 首次：复制模板并按需修改（.env 已 git 忽略）
#   ./scripts/run.sh init               # mysql/postgres 后端：生成数据库口令文件
#   ./scripts/run.sh serve              # 启动
#   HUB_ENV_FILE=/etc/agenthub/env AGENTHUB_BIN=/usr/local/bin/agenthub ./scripts/run.sh serve
#
# 设计约束：
#   - 只注入 HUB_[A-Z0-9_]+ 白名单变量，其余键值一律忽略；
#   - 纯文本解析，不做 eval / 命令替换——值里的 $()、反引号、分号不会被求值；
#   - 秘密不经过 .env：数据库口令只存在于 HUB_DB_PASSWORD_FILE 指向的文件
#     （init 创建，0600），管理员口令走交互式 `admin init/reset`，终端不回显——
#     等保红线：秘密不进环境变量/配置文件/日志。
#
# 注意：变量后紧跟中文时一律写成 ${var}——bash 3.2 在 UTF-8 locale 下会把
# 全角字符并入变量名导致 unbound variable（已踩过坑，勿回退成 $var）。
set -euo pipefail

# 从任何位置调用都回到仓库根（BIN/ENV_FILE 的相对路径以仓库根为基准）。
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."

ENV_FILE="${HUB_ENV_FILE:-.env}"
BIN="${AGENTHUB_BIN:-./agenthub}"

# load_env 解析 .env 并 export 白名单变量；无 .env 时静默（等价手动 export）。
load_env() {
  [[ -f "$ENV_FILE" ]] || return 0
  local line trimmed key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    # 跳过空行与整行注释。不做行内剥离：值里出现 # 是合法内容
    #（如口令文件路径），行内注释的取舍留给用户写文件时自己控制。
    trimmed="$(printf '%s' "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    [[ -z "$trimmed" || "$trimmed" == \#* ]] && continue
    [[ "$trimmed" =~ ^(HUB_[A-Z0-9_]+)=(.*)$ ]] || continue
    key="${BASH_REMATCH[1]}"
    value="${BASH_REMATCH[2]}"
    # 剥一层可选的单/双引号；值内部的引号与转义原样保留（不解析转义序列）。
    if [[ "$value" == \"*\" || "$value" == \'*\' ]]; then
      value="${value:1:${#value}-2}"
    fi
    export "$key=$value"
  done < "$ENV_FILE"
}

# ensure_db_password_file 启动前校验口令文件（仅 mysql/postgres 后端）。
# 文件缺失 = init 遗漏，明确报错；权限非 0600 = 自动收紧 + 警告（与 core 对
# control.db 的收紧理念一致：能修就修，修不了（只读挂载）也允许启动，由
# 部署侧审计兜底）；空文件报错。
ensure_db_password_file() {
  case "${HUB_DB_DRIVER:-sqlite}" in
    mysql | postgres) ;;
    *) return 0 ;;
  esac
  local pw="${HUB_DB_PASSWORD_FILE:-}"
  if [[ -z "$pw" ]]; then
    echo "run.sh: HUB_DB_DRIVER=${HUB_DB_DRIVER} 需要配置 HUB_DB_PASSWORD_FILE（见 .env.example）" >&2
    exit 1
  fi
  if [[ ! -f "$pw" ]]; then
    echo "run.sh: 数据库口令文件不存在：${pw}" >&2
    echo "请先执行：./scripts/run.sh init" >&2
    exit 1
  fi
  local mode
  mode="$(stat -f %Lp "$pw" 2>/dev/null || stat -c %a "$pw" 2>/dev/null)"
  if [[ "$mode" != "600" ]]; then
    if chmod 600 "$pw" 2>/dev/null; then
      echo "run.sh: 已把口令文件权限收紧到 600（原 ${mode}）：${pw}" >&2
    else
      echo "run.sh: 警告：无法收紧口令文件权限（当前 ${mode}，期望 600；只读挂载/卷场景请部署侧处理）：${pw}" >&2
    fi
  fi
  if [[ ! -s "$pw" ]]; then
    echo "run.sh: 口令文件为空：${pw}（请重新执行 ./scripts/run.sh init --force）" >&2
    exit 1
  fi
}

# cmd_init 交互式创建数据库口令文件：目录 0700、文件 0600、临时文件+原子替换，
# 避免写一半被杀留下半截口令。已存在时拒绝覆盖（防误改导致连不上库）。
cmd_init() {
  load_env
  case "${HUB_DB_DRIVER:-sqlite}" in
    mysql | postgres) ;;
    *)
      echo "run.sh init: 当前 HUB_DB_DRIVER=${HUB_DB_DRIVER:-sqlite} 无需数据库口令文件（仅 mysql/postgres 需要）" >&2
      return 0
      ;;
  esac
  local pw="${HUB_DB_PASSWORD_FILE:-}"
  if [[ -z "$pw" ]]; then
    echo "run.sh init: 请先在 .env 配置 HUB_DB_PASSWORD_FILE（参考 .env.example）" >&2
    exit 1
  fi
  if [[ -f "$pw" && "${1:-}" != "--force" ]]; then
    echo "run.sh init: 口令文件已存在：${pw}（确认要换口令请用 ./scripts/run.sh init --force）" >&2
    exit 1
  fi
  local dir
  dir="$(dirname "$pw")"
  if ! mkdir -p "$dir" 2>/dev/null; then
    echo "run.sh init: 无法创建目录 ${dir}（/run/secrets 等系统路径可能需要 root）" >&2
    exit 1
  fi
  chmod 700 "$dir" 2>/dev/null || true
  local p1 p2
  printf '数据库口令（不回显，建议 ≥16 字符）: ' >&2
  read -rs p1
  echo >&2
  printf '再次输入确认: ' >&2
  read -rs p2
  echo >&2
  if [[ -z "$p1" || "$p1" != "$p2" ]]; then
    echo "run.sh init: 两次输入不一致或为空，未写入。" >&2
    exit 1
  fi
  local tmp
  tmp="$(mktemp "$dir/.db-pass.XXXXXX")"
  printf '%s\n' "$p1" >"$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$pw"
  echo "run.sh init: 口令文件已写入 ${pw}（权限 600，目录 700）" >&2
  echo "下一步：用同一口令在数据库中创建账号，例如：" >&2
  echo '  MySQL     : CREATE USER '\''hub'\''@'\''%'\'' IDENTIFIED BY '\''<口令>'\''; GRANT ALL PRIVILEGES ON <库>.* TO '\''hub'\''@'\''%'\'';' >&2
  echo '  PostgreSQL: CREATE USER hub PASSWORD '\''<口令>'\''; GRANT ALL PRIVILEGES ON DATABASE <库> TO hub;' >&2
}

usage() {
  sed -n '2,20p' "$0"
  exit 0
}

case "${1:-}" in
  init)
    shift
    cmd_init "$@"
    ;;
  -h | --help | "")
    usage
    ;;
  *)
    load_env
    ensure_db_password_file
    if [[ ! -x "$BIN" ]]; then
      echo "run.sh: 可执行文件不存在 ${BIN}（请先 make build，或用 AGENTHUB_BIN 指定）" >&2
      exit 1
    fi
    exec "$BIN" "$@"
    ;;
esac

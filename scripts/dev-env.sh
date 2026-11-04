#!/usr/bin/env bash
# 开发环境自检与恢复（可重复执行）：Go 工具链、PostgreSQL 集群（flow 角色与 flow_dev / flow_test 库）、前端依赖。
# 容器重建后系统目录会丢失，所以：
#   - Go 安装包与 PostgreSQL 数据目录放在持久化目录 FLOW_DEV_HOME（默认 ~/.claude/envcache/flow）；
#   - 镜像里已有 Go / PostgreSQL 时直接使用，没有才下载 / apt 安装；
#   - 数据库用独立集群（pg_ctl 直接管理，不依赖系统服务），数据随 FLOW_DEV_HOME 保留。
# 用法：scripts/dev-env.sh            # 恢复并启动
#       scripts/dev-env.sh --check    # 只检查，不安装
#       eval "$(scripts/dev-env.sh --env)"  # 输出 PATH / 数据库地址等环境变量
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FLOW_DEV_HOME="${FLOW_DEV_HOME:-$HOME/.claude/envcache/flow}"
GO_VERSION="${GO_VERSION:-$(awk '/^go /{print $2}' "$ROOT/go.mod")}"
GO_MIRRORS="${GO_MIRRORS:-https://mirrors.aliyun.com/golang https://golang.google.cn/dl https://go.dev/dl}"
PG_PORT="${PG_PORT:-5432}"
PGDATA_DIR="$FLOW_DEV_HOME/pgdata"
PG_SOCK="$FLOW_DEV_HOME/pgsock"
LOCAL_GO="$HOME/.local/go"

log() { printf '[dev-env] %s\n' "$*" >&2; }

go_bin() {
  for d in /usr/local/go/bin "$LOCAL_GO/bin"; do
    if [ -x "$d/go" ] && "$d/go" version 2>/dev/null | grep -q "go$GO_VERSION"; then echo "$d"; return 0; fi
  done
  return 1
}

pg_bin() {
  local d
  d=$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1)
  [ -n "$d" ] && [ -x "$d/pg_ctl" ] && echo "$d"
}

print_env() {
  local gb pb
  gb=$(go_bin || true)
  pb=$(pg_bin || true)
  echo "export PATH=\"${gb:+$gb:}${pb:+$pb:}\$PATH\""
  echo "export GOTOOLCHAIN=local"
  echo "export GOPROXY=\"\${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}\""
  echo "export FLOW_DEV_DATABASE_URL='postgres://flow:flow@127.0.0.1:$PG_PORT/flow_dev?sslmode=disable'"
  echo "export FLOW_TEST_DATABASE_URL='postgres://flow:flow@127.0.0.1:$PG_PORT/flow_test?sslmode=disable'"
}

ensure_go() {
  if go_bin >/dev/null; then return 0; fi
  [ "${CHECK_ONLY:-}" ] && { log "缺少 Go $GO_VERSION"; return 1; }
  local tgz="go$GO_VERSION.linux-amd64.tar.gz" cache="$FLOW_DEV_HOME/cache"
  mkdir -p "$cache"
  if [ ! -s "$cache/$tgz" ]; then
    for m in $GO_MIRRORS; do
      log "下载 $m/$tgz"
      if curl -fsSL --retry 2 --connect-timeout 15 -o "$cache/$tgz.part" "$m/$tgz"; then mv "$cache/$tgz.part" "$cache/$tgz"; break; fi
    done
  fi
  [ -s "$cache/$tgz" ] || { log "Go 下载失败"; return 1; }
  rm -rf "$LOCAL_GO" && mkdir -p "$(dirname "$LOCAL_GO")"
  tar -C "$(dirname "$LOCAL_GO")" -xzf "$cache/$tgz"
  mkdir -p "$HOME/.local/bin" && ln -sf "$LOCAL_GO/bin/go" "$LOCAL_GO/bin/gofmt" "$HOME/.local/bin/"
  log "已安装 $("$LOCAL_GO/bin/go" version)"
}

ensure_pg_packages() {
  if pg_bin >/dev/null; then return 0; fi
  [ "${CHECK_ONLY:-}" ] && { log "缺少 PostgreSQL"; return 1; }
  log "apt 安装 PostgreSQL"
  sudo -n apt-get update -q >/dev/null
  # 只要二进制：不让包脚本创建 / 启动默认集群
  sudo -n mkdir -p /etc/postgresql-common
  echo "create_main_cluster = false" | sudo -n tee /etc/postgresql-common/createcluster.conf >/dev/null
  sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -q postgresql >/dev/null
  pg_bin >/dev/null || { log "PostgreSQL 安装失败"; return 1; }
}

pg_running() { "$1/pg_ctl" -D "$PGDATA_DIR" status >/dev/null 2>&1; }

ensure_pg_cluster() {
  local pb
  pb=$(pg_bin)
  mkdir -p "$PG_SOCK"
  if [ ! -s "$PGDATA_DIR/PG_VERSION" ]; then
    [ "${CHECK_ONLY:-}" ] && { log "数据库集群未初始化"; return 1; }
    log "初始化数据库集群 $PGDATA_DIR"
    "$pb/initdb" -D "$PGDATA_DIR" -U postgres --auth-local=trust --auth-host=scram-sha-256 -E UTF8 --locale=C.UTF-8 >/dev/null
    # 用户名 postgres 的超级用户只允许本地 socket 访问；flow 走 TCP 密码
    echo "host all all 127.0.0.1/32 trust" >> "$PGDATA_DIR/pg_hba.conf"
  fi
  if ! pg_running "$pb"; then
    [ "${CHECK_ONLY:-}" ] && { log "数据库未运行"; return 1; }
    # 系统服务（apt 默认集群）占用端口时先停掉
    if (exec 3<>"/dev/tcp/127.0.0.1/$PG_PORT") 2>/dev/null; then
      log "端口 $PG_PORT 被其他 PostgreSQL 占用，停止系统服务"
      sudo -n service postgresql stop >/dev/null 2>&1 || true
      sleep 1
    fi
    rm -f "$PGDATA_DIR/postmaster.pid"
    "$pb/pg_ctl" -D "$PGDATA_DIR" -l "$FLOW_DEV_HOME/postgres.log" -w \
      -o "-p $PG_PORT -k $PG_SOCK -c listen_addresses=127.0.0.1" start >/dev/null
    log "数据库已启动（端口 $PG_PORT，数据 $PGDATA_DIR）"
  fi
  local psql=("$pb/psql" -h "$PG_SOCK" -p "$PG_PORT" -U postgres -qtA)
  if [ "$("${psql[@]}" -c "SELECT 1 FROM pg_roles WHERE rolname='flow'")" != 1 ]; then
    "${psql[@]}" -c "CREATE ROLE flow LOGIN PASSWORD 'flow' SUPERUSER" >/dev/null
  fi
  for db in flow_dev flow_test; do
    if [ "$("${psql[@]}" -c "SELECT 1 FROM pg_database WHERE datname='$db'")" != 1 ]; then
      "${psql[@]}" -c "CREATE DATABASE $db OWNER flow" >/dev/null && log "已创建数据库 $db"
    fi
  done
}

ensure_web_deps() {
  [ -d "$ROOT/web/node_modules/vite" ] && return 0
  [ "${CHECK_ONLY:-}" ] && { log "缺少前端依赖"; return 1; }
  log "安装前端依赖"
  (cd "$ROOT/web" && npm ci --include=dev --no-audit --no-fund >/dev/null)
}

case "${1:-}" in
  --env) print_env; exit 0 ;;
  --check) CHECK_ONLY=1 ;;
  "") ;;
  *) echo "用法：$0 [--check|--env]" >&2; exit 2 ;;
esac

mkdir -p "$FLOW_DEV_HOME"
# 同一时间只跑一个（SessionStart 钩子与手动执行可能同时触发）；持久化目录在 9p 挂载上不支持 flock，锁放在内存文件系统
exec 9>"${FLOW_DEV_LOCK:-/dev/shm/flow-dev-env.lock}"
flock -w 300 9 || log "拿不到锁，继续执行"

status=0
ensure_go || status=1
ensure_pg_packages && ensure_pg_cluster || status=1
ensure_web_deps || status=1
if [ $status = 0 ]; then log "就绪：$("$(go_bin)/go" version | awk '{print $3}')，PostgreSQL $("$(pg_bin)/postgres" --version | awk '{print $3}') @127.0.0.1:$PG_PORT"; fi
exit $status

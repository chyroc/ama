#!/usr/bin/env bash
#
# Build and (re)start AMA in one step: frontend build → embed into the backend → a single binary listening on 0.0.0.0:5273.
#
#   ./dev.sh                # build + restart
#   ./dev.sh --skip-web     # only Go code changed, skip the frontend build (much faster)
#   ./dev.sh --stop         # only stop, do not start
#   ./dev.sh --logs         # follow the log
#
# Running it again is a restart: stop the old process first, then start the new one, so ports never clash.
#
# # Why a single binary
#
# The frontend build is embedded into the backend with go:embed (see server/httpd/static.go), so there is only one process,
# one port, and same origin by nature: no vite to start, no CORS to configure, no proxy target to manage.
# The cost is that **frontend changes require re-running this script** (no HMR any more), about 4 seconds each time.
#
# The port defaults to 5273; change it with PORT=xxxx ./dev.sh. ⚠️ Before starting a new process this script cleans up old processes by port
# (regardless of whose process it is), so when running several copies on one machine each must use a different PORT, and --stop must use the same PORT too.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

readonly ROOT="$PWD"
readonly RUN_DIR="$ROOT/.run"
readonly PID_FILE="$RUN_DIR/amad.pid"
readonly LOG_FILE="$RUN_DIR/amad.log"
readonly BIN="$RUN_DIR/amad"
readonly STATIC_DIR="$ROOT/server/httpd/static"
readonly PORT="${PORT:-5273}"

mkdir -p "$RUN_DIR"

# Use colors only when attached to a terminal, so no escape codes leak into redirected files.
if [[ -t 1 ]]; then
  C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_OK=''; C_WARN=''; C_ERR=''; C_DIM=''; C_OFF=''
fi
step() { printf '%s==>%s %s\n' "$C_OK" "$C_OFF" "$*"; }
warn() { printf '%s警告:%s %s\n' "$C_WARN" "$C_OFF" "$*" >&2; }
die()  { printf '%s错误:%s %s\n' "$C_ERR" "$C_OFF" "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Stop
# ---------------------------------------------------------------------------

# stop_server stops by pidfile first, then falls back to the port.
#
# Both paths are needed: the pidfile goes stale after the process is kill -9'd, the machine reboots, or it was started by hand in another terminal;
# relying only on the pidfile would then "show stopped while the port is still taken", and the new process would fail to start with
# address already in use, an error that does not point to the real cause (the old process was not fully stopped).
stop_server() {
  local stopped=0

  if [[ -f "$PID_FILE" ]]; then
    local pid
    pid="$(cat "$PID_FILE" 2>/dev/null || true)"
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      step "停止旧进程 (pid $pid)"
      kill "$pid" 2>/dev/null || true
      # Wait for it to finish by itself: on SIGTERM amad does a graceful shutdown (see httpd.Server.Run).
      for _ in $(seq 1 50); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
      done
      if kill -0 "$pid" 2>/dev/null; then
        warn "优雅关闭超时，强制结束 pid $pid"
        kill -9 "$pid" 2>/dev/null || true
      fi
      stopped=1
    fi
    rm -f "$PID_FILE"
  fi

  # Fallback: if something else still holds the port, clear it too (most likely one started by hand last time).
  local leftover
  leftover="$(port_pids)"
  if [[ -n "$leftover" ]]; then
    warn "端口 $PORT 仍被占用 (pid: $(echo "$leftover" | tr '\n' ' ')), 清理中"
    # shellcheck disable=SC2086
    kill $leftover 2>/dev/null || true
    sleep 0.5
    leftover="$(port_pids)"
    if [[ -n "$leftover" ]]; then
      # shellcheck disable=SC2086
      kill -9 $leftover 2>/dev/null || true
      sleep 0.3
    fi
    stopped=1
  fi

  [[ "$stopped" == 1 ]] && return 0 || return 1
}

# port_pids lists the pids of processes listening on $PORT.
#
# Only LISTEN state: clients connected to this port (like a TIME_WAIT from the last curl) should not be killed.
port_pids() {
  if command -v ss >/dev/null 2>&1; then
    ss -ltnpH "sport = :$PORT" 2>/dev/null |
      grep -oP 'pid=\K[0-9]+' | sort -u
  elif command -v lsof >/dev/null 2>&1; then
    lsof -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | sort -u
  fi
}

# ---------------------------------------------------------------------------
# Subcommands
# ---------------------------------------------------------------------------

SKIP_WEB=0
case "${1:-}" in
  --stop)
    stop_server && step "已停止" || step "本来就没在跑"
    exit 0
    ;;
  --logs)
    [[ -f "$LOG_FILE" ]] || die "还没有日志：$LOG_FILE"
    exec tail -f "$LOG_FILE"
    ;;
  --skip-web) SKIP_WEB=1 ;;
  "") ;;
  *) die "未知参数：$1（可用：--skip-web / --stop / --logs）" ;;
esac

# ---------------------------------------------------------------------------
# Terminal WebSocket: on by default locally
# ---------------------------------------------------------------------------
#
# The terminal's main path is a WebSocket; only when it cannot connect does it fall back to "SSE down + POST up".
# And that fallback path **echoes out of order when typing fast, with one round trip per character**: exactly the two bugs reported back then.
#
# ⚠️ **Without config, local always takes the fallback, with zero errors** (only an extra "compatibility mode" badge in the terminal status bar).
# So defaults are given here.
#
# Locally WS works: dev.sh starts HTTP/1.1, and the frontend and WS are **same origin** (the frontend build is go:embed'ed
# into the binary, one port only).
#
# The two variables each handle half; missing either one falls back:
#   VITE_TERMINAL_WS_BASE    tells the frontend where to connect (**replaced by a literal at build time**, so set it before building)
#   AMA_TERMINAL_WS_ORIGINS  the backend CheckOrigin allowlist (fail-closed when empty, rejecting all cross-origin requests)
#
# ⚠️ **The allowlist must contain both 127.0.0.1 and the LAN IP.** When opening
# `http://10.x.x.x:5273` from another machine the browser sends that IP as Origin, so allowing only 127.0.0.1 still gets rejected,
# and the symptom is still only "compatibility mode", with no error. This was hit in testing.
host_ip="$(ip -4 route get 1 2>/dev/null | grep -oP 'src \K\S+' || true)"

# Use ${VAR+set} instead of ${VAR:-...}: the colon form treats "set to empty string" as "not set",
# so `VITE_TERMINAL_WS_BASE= ./dev.sh` (deliberately turning WS off to debug the fallback path) would silently
# fall back to the default: that opening would not exist at all, with no error.
# ⚠️ **Pass `same-origin` instead of hard-coding `http://127.0.0.1:$PORT`.**
#
# `VITE_*` is **replaced by a literal at build time**, so once hard-coded that IP is baked into the js. And the browser may
# run on **another machine** (opened via the LAN IP); then `127.0.0.1` means the browser's own
# computer, where there is no amad; the symptom is `WebSocket connection to 'ws://127.0.0.1:5273/...' failed`.
#
# `same-origin` makes the frontend compute it at **runtime** from `location` (see wsBase in lib/terminalTransport.ts):
# it connects to whichever host it was opened from, so all three entry points work.
if [[ -z "${VITE_TERMINAL_WS_BASE+set}" ]]; then
  VITE_TERMINAL_WS_BASE="same-origin"
fi
export VITE_TERMINAL_WS_BASE

if [[ -z "${AMA_TERMINAL_WS_ORIGINS+set}" ]]; then
  AMA_TERMINAL_WS_ORIGINS="http://127.0.0.1:$PORT,http://localhost:$PORT"
  [[ -n "$host_ip" ]] && AMA_TERMINAL_WS_ORIGINS+=",http://$host_ip:$PORT"
fi
export AMA_TERMINAL_WS_ORIGINS

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

if [[ "$SKIP_WEB" == 1 ]]; then
  step "跳过前端构建 (--skip-web)"
  [[ -f "$STATIC_DIR/index.html" ]] || die "static/ 里没有产物，第一次跑不能用 --skip-web"
else
  step "构建前端"
  cd "$ROOT/web"
  [[ -d node_modules ]] || die "web/node_modules 不存在，先跑 npm install"
  # npm run build already chains check-icons and tsc; do not run them again here.
  npm run build

  step "把前端产物嵌进后端"
  cd "$ROOT"
  [[ -f web/dist/index.html ]] || die "web/dist/index.html 不存在，前端构建似乎没出产物"
  # Clear before copying: old renamed chunks (assets/index-oldhash.js) would keep piling up in the binary if not cleared,
  # making it a bit bigger with every build, with no symptom to tell you.
  # Keep the two files that are checked in: .gitignore and placeholder.html (deleting them makes git show deletions,
  # and people who never ran dev.sh lose that explanation page).
  find "$STATIC_DIR" -mindepth 1 \
    -not -name '.gitignore' -not -name 'placeholder.html' -delete
  cp -r web/dist/. "$STATIC_DIR/"
  printf '%s    产物 %s，共 %s 个文件%s\n' "$C_DIM" \
    "$(du -sh "$STATIC_DIR" | cut -f1)" \
    "$(find "$STATIC_DIR" -type f -not -name '.gitignore' | wc -l)" "$C_OFF"
fi

step "构建后端（含嵌入的前端）"
cd "$ROOT/server"
# Build to a temporary name, then replace atomically: writing $BIN directly could be blocked by "text file busy" from the running process,
# or worse: if the build fails, the old binary is already truncated, and the service can neither start nor go back.
go build -o "$BIN.tmp" ./cmd/amad
mv -f "$BIN.tmp" "$BIN"
printf '%s    %s%s\n' "$C_DIM" "$(du -h "$BIN" | cut -f1)" "$C_OFF"

# ---------------------------------------------------------------------------
# Restart
# ---------------------------------------------------------------------------

cd "$ROOT"
stop_server || true

# Local credentials (Ark API Key etc.). It starts without them, but chats will not run.
if [[ -f "$ROOT/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT/.env"
  set +a
else
  warn ".env 不存在，方舟相关功能会不可用"
fi

step "启动服务"
# Neither of these env vars may be left out; leaving one out "silently switches to different behavior" instead of erroring:
#   AMA_ADDR      — listen address, follows $PORT
#   AMA_DEV_ROUTES— mounts /api/_debug/reset, which the frontend e2e helpers need
# AMA_ALLOWED_ORIGIN is not needed: frontend and backend are same origin, no cross-origin.
#
# **Local development also requires a real login** (local account or OIDC); there is no "skip login locally" switch. The account
# registered on the first open automatically becomes the platform admin. The bypass is only for callers that "are not people" (e2e / CI / scripts),
# through the AMA_API_KEY + AMA_API_KEY_EMAIL channel.
#
# To skip login locally too (e.g. only working on the UI), two ways:
#   1. Run the frontend with mocks: cd web && VITE_API_MODE=local ./scripts/dev.sh
#   2. Pass an API Key to this script:
#      AMA_API_KEY=dev-key AMA_API_KEY_EMAIL=alice@example.com ./dev.sh
#      (then send requests with Authorization: Bearer dev-key; the browser still needs to log in, as it does not send this header)
cd "$ROOT/server"
AMA_ADDR="0.0.0.0:$PORT" \
AMA_TERMINAL_WS_ORIGINS="$AMA_TERMINAL_WS_ORIGINS" \
AMA_DEV_ROUTES="${AMA_DEV_ROUTES:-1}" \
  nohup "$BIN" >>"$LOG_FILE" 2>&1 &
echo $! >"$PID_FILE"
cd "$ROOT"

# Wait until it can really serve before reporting success. Sleeping a fixed number of seconds would show "started successfully"
# even when it fails to start (e.g. exits immediately because it cannot reach the database), and you would only find it empty in the browser.
step "等待就绪"
ready=0
for _ in $(seq 1 100); do
  if curl -fsS -m 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  # If the process has already exited there is no need to wait; print the tail of the log right away.
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  if [[ -z "$pid" ]] || ! kill -0 "$pid" 2>/dev/null; then
    printf '\n%s--- %s 末尾 ---%s\n' "$C_DIM" "$LOG_FILE" "$C_OFF"
    tail -20 "$LOG_FILE" 2>/dev/null || true
    die "进程启动后立刻退出了（最常见原因：连不上 PostgreSQL，试试 docker compose up -d）"
  fi
  sleep 0.1
done
[[ "$ready" == 1 ]] || die "$PORT 端口 10 秒内没就绪，看日志：./dev.sh --logs"

# host_ip was already computed in the "Terminal WebSocket" section above (the allowlist needs it); reuse it here.
printf '\n%s就绪%s  pid %s\n' "$C_OK" "$C_OFF" "$(cat "$PID_FILE")"
printf '  本机   http://127.0.0.1:%s\n' "$PORT"
[[ -n "$host_ip" ]] && printf '  局域网 http://%s:%s\n' "$host_ip" "$PORT"
if [[ -n "$VITE_TERMINAL_WS_BASE" ]]; then
  printf '  终端 WS %s（白名单 %s）\n' "$VITE_TERMINAL_WS_BASE" "$AMA_TERMINAL_WS_ORIGINS"
else
  printf '  终端 WS 已关闭 —— 终端将走 SSE 回退（连打可能乱序）\n'
fi
printf '%s  日志 ./dev.sh --logs   停止 ./dev.sh --stop   只改 Go 用 ./dev.sh --skip-web%s\n' \
  "$C_DIM" "$C_OFF"

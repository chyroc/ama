#!/usr/bin/env bash
# Smoke test: really say one sentence to the agent and wait for its reply.
#
# # Why a separate script
#
# setup.sh only proves "the resources were created", which is a different thing from "it works":
# credentials not configured, a wrong model id,
# the worker not running; resources still get created in all those cases. There is only one way to
# judge "it works": **really send a message,
# and really wait for the reply**.
#
# # Why SSE must be open, rather than only polling events
#
# We learned this the hard way; do not "simplify" it away. Polling `GET /api/runs/{id}/events` looks
# good enough,
# but that endpoint only backfills events from Ark when `run.Status == running`
# (the same check in three places: ListRunEvents / GetRun in core/ark.go, and StreamRunEvents in
# core/chat.go).
# And once this turn's stop_reason is **requires_action** (i.e.
# "the tool is running in our container"; see item 2 in /llms.txt), the run is marked
# `requires_action`, **which is not running, so the backfill never happens again**.
#
# The symptom is very misleading: all tools in the container ran successfully (the log has
# `工具执行完成 失败=false`, i.e. "tool finished, failed=false",
# and `posted=true`), and the turn on the Ark side also ended normally, but AMA's events stay forever
# at
# the "agent.tool_use" one, so the script reports "timed out, no agent.message received":
# **the error points at the worker or credentials, while the real cause is that events were not
# synced back**.
#
# With SSE open this check does not matter: frames are pushed straight from Ark, persistArkEvent
# stores and forwards them,
# the same path as the chat page in the browser. So here we **open the stream first, then send the
# message**.
#
# ⚠️ By the way: `requires_action` is not "waiting for user confirmation"; the right reaction is to
# keep waiting.
# The first tool call waits for the container to be lazily created/resumed, so the timeout is wider
# than you might expect.
#
# Usage:
#   AMA_API_KEY=<key> AGENT_ID=<agent_x> scripts/setup-ark-agent/smoke-test.sh
#   AMA_BASE_URL=https://ama.example.com AMA_API_KEY=amak_xxx AGENT_ID=... \
#     scripts/setup-ark-agent/smoke-test.sh "what is in the current directory?"
set -euo pipefail

BASE_URL="${AMA_BASE_URL:-http://127.0.0.1:5273}"
PROMPT="${1:-你好，用一句话说明你是谁、以及你手上有哪些技能。}"
TIMEOUT="${SMOKE_TIMEOUT:-180}"

die() { printf '\033[31m错误\033[0m %s\n' "$*" >&2; exit 1; }
step() { printf '\n\033[36m==>\033[0m %s\n' "$*"; }

[[ -n "${AMA_API_KEY:-}" ]] || die "缺 AMA_API_KEY"
[[ -n "${AGENT_ID:-}" ]] || die "缺 AGENT_ID（setup.sh 结束时打印了）"
command -v jq >/dev/null || die "缺 jq"

auth=(-H "Authorization: Bearer $AMA_API_KEY")

# Which workspace the agent belongs to must be asked, not guessed: guessing wrong gives 404 or 403,
# and that error will not tell you the workspace was wrong (see item 4 in /llms.txt).
step "取 agent $AGENT_ID"
agent="$(curl -sS "${auth[@]}" "$BASE_URL/api/agents/$AGENT_ID")"
WS="$(printf '%s' "$agent" | jq -r '.workspaceId // empty')"
[[ -n "$WS" ]] || die "取不到 agent：$(printf '%s' "$agent" | jq -r '.message // .')"
printf '    空间 %s，模型 %s\n' "$WS" "$(printf '%s' "$agent" | jq -r '.model.id')"

step "开对话"
run="$(curl -sS -X POST "$BASE_URL/api/workspaces/$WS/conversations" "${auth[@]}" \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg a "$AGENT_ID" '{agentId:$a, title:"冒烟测试"}')")"
RUN="$(printf '%s' "$run" | jq -r '.id // empty')"
[[ -n "$RUN" ]] || die "开对话失败：$(printf '%s' "$run" | jq -r '.message // .')"
printf '    run %s\n' "$RUN"

# -- Open the stream first, then send the message ----------------------------
#
# The order must not be reversed; for why see the file header: once the run enters requires_action,
# the polling path never backfills
# events again, and only SSE can still get later frames.
#
# SSE only accepts an API Key, and it must go in a query parameter (EventSource cannot send custom
# headers;
# see "the SSE exception" in /llms.txt).
step "开 SSE"
SSE_OUT="$(mktemp -t smoke-sse-XXXXXX.jsonl)"
curl -sS -N "$BASE_URL/api/runs/$RUN/events/stream?api_key=$AMA_API_KEY" \
  >"$SSE_OUT" 2>/dev/null &
SSE_PID=$!
# Clean up however it exits, otherwise curl keeps hanging in the background writing the temp file.
cleanup() { kill "$SSE_PID" 2>/dev/null || true; rm -f "$SSE_OUT"; }
trap cleanup EXIT
sleep 2
kill -0 "$SSE_PID" 2>/dev/null || die "SSE 开不起来（key 不对？只有 API Key 能走这条）"
printf '    已连上\n'

step "发消息"
printf '    「%s」\n' "$PROMPT"
sent="$(curl -sS -X POST "$BASE_URL/api/runs/$RUN/events" "${auth[@]}" \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg t "$PROMPT" \
    '{events:[{type:"user.message", content:[{type:"text", text:$t}]}]}')")"
# A successful send returns an empty body or an event array; only a message field means an error.
err="$(printf '%s' "$sent" | jq -r 'if type=="object" then .message // empty else empty end' 2>/dev/null || true)"
[[ -z "$err" ]] || die "发消息失败：$err"

step "等回复（最多 ${TIMEOUT}s）"
deadline=$(( SECONDS + TIMEOUT ))
shown=0

# frames decodes the frames SSE wrote down into one JSON per line. The `data: ` prefix must be
# stripped.
frames() {
  sed -n 's/^data: //p' "$SSE_OUT" 2>/dev/null \
    | jq -c 'select(type=="object")' 2>/dev/null || true
}

while (( SECONDS < deadline )); do
  sleep 3

  all="$(frames)"
  [[ -z "$all" ]] && continue

  # Only print the newly appeared lines. **Take them by line offset, do not use comm**: comm requires
  # both sides to be
  # sorted, while events come in arrival order, and feeding them to it prints a mess of jumbled
  # duplicate lines
  # (the first version did that, and it looked like events were replayed twice).
  total="$(printf '%s\n' "$all" | wc -l)"
  if (( total > shown )); then
    printf '%s\n' "$all" | tail -n +$(( shown + 1 )) \
      | jq -r '"    " + (.type // "?")'
    shown="$total"
  fi

  # Whether this turn is finished: three things must hold together:
  #
  #  1. there is agent.message text;
  #  2. **every agent.tool_use has a matching user.tool_result**: a pending tool call
  #     means it is not done;
  #  3. the last frame is a session-level idle.
  #
  # Looking only at 1 would stop too early: the model often first says "let me look at that
  # directory" and then starts a tool_use,
  # and that opening line is also an agent.message. Exiting on it alone would print half a sentence
  # and report status as
  # requires_action, looking like a failure when it really just stopped too early (the first version
  # did that, and it "passed", which is worse).
  #
  # Looking only at 3 is not enough either: during tool calls status flips back and forth between
  # running ⇄ idle
  # (see item 2 in /llms.txt), so there are idles midway.
  read -r n_use n_result last <<<"$(printf '%s\n' "$all" | jq -rs '
    [ (map(select(.type=="agent.tool_use")) | length),
      (map(select(.type=="user.tool_result")) | length),
      (last.type // "-") ] | @tsv')"
  reply="$(printf '%s\n' "$all" | jq -rs '
    [ .[] | select(.type=="agent.message") | .content // [] | .[]
      | select(.type=="text") | .text ] | join("")')"

  if [[ -n "$reply" && "$reply" != "null" ]] \
     && (( n_use == n_result )) \
     && [[ "$last" == "session.status_idle" ]]; then
    step "agent 回复"
    printf '%s\n' "$reply"
    status="$(curl -sS "${auth[@]}" "$BASE_URL/api/runs/$RUN" | jq -r '.status')"
    printf '\n\033[32m通过\033[0m run %s，最终 status=%s（工具调用 %s 次）\n' \
      "$RUN" "$status" "$n_use"
    exit 0
  fi
done

# On timeout, print the scene. Saying only "timed out" leaves no way to look further.
status="$(curl -sS "${auth[@]}" "$BASE_URL/api/runs/$RUN" | jq -r '.status // "?"')"
printf '\n' >&2
printf '\033[31m超时\033[0m %ss 内没等到 agent.message。run=%s status=%s\n' \
  "$TIMEOUT" "$RUN" "$status" >&2
printf 'SSE 收到的事件类型：\n' >&2
sed -n 's/^data: //p' "$SSE_OUT" 2>/dev/null \
  | jq -rc 'select(type=="object") | .type // empty' 2>/dev/null \
  | sort | uniq -c >&2 || printf '  （一条都没有）\n' >&2
cat >&2 <<'HINT'

按这个顺序查：
  只有 agent.tool_use 没有后续   工具在容器里跑但结果没回来。看服务日志里
                                 「工具执行完成」那几行：失败=false 说明工具没问题，
                                 那就是方舟侧没继续推帧。
  一条事件都没有                 方舟凭证无效，或模型 id 不存在。
                                 跑 POST /api/workspaces/<ws>/ark-credential/verify 看 check 字段。
  SSE 一开就断 / 工具调用报错     执行端没起。看启动日志里 Docker 执行端那一行
                                 （连不上 docker.sock 或会话镜像没构建会明说）。
HINT
exit 1

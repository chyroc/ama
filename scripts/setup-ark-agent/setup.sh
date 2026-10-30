#!/usr/bin/env bash
# Creates the ark-agent workspace on AMA, together with its agent and skills, in one go.
#
# # Idempotent
#
# Running the same command again and again is safe: the workspace is reused by key, skills and the
# agent are updated when found by name.
# This is not for convenience: **creating an agent has remote side effects** (it creates an Agent and
# uploads skill packages on the Ark side),
# and "running it a second time creates another copy" would make people afraid to rerun, so any
# failure would need manual cleanup.
#
# # Why the API and not clicking through the UI
#
# The prompt is 5 KB and the skill has 20 attachment files; pasting them into forms by hand is slow
# and error-prone. And this script
# is itself the source of truth for that config: changing the prompt means changing the files under
# assets/, and rerunning once applies it.
#
# Usage:
#
#   # Local (the service started by ./dev.sh)
#   AMA_API_KEY=<key> scripts/setup-ark-agent/setup.sh
#
#   # A deployed service
#   AMA_BASE_URL=https://ama.example.com AMA_API_KEY=amak_xxx \
#     scripts/setup-ark-agent/setup.sh
#
#   # Only show what would be done, without changing anything
#   AMA_API_KEY=<key> scripts/setup-ark-agent/setup.sh --dry-run
#
# Environment variables:
#
#   AMA_API_KEY    Required. An amak_... key created on the settings page; locally the AMA_API_KEY
#                  in .env also works.
#   AMA_BASE_URL   Default http://127.0.0.1:5273 (the address of local dev.sh).
#   ARK_API_KEY    Optional. If given, the Ark credentials are also written into this workspace and
#                  verified;
#                  **without it the agent can be created but cannot start a conversation**.
#   WORKSPACE_KEY  Default ark-agent.
#   AGENT_MODEL    Default doubao-seed-2-1-pro-260628.
set -euo pipefail

cd "$(dirname "$0")"
ASSETS="$PWD/assets"

BASE_URL="${AMA_BASE_URL:-http://127.0.0.1:5273}"
WORKSPACE_KEY="${WORKSPACE_KEY:-ark-agent}"
WORKSPACE_NAME="${WORKSPACE_NAME:-Ark Agent}"
AGENT_NAME="${AGENT_NAME:-ark-agent}"
AGENT_MODEL="${AGENT_MODEL:-doubao-seed-2-1-pro-260628}"

DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

die() { printf '\033[31m错误\033[0m %s\n' "$*" >&2; exit 1; }
step() { printf '\n\033[36m==>\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }

[[ -n "${AMA_API_KEY:-}" ]] || die "缺 AMA_API_KEY。本地可以 source .env，或到设置页创建一个。"
command -v jq >/dev/null || die "缺 jq"
command -v python3 >/dev/null || die "缺 python3（用来打技能 zip）"

# api sends one JSON request and returns "<body>\n<http_code>".
#
# **No curl -f**: with it the body is dropped on error, while AMA's error info is all in the body's
# message
# field (see "check code, not status" in /llms.txt); dropping it leaves only a status code,
# which does not point at the real cause at all when debugging.
api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -X "$method" "$BASE_URL$path"
    -H "Authorization: Bearer $AMA_API_KEY"
    -w '\n%{http_code}')
  [[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' -d "$body")
  curl "${args[@]}"
}

# check splits the output of api: on success returns the body, on failure reports it along with the
# message.
check() {
  local out="$1" what="$2"
  local code="${out##*$'\n'}" body="${out%$'\n'*}"
  case "$code" in
    2*) printf '%s' "$body" ;;
    *)
      local msg
      msg="$(printf '%s' "$body" | jq -r '.message // empty' 2>/dev/null || true)"
      die "$what 失败（HTTP $code）${msg:+：$msg}"
      ;;
  esac
}

if (( DRY_RUN )); then
  step "dry-run：只打印将要做的事"
  info "目标        $BASE_URL"
  info "空间        $WORKSPACE_KEY（$WORKSPACE_NAME）"
  info "agent       $AGENT_NAME，模型 $AGENT_MODEL"
  info "提示词      $(wc -c <"$ASSETS/system-prompt.md") 字节"
  for d in "$ASSETS"/skills/*/; do
    info "技能        $(basename "$d")，$(find "$d" -type f | wc -l) 个文件"
  done
  # ⚠️ Do not write it as `${ARK_API_KEY:+provided}${ARK_API_KEY:-not provided}`: when the variable
  # is non-empty the second half
  # expands to **the key itself**, so dry-run would print the credential to the terminal (the first
  # version did exactly that,
  # and it looked like just an ordinary line of output).
  if [[ -n "${ARK_API_KEY:-}" ]]; then
    info "方舟凭证    会写入并校验"
  else
    info "方舟凭证    未提供（agent 能建但开不了对话）"
  fi
  exit 0
fi

step "连通性"
health="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$BASE_URL/healthz" 2>&1)" \
  || die "连不上 $BASE_URL（$health）。确认地址与网络可达。"
[[ "$health" == "200" ]] || die "$BASE_URL/healthz 回 $health，服务可能没起"
info "$BASE_URL 可达"

# -- Workspace ---------------------------------------------------------------
#
# Look it up by key first, then decide whether to create. The TOCTOU here is acceptable: on a
# collision creating the workspace fails because the key is unique,
# and the failure is loud (not silently creating a second one).
step "空间 $WORKSPACE_KEY"
existing="$(api GET "/api/workspace-by-key?key=$WORKSPACE_KEY")"
if [[ "${existing##*$'\n'}" == "200" ]]; then
  WS="$(printf '%s' "${existing%$'\n'*}" | jq -r '.id')"
  info "已存在，复用 $WS"
else
  created="$(check "$(api POST /api/workspaces "$(jq -n \
    --arg n "$WORKSPACE_NAME" --arg k "$WORKSPACE_KEY" \
    '{name:$n, key:$k, description:"通用 agent 提示词与飞书卡片技能"}')")" "建空间")"
  WS="$(printf '%s' "$created" | jq -r '.id')"
  info "已建 $WS"
fi

# -- Ark credentials ---------------------------------------------------------
#
# Placed before creating the agent: creating the agent syncs to the Ark side, and without
# credentials that step only WARNs and does not fail,
# so "the agent is created but cannot start a conversation", and that symptom does not point at
# missing credentials.
if [[ -n "${ARK_API_KEY:-}" ]]; then
  step "方舟凭证"
  check "$(api PUT "/api/workspaces/$WS/ark-credential" \
    "$(jq -n --arg k "$ARK_API_KEY" '{apiKey:$k, region:"cn-beijing"}')")" "存方舟凭证" >/dev/null
  verdict="$(check "$(api POST "/api/workspaces/$WS/ark-credential/verify")" "校验方舟凭证")"
  # A failed verification **is also 200**; the conclusion is in the check field (see /llms.txt);
  # judging by status code would miss it.
  result="$(printf '%s' "$verdict" | jq -r '.check')"
  [[ "$result" == "valid" ]] || die "方舟凭证校验为 $result，对话会开不起来"
  info "valid"
else
  step "方舟凭证"
  info "未提供 ARK_API_KEY——跳过。⚠️ 没有它 agent 建得出来但开不了对话。"
fi

# -- Skills ------------------------------------------------------------------
#
# Each skill directory is zipped and sent to /skills/upload. **Not POST /skills**: that one only takes
# instructions text, attachments cannot be sent, so the guidance in SKILL.md to read references/
# would all come to nothing: the agent cannot read the files and just retries, with zero errors
# (server/shared/infra/shelltools/skills.go).
step "技能"
SKILL_IDS=()
existing_skills="$(check "$(api GET "/api/workspaces/$WS/skills")" "列技能")"
for dir in "$ASSETS"/skills/*/; do
  name="$(basename "$dir")"
  [[ -f "$dir/SKILL.md" ]] || die "$name 缺 SKILL.md"

  # If it already exists, delete it first: upload only creates new ones, and uploading the same name
  # again gives a second copy.
  old="$(printf '%s' "$existing_skills" | jq -r --arg n "$name" '.[] | select(.name==$n) | .id')"
  if [[ -n "$old" ]]; then
    check "$(api DELETE "/api/skills/$old")" "删旧技能 $name" >/dev/null
    info "$name：删掉旧的 $old"
  fi

  zip="$(mktemp -t "skill-$name-XXXXXX.zip")"
  # python rather than zip(1): many machines do not have zip installed, while python3 is usually
  # there.
  # Wrap a top-level directory with the same name as the skill: Ark recommends `foo.zip →
  # foo/SKILL.md`,
  # and AMA strips that layer when parsing (commonTopDir in server/service/skill/skillpkg.go).
  python3 - "$dir" "$zip" "$name" <<'PY'
import sys, zipfile
from pathlib import Path
src, out, name = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    for p in sorted(src.rglob("*")):
        if p.is_file():
            z.write(p, f"{name}/{p.relative_to(src).as_posix()}")
PY

  out="$(curl -sS -X POST "$BASE_URL/api/workspaces/$WS/skills/upload" \
    -H "Authorization: Bearer $AMA_API_KEY" -F "file=@$zip" -w '\n%{http_code}')"
  rm -f "$zip"
  sk="$(check "$out" "上传技能 $name")"
  id="$(printf '%s' "$sk" | jq -r '.id')"
  SKILL_IDS+=("$id")
  info "$name：$id（$(printf '%s' "$sk" | jq -r '.files | length') 个附件）"
done

# -- Agent -------------------------------------------------------------------
step "agent $AGENT_NAME"
system="$(cat "$ASSETS/system-prompt.md")"
skills_json="$(printf '%s\n' "${SKILL_IDS[@]}" | jq -R . | jq -s .)"

# PATCH is handled as a full update, so creating and editing use the same body (see the field
# pitfalls section in /llms.txt).
draft="$(jq -n \
  --arg name "$AGENT_NAME" --arg system "$system" --arg model "$AGENT_MODEL" \
  --argjson skills "$skills_json" \
  '{name:$name,
    description:"通用 agent 行为规范 + 飞书卡片技能",
    system:$system,
    model:{id:$model},
    builtin_tools:["bash","read","write","edit","glob","grep"],
    skillIds:$skills,
    feishuAppIds:[],
    status:"active"}')"

agents="$(check "$(api GET "/api/workspaces/$WS/agents")" "列 agent")"
old_agent="$(printf '%s' "$agents" | jq -r --arg n "$AGENT_NAME" '.[] | select(.name==$n) | .id')"
if [[ -n "$old_agent" ]]; then
  agent="$(check "$(api PATCH "/api/agents/$old_agent" "$draft")" "改 agent")"
  info "已更新 $old_agent"
else
  agent="$(check "$(api POST "/api/workspaces/$WS/agents" "$draft")" "建 agent")"
  info "已建 $(printf '%s' "$agent" | jq -r '.id')"
fi
AGENT_ID="$(printf '%s' "$agent" | jq -r '.id')"

step "完成"
info "空间   $WS（$WORKSPACE_KEY）"
info "agent  $AGENT_ID"
info "技能   ${SKILL_IDS[*]}"
info "对话页 $BASE_URL/w/$WORKSPACE_KEY/chat"
printf '\n    验证能不能真的说上话：\n'
printf '      AMA_BASE_URL=%s AMA_API_KEY=<key> AGENT_ID=%s \\\n' "$BASE_URL" "$AGENT_ID"
printf '        scripts/setup-ark-agent/smoke-test.sh\n'

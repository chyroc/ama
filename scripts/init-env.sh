#!/usr/bin/env bash
# Generate the local .env: copy it from .env.example, and fill empty "secret" entries with random values.
#
#   ./scripts/init-env.sh
#
# Safe to run again: entries that already have a value are never touched (especially the PG password:
# if you change it after the data volume is created, the password in the database does not change
# with it, and amad can no longer connect).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/.env"

command -v openssl >/dev/null || { echo "需要 openssl 来生成随机值" >&2; exit 1; }

if [[ ! -f "$ENV_FILE" ]]; then
  cp "$ROOT/.env.example" "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "已从 .env.example 复制出 .env"
fi

# Key name -> how to generate it. hex only contains [0-9a-f], so it needs no escaping in URLs, shell or YAML.
declare -A GEN=(
  [AMA_PG_PASSWORD]="openssl rand -hex 24"
  [AMA_REDIS_PASSWORD]="openssl rand -hex 24"
  [AMA_S3_ACCESS_KEY]="openssl rand -hex 10"
  [AMA_S3_SECRET_KEY]="openssl rand -hex 24"
  # These two need base64 of 32 bytes.
  [ARK_SECRET_KEY]="openssl rand -base64 32"
  [AMA_AUTH_MANAGED_KEY_SECRET]="openssl rand -base64 32"
)

for key in AMA_PG_PASSWORD AMA_REDIS_PASSWORD AMA_S3_ACCESS_KEY AMA_S3_SECRET_KEY \
  ARK_SECRET_KEY AMA_AUTH_MANAGED_KEY_SECRET; do
  current="$(grep -E "^${key}=" "$ENV_FILE" | tail -n1 | cut -d= -f2- || true)"
  if [[ -n "$current" ]]; then
    echo "  保留 $key"
    continue
  fi
  value="$(${GEN[$key]})"
  if grep -qE "^${key}=" "$ENV_FILE"; then
    # Use awk, not sed: base64 contains / and +, which would need escaping in a sed replacement.
    tmp="$(mktemp)"
    awk -v k="$key" -v v="$value" 'BEGIN{FS=OFS="="} $1==k && !done {print k "=" v; done=1; next} {print}' \
      "$ENV_FILE" >"$tmp"
    cat "$tmp" >"$ENV_FILE"
    rm -f "$tmp"
  else
    printf '%s=%s\n' "$key" "$value" >>"$ENV_FILE"
  fi
  echo "  生成 $key"
done

echo "完成：$ENV_FILE（权限 600，别提交它）"

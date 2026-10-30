#!/usr/bin/env bash
# Export the frontend seed data as JSON, for the backend to load with go:embed.
#
# Why not copy it by hand into Go literals: seed.ts is 39 KB and only uses `import type`
# (it is pure data literals), so a mechanical export is easier and cannot introduce copy
# mistakes. The exported JSON also works as a **byte-exact golden file**:
# JSON.stringify already drops keys whose value is undefined, which is exactly the shape
# Go produces with omitempty, so the backend can assert "unmarshal, marshal again,
# compare byte by byte".
#
# Usage: run scripts/export-seed/export.sh from anywhere in the repo.
set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT="$(pwd)"
WEB="$REPO_ROOT/web"
OUT="$REPO_ROOT/server/model/persistmodel/seed.json"

if [ ! -x "$WEB/node_modules/.bin/esbuild" ]; then
  echo "缺 esbuild，先在 web/ 下跑 npm install" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# seed.ts uses the @ alias and type imports, so bundle it into one ESM file first, then run it.
# --log-level=error because bundling a pure data module does not need warnings.
"$WEB/node_modules/.bin/esbuild" "$WEB/src/data/seed.ts" \
  --bundle --format=esm --log-level=error \
  --outfile="$TMP/seed.mjs"

# Print the JSON.stringify result as is, without pretty printing:
# this file is a golden file for machines, and formatting would add whitespace that does
# not match the frontend.
cat > "$TMP/dump.mjs" <<'EOF'
import { SEED } from "./seed.mjs";
process.stdout.write(JSON.stringify(SEED));
EOF

node "$TMP/dump.mjs" > "$OUT"
echo "已写出 $OUT（$(wc -c < "$OUT") 字节）"

#!/usr/bin/env bash
# Build the binaries shipped in the npm package.
#
# Why a script and not a list of commands in the README: the cross-compile target matrix,
# -ldflags and the version number must stay in sync with package.json. Typing three commands by
# hand will always miss one of them (most often -s -w, which makes the package 30% bigger).
#
# Usage (works from the repo root or from cli/):
#   ./cli/build.sh
set -euo pipefail

cd "$(dirname "$0")"
CLI_DIR="$PWD"
SERVER_DIR="$CLI_DIR/../server"

VERSION=$(node -p "require('./package.json').version")

# Target matrix. **linux-amd64 is the required one**: that is what the container runs.
# The other three are for people installing on their own machine; the only cost is a bigger package.
TARGETS=(
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
)

rm -f "$CLI_DIR"/bin/ama-*

for t in "${TARGETS[@]}"; do
  read -r goos goarch <<<"$t"
  out="$CLI_DIR/bin/ama-$goos-$goarch"
  echo "构建 $goos/$goarch → $(basename "$out")"
  # -s -w strips the symbol table and DWARF; measured 30% smaller (8.3 MB → 5.7 MB).
  # -X main.version makes `ama --version` match the npm package version; otherwise, when
  # debugging, "which version is installed" is pure guesswork.
  ( cd "$SERVER_DIR" && GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
      go build -ldflags="-s -w -X main.version=$VERSION" -o "$out" ./cmd/ama-cli )
done

chmod +x "$CLI_DIR"/bin/ama-*
echo
echo "版本 $VERSION，产物："
ls -la "$CLI_DIR"/bin/ | awk 'NR>1 {printf "  %-26s %6.1f MB\n", $9, $5/1048576}'
echo
echo "开源版的发布方式还没定（package.json 标了 private，防止误发）。"

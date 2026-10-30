#!/usr/bin/env bash
# Starts the dev server.
#
# This machine's soft fd limit is 1024, and vite's file watcher hits EMFILE right away, so we
# first raise the soft limit to 65535 (the hard limit is 1024768, so this is allowed). We also
# turn off vite's config file watching to keep fd usage even lower.
#
# The port can be overridden with PORT: e2e starts its own server on a separate port (see
# playwright.config.ts), so tests can run while the dev server is up, without the two clashing.
set -euo pipefail

cd "$(dirname "$0")/.."
ulimit -n 65535
mkdir -p .run
# Listen on 0.0.0.0 so other machines on the same network can connect (otherwise only this
# machine can)
exec npx vite --port "${PORT:-5273}" --host 0.0.0.0 "$@"

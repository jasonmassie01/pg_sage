#!/usr/bin/env bash
set -euo pipefail

cat <<'MSG'
pg_sage v0.9 demo

The old terminal-recording script has been retired because it described the
legacy extension/MCP flow. The current demo is the authenticated sidecar UI:

  1. Overview
  2. Cases
  3. Actions
  4. Fleet
  5. Settings + Shadow Mode

Starting the live demo now.
MSG

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
exec "$SCRIPT_DIR/run-live.sh"

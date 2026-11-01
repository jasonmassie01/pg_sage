#!/bin/bash
# Build the native development version while retaining libpg_query validation.
set -euo pipefail
PG_SAGE_BUILD_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PG_SAGE_BUILD_VERSION="$(cat "$PG_SAGE_BUILD_ROOT/VERSION")"
PG_SAGE_BUILD_COMMIT="$(git -C "$PG_SAGE_BUILD_ROOT" rev-parse --short HEAD)"
if [[ -n "$(git -C "$PG_SAGE_BUILD_ROOT" status --porcelain --untracked-files=normal)" ]]; then
  PG_SAGE_BUILD_COMMIT="${PG_SAGE_BUILD_COMMIT}+local"
fi
PG_SAGE_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PG_SAGE_BUILD_OUTPUT="${1:-$PG_SAGE_BUILD_ROOT/dist/pg_sage}"
mkdir -p "$(dirname "$PG_SAGE_BUILD_OUTPUT")"
PG_SAGE_BUILD_OUTPUT="$(cd "$(dirname "$PG_SAGE_BUILD_OUTPUT")" && pwd)/$(basename "$PG_SAGE_BUILD_OUTPUT")"
cd "$PG_SAGE_BUILD_ROOT/sidecar/web"
npm run build
cd "$PG_SAGE_BUILD_ROOT/sidecar"
CGO_ENABLED=1 go build -trimpath \
  -ldflags "-s -w -X main.version=$PG_SAGE_BUILD_VERSION -X main.commit=$PG_SAGE_BUILD_COMMIT -X main.date=$PG_SAGE_BUILD_DATE" \
  -o "$PG_SAGE_BUILD_OUTPUT" ./cmd/pg_sage_sidecar/
PG_SAGE_BUILD_INFO="$("$PG_SAGE_BUILD_OUTPUT" --version)"
if [[ "$PG_SAGE_BUILD_INFO" != *"sql-ast: libpg_query"* ]]; then
  echo "Build did not retain libpg_query SQL validation" >&2
  exit 1
fi
echo "$PG_SAGE_BUILD_INFO"

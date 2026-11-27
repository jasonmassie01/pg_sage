#!/usr/bin/env bash
# migration-11 (CHECK-24): runs the docs/migration-ci.md gate against the
# fixture migrations and fails unless each exits with its expected code:
# 0 safe, 1 review, 2 risky. A tool error (3) always fails. Static mode
# runs always; live mode runs when SAGE_LINT_DATABASE_URL is set.
# Usage: scripts/ci/lint-migration-fixtures.sh path/to/pg_sage_sidecar
set -euo pipefail

bin=${1:?usage: lint-migration-fixtures.sh path/to/pg_sage_sidecar}
dir=$(cd "$(dirname "$0")/../../test-fixtures/migrations" && pwd)
declare -A want=([0001_safe_additive.sql]=0 [0002_review_index.sql]=1
  [0003_risky_rewrite.sql]=2)
failed=0

check() {
  local mode=$1 file=$2 expected=$3 got=0
  shift 3
  "$bin" lint-migration --file "$dir/$file" --format github "$@" || got=$?
  if [ "$got" -ne "$expected" ]; then
    echo "FAIL ($mode) $file: exit $got, want $expected" >&2
    failed=1
  else
    echo "ok ($mode) $file: exit $got"
  fi
}

for file in "${!want[@]}"; do
  check static "$file" "${want[$file]}" --static --pg-version 17
  if [ -n "${SAGE_LINT_DATABASE_URL:-}" ]; then
    check live "$file" "${want[$file]}"
  fi
done
exit "$failed"

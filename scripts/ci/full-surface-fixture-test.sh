#!/usr/bin/env bash
# Tests the input checks of full-surface-fixture.sh (ci-08). Every case runs
# with PG_SAGE_E2E_DRY_RUN=1, so the script validates and stops before it
# creates anything.
#
# Usage: full-surface-fixture-test.sh [taken-container]
# With a container name (CI passes the running fixture's), it also checks that
# a second run against a taken name fails and leaves that container running.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/full-surface-fixture.sh"
failures=0

# expect <want-status> <stderr-pattern> <description> -- <env/args...>
expect() {
  local want=$1 pattern=$2 name=$3
  shift 4
  local err status
  err=$(env PG_SAGE_E2E_DRY_RUN=1 "$@" 2>&1 >/dev/null)
  status=$?
  if [[ $status -ne $want ]]; then
    echo "FAIL $name: exit $status, want $want; output: $err" >&2
    failures=$((failures + 1))
  elif [[ -n $pattern ]] && ! grep -Eq "$pattern" <<<"$err"; then
    echo "FAIL $name: output does not match /$pattern/: $err" >&2
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

free="pgsage_e2e_fixture_test_$$_free"

expect 2 'usage: .*full-surface-fixture.sh <pg-major>' "missing PG major" -- \
  bash "$script"
expect 2 'PostgreSQL major must be one of 14 15 16 17 18' "non-numeric PG major" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" bash "$script" seventeen
expect 2 'PostgreSQL major must be one of' "PG major below range (13)" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" bash "$script" 13
expect 2 'PostgreSQL major must be one of' "PG major above range (19)" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" bash "$script" 19
expect 2 'usage:' "extra argument" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" bash "$script" 17 18
expect 2 'PG_SAGE_E2E_API_PORT must be a TCP port' "non-numeric API port" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" PG_SAGE_E2E_API_PORT=http bash "$script" 17
expect 2 'PG_SAGE_E2E_PG_PORT must be a TCP port' "port 0" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" PG_SAGE_E2E_PG_PORT=0 bash "$script" 17
expect 2 'PG_SAGE_E2E_METRICS_PORT must be a TCP port' "port 65536" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" PG_SAGE_E2E_METRICS_PORT=65536 bash "$script" 17
expect 2 'container name .* is not valid' "container name with a space" -- \
  PG_SAGE_E2E_PG_CONTAINER="bad name" bash "$script" 17
expect 2 'PG_SAGE_E2E_KEYLESS_METRICS_PORT must be a TCP port' "keyless metrics port" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" PG_SAGE_E2E_KEYLESS_METRICS_PORT=-1 bash "$script" 17
expect 1 'docker daemon did not answer' "docker daemon unreachable" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" DOCKER_HOST=tcp://127.0.0.1:1 bash "$script" 17
expect 1 'keyless needs PG_SAGE_ADMIN_PASS' "keyless without the fixture's admin" -- \
  PG_SAGE_ADMIN_PASS= bash "$script" keyless
expect 0 'dry run: inputs valid' "lowest PG major (14)" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" bash "$script" 14
expect 0 'dry run: inputs valid' "highest PG major (18)" -- \
  PG_SAGE_E2E_PG_CONTAINER="$free" PG_SAGE_E2E_PG_PORT=65535 bash "$script" 18

if [[ $# -ge 1 ]]; then
  taken=$1
  expect 1 "container name '$taken' is already in use" "container name taken" -- \
    PG_SAGE_E2E_PG_CONTAINER="$taken" bash "$script" 17
  running=$(docker container inspect -f '{{.State.Running}}' "$taken" 2>/dev/null)
  if [[ $running != true ]]; then
    echo "FAIL taken container $taken is no longer running after the refused run" >&2
    failures=$((failures + 1))
  else
    echo "ok   taken container left running"
  fi
fi

if [[ $failures -gt 0 ]]; then
  echo "$failures fixture check(s) failed" >&2
  exit 1
fi
echo "all fixture checks passed"

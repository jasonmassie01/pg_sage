#!/usr/bin/env bash
# Prepares a dedicated pgvector/pgvector container for the two agent audit
# tests that need server settings the shared test servers cannot have:
# pgaudit preloaded with the jsonlog collector (E2's real pgaudit
# correlation) and a pg_stat_statements.max of 100 so entries can be
# evicted on demand (G1-10's dealloc fallback; the shared servers keep
# 50000 so other suites' probes survive). The server is the tests' own.
#
# Usage: setup-agent-audit-postgres.sh <container> <pg-major>
set -euo pipefail

container=$1
major=$2

psql_admin() {
  docker exec "$container" psql -U postgres -v ON_ERROR_STOP=1 "$@"
}

restart_and_wait() {
  docker restart "$container" >/dev/null
  for _ in $(seq 1 60); do
    if docker exec "$container" pg_isready -U postgres >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "PostgreSQL in $container did not come back after a restart" >&2
  return 1
}

docker exec -e DEBIAN_FRONTEND=noninteractive "$container" sh -c \
  "apt-get update -qq && apt-get install -y -qq --no-install-recommends \
   postgresql-${major}-pgaudit >/dev/null"
psql_admin \
  -c "ALTER SYSTEM SET shared_preload_libraries = pg_stat_statements, pgaudit" \
  -c "ALTER SYSTEM SET logging_collector = on" \
  -c "ALTER SYSTEM SET log_destination = 'stderr,jsonlog'"
restart_and_wait
psql_admin -c "ALTER SYSTEM SET pg_stat_statements.max = 100"
restart_and_wait
psql_admin \
  -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements" \
  -c "SHOW shared_preload_libraries" -c "SHOW pg_stat_statements.max" \
  -c "SELECT pg_current_logfile('jsonlog') IS NOT NULL AS jsonlog"

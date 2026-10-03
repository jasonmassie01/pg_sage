#!/usr/bin/env bash
# Prepares a pgvector/pgvector test container for the sidecar test suites:
# installs HypoPG and pg_hint_plan from PGDG, preloads pg_stat_statements
# and pg_hint_plan, enables logical WAL, and restarts. ALTER SYSTEM keeps it
# independent of the image's PGDATA layout (PostgreSQL 18 moved it).
# pg_stat_statements then keeps 50000 entries instead of 5000 (a second
# restart: the setting is only recognized once the library is loaded): the
# parallel suite runs more distinct statements than 5000, and evictions
# erased the probe entries tests measure.
#
# Usage: setup-test-postgres.sh <container> <pg-major>
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
   postgresql-${major}-hypopg postgresql-${major}-pg-hint-plan >/dev/null"
psql_admin \
  -c "ALTER SYSTEM SET shared_preload_libraries = pg_stat_statements, pg_hint_plan" \
  -c "ALTER SYSTEM SET wal_level = logical"
restart_and_wait
psql_admin -c "ALTER SYSTEM SET pg_stat_statements.max = 50000"
restart_and_wait
psql_admin \
  -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements" \
  -c "SHOW server_version" -c "SHOW shared_preload_libraries" \
  -c "SHOW pg_stat_statements.max"

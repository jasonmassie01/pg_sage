#!/usr/bin/env bash
# Checks that a CI PostgreSQL container runs the test server the suites
# expect (ci-03b): the image's major, every setting in
# scripts/ci/images/postgresql.ci.conf, pg_stat_statements created,
# HypoPG installable, pg_hint_plan loaded and pgaudit available. It only
# reads (the HypoPG check rolls back), never installs or restarts: the
# image (scripts/ci/images/Dockerfile.pg) carries all of it. It lists every
# mismatch and exits 1 on any.
#
# Usage: verify-test-postgres.sh <container> <pg-major>
# PG_SAGE_CI_CONF names another settings file (default: the image's).
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: $0 <container> <pg-major>" >&2; exit 2; }
container=$1
major=$2
here=$(cd "$(dirname "$0")" && pwd)
conf=${PG_SAGE_CI_CONF:-$here/images/postgresql.ci.conf}
[ -r "$conf" ] || { echo "cannot read the settings file $conf" >&2; exit 2; }

psql_q() {
  docker exec "$container" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX "$@"
}

# The image's entrypoint runs initdb scripts on a server that listens on
# the socket only, so TCP readiness means the final server is up.
ready=
for _ in $(seq 1 90); do
  if docker exec "$container" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
[ -n "$ready" ] || { echo "PostgreSQL in $container is not ready on TCP after 90s" >&2; exit 1; }

problems=()
got=$(psql_q -c "SELECT current_setting('server_version_num')::int / 10000")
[ "$got" = "$major" ] || problems+=("server major is $got, want $major")

while IFS= read -r line || [ -n "$line" ]; do
  line=${line%%#*}
  line=$(echo "$line" | tr -d '\r' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
  [ -n "$line" ] || continue
  name=$(echo "${line%%=*}" | sed 's/[[:space:]]*$//')
  want=$(echo "${line#*=}" | sed -e 's/^[[:space:]]*//' -e "s/^'//" -e "s/'$//")
  got=$(psql_q -c "SHOW $name" 2>&1) || got="(error: $got)"
  [ "$got" = "$want" ] || problems+=("$name is $got, want $want")
done < "$conf"

got=$(psql_q -c "SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements'")
[ "$got" = "1" ] || problems+=("pg_stat_statements is not created in the postgres database")
psql_q -c "BEGIN" -c "CREATE EXTENSION hypopg" -c "ROLLBACK" >/dev/null 2>&1 ||
  problems+=("CREATE EXTENSION hypopg fails (HypoPG is not installed)")
got=$(psql_q -c "SHOW pg_hint_plan.enable_hint" 2>/dev/null) || got=
[ "$got" = "on" ] || problems+=("pg_hint_plan is not loaded (pg_hint_plan.enable_hint is '$got')")
got=$(psql_q -c "SELECT count(*) FROM pg_available_extensions WHERE name = 'pgaudit'")
[ "$got" = "1" ] || problems+=("pgaudit is not installed")

if [ ${#problems[@]} -gt 0 ]; then
  echo "PostgreSQL in $container is not CI's test server ($conf):" >&2
  printf '  - %s\n' "${problems[@]}" >&2
  exit 1
fi
psql_q -c "SELECT 'PostgreSQL ' || current_setting('server_version') || ', preload ' ||
  current_setting('shared_preload_libraries') || ', ' ||
  (SELECT string_agg(name || ' ' || default_version, ', ' ORDER BY name)
     FROM pg_available_extensions WHERE name IN ('hypopg', 'pg_hint_plan', 'pgaudit'))"

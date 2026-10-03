#!/usr/bin/env bash
# Extra load on a shared test server, for checking that the Go test suite
# stays deterministic while other packages share the server (CI runs every
# package in parallel against one PostgreSQL). It runs from its own client
# container, never inside the server's container: there the postmaster is
# PID 1, reaps orphaned clients and crash-restarts on one killed by a
# signal.
#
# usage: [PAT=..] [AVPAT=..] scripts/ci/stress-load.sh start|stop <port>
#
#   Base load (application_name ci_stress, database ci_stress): 10 sessions
#   active in pg_sleep, 10 idle in a REPEATABLE READ transaction, a
#   pg_stat_statements_reset() every 3 s and one new connection a second.
#   PAT (datname LIKE): every ~0.5 s, in each matching database, a session
#   holds a REPEATABLE READ snapshot idle for 0.3 s (application_name
#   ci_snapchurn), the way an autovacuum ANALYZE of that database does.
#   AVPAT (datname LIKE): in each matching database, once, a table whose
#   autovacuum crawls (cost limit 1, delay 100 ms), so a real autovacuum
#   worker stays active in that database.
#
# STRESS_HOST (default host.docker.internal) is the server's host as seen
# from a container; STRESS_IMAGE (default pgvector/pgvector:0.8.2-pg18) is
# any local image with psql.
set -euo pipefail

cmd=${1:?usage: stress-load.sh start|stop <port>}
port=${2:?usage: stress-load.sh start|stop <port>}
host=${STRESS_HOST:-host.docker.internal}
img=${STRESS_IMAGE:-pgvector/pgvector:0.8.2-pg18}
name=ci-stress-$port
url="postgresql://postgres:postgres@$host:$port/postgres"

if [ "$cmd" = stop ]; then
  docker stop -t 2 "$name" >/dev/null 2>&1 || true
  # pg_sleep does not notice its client is gone: end the server sessions
  # once no client loop is left to race new connections.
  MSYS_NO_PATHCONV=1 docker run --rm --entrypoint psql "$img" "$url" -Atc \
    "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
     WHERE application_name IN ('ci_stress', 'ci_snapchurn')"
  exit 0
fi

read -r -d '' load <<'SCRIPT' || true
export PGUSER=postgres PGPASSWORD=postgres PGAPPNAME=ci_stress
psql -d postgres -qc "CREATE DATABASE ci_stress" 2>/dev/null
for i in $(seq 10); do psql -d ci_stress -qc "SELECT pg_sleep(7200)" >/dev/null 2>&1 & done
for i in $(seq 10); do
  (echo "BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT count(*) FROM pg_class;"
   sleep 7200) | psql -d ci_stress -q >/dev/null 2>&1 &
done
crawl="DO \$\$ BEGIN IF to_regclass('public.ci_av_crawl') IS NULL THEN
  CREATE TABLE public.ci_av_crawl (id int, pad text) WITH (
    autovacuum_vacuum_cost_limit = 1, autovacuum_vacuum_cost_delay = 100,
    autovacuum_vacuum_threshold = 0, autovacuum_vacuum_scale_factor = 0);
  INSERT INTO public.ci_av_crawl SELECT g, repeat('x', 200)
    FROM generate_series(1, 20000) g;
  DELETE FROM public.ci_av_crawl WHERE id % 2 = 0;
END IF; END \$\$"
if [ -n "$AVPAT" ]; then
  declare -A seeded
  while true; do
    for db in $(psql -d postgres -Atc \
        "SELECT datname FROM pg_database WHERE datname LIKE '$AVPAT'"); do
      [ -n "${seeded[$db]:-}" ] && continue
      psql -d "$db" -qc "$crawl" >/dev/null 2>&1 && seeded[$db]=1
    done
    sleep 2
  done &
fi
if [ -n "$PAT" ]; then
  while true; do
    for db in $(psql -d postgres -Atc \
        "SELECT datname FROM pg_database WHERE datname LIKE '$PAT'"); do
      ( (echo "BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1;"; sleep 0.3
         echo "COMMIT;") | PGAPPNAME=ci_snapchurn psql -d "$db" -q >/dev/null 2>&1 ) &
    done
    wait; sleep 0.5
  done &
fi
while true; do
  psql -d postgres -qc "SELECT pg_stat_statements_reset()" >/dev/null 2>&1
  for j in 1 2 3; do psql -d ci_stress -qc "SELECT 1" >/dev/null 2>&1; sleep 1; done
done
SCRIPT

MSYS_NO_PATHCONV=1 docker run -d --rm --name "$name" --cpus=1 \
  -e PGHOST="$host" -e PGPORT="$port" -e PAT="${PAT:-}" -e AVPAT="${AVPAT:-}" \
  --entrypoint bash "$img" -c "$load" >/dev/null
sleep 4
MSYS_NO_PATHCONV=1 docker run --rm --entrypoint psql "$img" "$url" -Atc \
  "SELECT application_name, state, count(*) FROM pg_stat_activity
   WHERE application_name LIKE 'ci\_%' GROUP BY 1, 2"

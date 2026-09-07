#!/usr/bin/env bash
# Pipeline-coverage runner: provisions a DEDICATED throwaway Postgres
# (pgvector image so HNSW is testable, pg_stat_statements preloaded so the
# slow-query rule fires), runs both pipeline-coverage layers, tears down.
#
# Usage: bash scripts/run_pipeline_coverage.sh [-k]   (-k keeps container)
set -u

CONTAINER=pgsage_pipeline_pg
PORT=5455
IMAGE=pgvector/pgvector:pg16
DB=pipeline
URL="postgres://postgres:postgres@localhost:${PORT}/${DB}?sslmode=disable"
KEEP=0
[ "${1:-}" = "-k" ] && KEEP=1

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

cleanup() {
  if [ "$KEEP" = "0" ]; then
    docker rm -f "$CONTAINER" >/dev/null 2>&1
  else
    echo "[runner] keeping container $CONTAINER (port $PORT)"
  fi
}
trap cleanup EXIT

echo "[runner] starting $IMAGE on :$PORT ..."
docker rm -f "$CONTAINER" >/dev/null 2>&1
docker run -d --name "$CONTAINER" -p "$PORT":5432 \
  -e POSTGRES_PASSWORD=postgres \
  "$IMAGE" \
  -c shared_preload_libraries=pg_stat_statements \
  -c max_connections=120 >/dev/null || { echo "docker run failed"; exit 1; }

echo "[runner] waiting for postgres ..."
for i in $(seq 1 90); do
  if docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1 \
  || { echo "postgres never became ready; container state + logs:";
       docker ps -a --filter "name=$CONTAINER" --format '{{.Status}}';
       docker logs "$CONTAINER" 2>&1 | tail -30;
       exit 1; }

docker exec "$CONTAINER" psql -U postgres -q \
  -c "DROP DATABASE IF EXISTS ${DB}" -c "CREATE DATABASE ${DB}" \
  || { echo "create db failed"; exit 1; }

echo "[runner] running pipeline coverage suite ..."
cd "$ROOT/sidecar" || exit 1
PIPELINE_PG_URL="$URL" \
  go test -tags=e2e -count=1 -v -timeout 900s \
  -run 'TestPipelineCoverage' ./e2e/
rc=$?

echo "[runner] exit code: $rc"
exit $rc

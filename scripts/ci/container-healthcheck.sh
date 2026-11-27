#!/usr/bin/env bash
# ci-06: run the sidecar image twice, plain HTTP and TLS with a throwaway
# self-signed pair, and require Docker to report both containers healthy.
# The image HEALTHCHECK runs "sage-sidecar healthcheck", which must follow
# SAGE_TLS_CERT/SAGE_TLS_KEY; the old curl probe failed the TLS run.
#
# Usage: scripts/ci/container-healthcheck.sh [image]
#   image     defaults to pg_sage:healthcheck, built from sidecar/ unless
#             SKIP_BUILD=1.
#   PG_IMAGE  the PostgreSQL image (default postgres:17-alpine).
set -euo pipefail

image="${1:-pg_sage:healthcheck}"
pg_image="${PG_IMAGE:-postgres:17-alpine}"
prefix="sage-health-$$"
net="${prefix}-net"
tlsdir="$(mktemp -d)"

cleanup() {
  docker rm -f -v "${prefix}-pg" "${prefix}-plain" "${prefix}-tls" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$tlsdir"
}
trap cleanup EXIT

if [ "${SKIP_BUILD:-0}" != "1" ]; then
  docker build -t "$image" sidecar/
fi

docker network create "$net" >/dev/null
docker run -d --name "${prefix}-pg" --network "$net" \
  -e POSTGRES_PASSWORD=postgres "$pg_image" >/dev/null
for _ in $(seq 1 60); do
  docker exec "${prefix}-pg" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && break
  sleep 1
done
docker exec "${prefix}-pg" pg_isready -h 127.0.0.1 -U postgres >/dev/null
docker exec "${prefix}-pg" createdb -U postgres plain
docker exec "${prefix}-pg" createdb -U postgres tls

# The image runs as uid 1000, so the pair must be readable by it.
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=sage-health" \
  -keyout "$tlsdir/tls.key" -out "$tlsdir/tls.crt" 2>/dev/null
chmod 755 "$tlsdir"
chmod 644 "$tlsdir/tls.key" "$tlsdir/tls.crt"

dsn() { echo "postgres://postgres:postgres@${prefix}-pg:5432/$1?sslmode=disable"; }

# A short interval keeps the job quick; the probe command is the image's.
docker run -d --name "${prefix}-plain" --network "$net" --health-interval=5s \
  -e SAGE_DATABASE_URL="$(dsn plain)" "$image" >/dev/null
docker run -d --name "${prefix}-tls" --network "$net" --health-interval=5s \
  -v "$tlsdir:/tls:ro" -e SAGE_TLS_CERT=/tls/tls.crt -e SAGE_TLS_KEY=/tls/tls.key \
  -e SAGE_DATABASE_URL="$(dsn tls)" "$image" >/dev/null

wait_healthy() {
  local name="$1" status=""
  for _ in $(seq 1 60); do
    status="$(docker inspect --format '{{.State.Health.Status}}' "$name")"
    case "$status" in
      healthy)
        echo "$name: healthy"
        return 0
        ;;
      unhealthy) break ;;
    esac
    if [ "$(docker inspect --format '{{.State.Running}}' "$name")" != "true" ]; then
      status="exited"
      break
    fi
    sleep 2
  done
  echo "$name: health status '$status', want healthy" >&2
  docker inspect --format '{{json .State.Health}}' "$name" >&2 || true
  docker logs --tail 50 "$name" >&2 || true
  return 1
}

wait_healthy "${prefix}-plain"
wait_healthy "${prefix}-tls"

#!/usr/bin/env bash
# Builds the disposable full-surface fixture the root Playwright suite (e2e/)
# runs against (ci-08): one throwaway PostgreSQL container holding a metadata
# database and three target databases, the pg_sage binary built from this
# checkout in metadata fleet mode with a random test-only encryption key, an
# admin created through cmd/create_admin, and the three targets added through
# the API. Everything listens on 127.0.0.1 only. Nothing here points at a
# database that is not created by this script.
#
# Usage:
#   full-surface-fixture.sh <pg-major>   start the fixture (PostgreSQL 14-18)
#   full-surface-fixture.sh keyless      start a second, standalone server with
#                                        no encryption key (needs a running
#                                        fixture and PG_SAGE_ADMIN_PASS)
#   full-surface-fixture.sh down         stop both servers, remove the container
#
# Settings (environment, all optional):
#   PG_SAGE_E2E_PG_CONTAINER  container name             (pgsage_e2e)
#   PG_SAGE_E2E_PG_PORT       PostgreSQL host port       (5455)
#   PG_SAGE_E2E_API_PORT      fixture API port           (8086)
#   PG_SAGE_E2E_METRICS_PORT  fixture metrics port       (9189)
#   PG_SAGE_E2E_KEYLESS_PORT  keyless server API port    (8087)
#   PG_SAGE_E2E_KEYLESS_METRICS_PORT  its metrics port   (9190)
#   PG_SAGE_E2E_IMAGE         PostgreSQL image (the mirrored CI image)
#   PG_SAGE_E2E_WORKDIR       binaries, configs and logs (${RUNNER_TEMP:-/tmp}/pg_sage_e2e)
#   PG_SAGE_E2E_DRY_RUN=1     validate the inputs and stop before changing anything
#
# The suite's settings (PG_SAGE_E2E_*, PG_SAGE_ADMIN_EMAIL, PG_SAGE_ADMIN_PASS)
# go to $GITHUB_ENV in Actions, or to $PG_SAGE_E2E_WORKDIR/e2e.env locally.
# The admin password is never printed.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
container=${PG_SAGE_E2E_PG_CONTAINER:-pgsage_e2e}
pg_port=${PG_SAGE_E2E_PG_PORT:-5455}
api_port=${PG_SAGE_E2E_API_PORT:-8086}
metrics_port=${PG_SAGE_E2E_METRICS_PORT:-9189}
keyless_port=${PG_SAGE_E2E_KEYLESS_PORT:-8087}
keyless_metrics_port=${PG_SAGE_E2E_KEYLESS_METRICS_PORT:-9190}
workdir=${PG_SAGE_E2E_WORKDIR:-${RUNNER_TEMP:-/tmp}/pg_sage_e2e}
admin_email=${PG_SAGE_ADMIN_EMAIL:-e2e-admin@pg-sage.local}
targets=(testdb:e2e_target1 testdb2:e2e_target2 health_test:e2e_target3)

die() {
  echo "full-surface-fixture: $*" >&2
  exit 1
}

usage() {
  echo "usage: full-surface-fixture.sh <pg-major> | keyless | down" >&2
  exit 2
}

check_port() {
  local name=$1 value=$2
  if ! [[ $value =~ ^[0-9]{1,5}$ ]] || ((10#$value < 1 || 10#$value > 65535)); then
    echo "full-surface-fixture: $name must be a TCP port (1-65535), got '$value'" >&2
    exit 2
  fi
}

# validate_inputs checks everything before the first change: the major, the
# ports, the container name, and that the name is free.
validate_inputs() {
  local major=$1
  if ! [[ $major =~ ^(14|15|16|17|18)$ ]]; then
    echo "full-surface-fixture: PostgreSQL major must be one of 14 15 16 17 18," \
      "got '$major'" >&2
    exit 2
  fi
  check_port PG_SAGE_E2E_PG_PORT "$pg_port"
  check_port PG_SAGE_E2E_API_PORT "$api_port"
  check_port PG_SAGE_E2E_METRICS_PORT "$metrics_port"
  check_port PG_SAGE_E2E_KEYLESS_PORT "$keyless_port"
  check_port PG_SAGE_E2E_KEYLESS_METRICS_PORT "$keyless_metrics_port"
  if ! [[ $container =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]]; then
    echo "full-surface-fixture: container name '$container' is not valid" >&2
    exit 2
  fi
  for tool in docker curl node openssl go npm timeout; do
    command -v "$tool" >/dev/null || die "$tool is required on PATH"
  done
  timeout 30 docker info >/dev/null 2>&1 ||
    die "the docker daemon did not answer within 30s; start Docker and retry"
  if docker container inspect "$container" >/dev/null 2>&1; then
    die "container name '$container' is already in use; remove it" \
      "('full-surface-fixture.sh down') or set PG_SAGE_E2E_PG_CONTAINER"
  fi
}

psql_in() {
  docker exec -i "$container" psql -U postgres -v ON_ERROR_STOP=1 -qAt "$@"
}

start_postgres() {
  local major=$1
  local image=${PG_SAGE_E2E_IMAGE:-ghcr.io/jasonmassie01/pg_sage-ci-pgvector:0.8.2-pg$major-r2}
  docker run -d --name "$container" -e POSTGRES_PASSWORD=postgres \
    -p "127.0.0.1:$pg_port:5432" "$image" >/dev/null
  local ready=0
  for _ in $(seq 1 60); do
    docker exec "$container" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && { ready=1; break; }
    sleep 1
  done
  ((ready)) || die "PostgreSQL in container '$container' did not accept connections" \
    "within 60s (image $image); see 'docker logs $container'"
  # CI's image has the test server baked in (ci-03b); a PG_SAGE_E2E_IMAGE must
  # too (scripts/ci/images/Dockerfile.pg), or the check names what is missing.
  bash "$root/scripts/ci/verify-test-postgres.sh" "$container" "$major" >/dev/null
  psql_in -c "CREATE DATABASE e2e_meta" -c "CREATE DATABASE e2e_standalone"
  local entry
  for entry in "${targets[@]}"; do
    psql_in -c "CREATE DATABASE ${entry#*:}"
    psql_in -d "${entry#*:}" -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"
  done
}

build_binaries() {
  local exe
  exe=$(cd "$root/sidecar" && go env GOEXE)
  (cd "$root/sidecar/web" && npm ci --no-audit --no-fund && npm run build) >/dev/null
  (cd "$root/sidecar" && go build -o "$workdir/pg_sage_sidecar$exe" ./cmd/pg_sage_sidecar &&
    go build -o "$workdir/create_admin$exe" ./cmd/create_admin)
  sidecar_bin="$workdir/pg_sage_sidecar$exe"
  admin_bin="$workdir/create_admin$exe"
}

pg_dsn() {
  echo "postgres://postgres:postgres@127.0.0.1:$pg_port/$1?sslmode=disable"
}

# start_sidecar <name> <mode> <api-port> <metrics-port> [VAR=value ...]
start_sidecar() {
  local name=$1 mode=$2 port=$3 metrics=$4
  shift 4
  cat >"$workdir/$name.yaml" <<YAML
mode: $mode
api:
  listen_addr: "127.0.0.1:$port"
prometheus:
  listen_addr: "127.0.0.1:$metrics"
llm:
  enabled: false
YAML
  env SAGE_META_DB= SAGE_ENCRYPTION_KEY= SAGE_DATABASE_URL= "$@" \
    nohup "$workdir/pg_sage_sidecar$(cd "$root/sidecar" && go env GOEXE)" \
    --config "$workdir/$name.yaml" >"$workdir/$name.log" 2>&1 &
  echo $! >"$workdir/$name.pid"
  wait_ready "$name" "$port"
}

wait_ready() {
  local name=$1 port=$2
  for _ in $(seq 1 120); do
    if [[ $(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/ready") == 200 ]]
    then
      echo "$name: /ready returned 200 on 127.0.0.1:$port"
      return 0
    fi
    kill -0 "$(cat "$workdir/$name.pid")" 2>/dev/null || break
    sleep 1
  done
  tail -n 60 "$workdir/$name.log" >&2 || true
  die "$name did not report /ready 200 on 127.0.0.1:$port (log: $workdir/$name.log)"
}

# create_admin prints the email and id, never the password.
create_admin() {
  local dsn=$1
  PG_SAGE_DSN=$dsn PG_SAGE_ADMIN_EMAIL=$admin_email PG_SAGE_ADMIN_PASSWORD=$admin_pass \
    "$admin_bin" || die "cmd/create_admin failed against database ${dsn##*/}"
}

# api <method> <path> [body-file]: an authenticated call to the fixture; the
# response body goes to stdout, a non-2xx status fails the script.
api() {
  local method=$1 route=$2 data=${3:-}
  local args=(-s -o "$workdir/api.out" -w '%{http_code}' -b "$workdir/cookies"
    -c "$workdir/cookies" -X "$method" -H 'Content-Type: application/json')
  [[ -n $data ]] && args+=(--data-binary "@$data")
  local code
  code=$(curl "${args[@]}" "http://127.0.0.1:$api_port/api/v1$route")
  [[ $code == 2* ]] || die "$method $route returned HTTP $code: $(head -c 400 "$workdir/api.out")"
  cat "$workdir/api.out"
}

login() {
  # The body file keeps the password off the command line.
  umask 077
  printf '{"email":"%s","password":"%s"}' "$admin_email" "$admin_pass" >"$workdir/login.json"
  api POST /auth/login "$workdir/login.json" >/dev/null
  rm -f "$workdir/login.json"
}

add_targets() {
  local entry
  for entry in "${targets[@]}"; do
    printf '{"name":"%s","host":"127.0.0.1","port":%d,"database_name":"%s",%s}' \
      "${entry%%:*}" "$pg_port" "${entry#*:}" \
      '"username":"postgres","password":"postgres","sslmode":"disable","max_connections":2,
      "trust_level":"observation","execution_mode":"approval"' >"$workdir/target.json"
    api POST /databases/managed "$workdir/target.json" >/dev/null
    echo "added target ${entry%%:*} (${entry#*:})"
  done
}

# smoke_check: the fleet lists exactly the three targets, by name.
smoke_check() {
  local want="health_test,testdb,testdb2" got=""
  for _ in $(seq 1 60); do
    got=$(api GET /databases | node -e '
      let s = ""; process.stdin.on("data", (c) => { s += c; });
      process.stdin.on("end", () => {
        const names = (JSON.parse(s).databases || []).map((d) => d.name).sort();
        console.log(names.join(","));
      });')
    [[ $got == "$want" ]] && { echo "smoke: GET /api/v1/databases lists $got"; return 0; }
    sleep 2
  done
  die "GET /api/v1/databases lists '$got', want exactly '$want'"
}

export_env() {
  local file=${GITHUB_ENV:-$workdir/e2e.env}
  umask 077
  {
    echo "PG_SAGE_E2E_FIXTURE=full-surface"
    echo "PG_SAGE_E2E_BASE_URL=http://127.0.0.1:$api_port"
    echo "PG_SAGE_E2E_METRICS_URL=http://127.0.0.1:$metrics_port/metrics"
    echo "PG_SAGE_E2E_PG_CONTAINER=$container"
    echo "PG_SAGE_E2E_PG_PORT=$pg_port"
    echo "PG_SAGE_ADMIN_EMAIL=$admin_email"
    echo "PG_SAGE_ADMIN_PASS=$admin_pass"
  } >>"$file"
  echo "suite settings written to $file"
}

start_fixture() {
  local major=$1
  validate_inputs "$major"
  if [[ ${PG_SAGE_E2E_DRY_RUN:-} == 1 ]]; then
    echo "dry run: inputs valid for PostgreSQL $major, container '$container'" >&2
    return 0
  fi
  mkdir -p "$workdir"
  admin_pass=$(openssl rand -hex 16)
  [[ ${GITHUB_ACTIONS:-} == true ]] && echo "::add-mask::$admin_pass"
  start_postgres "$major"
  build_binaries
  start_sidecar fixture meta "$api_port" "$metrics_port" \
    "SAGE_META_DB=$(pg_dsn e2e_meta)" "SAGE_ENCRYPTION_KEY=$(openssl rand -hex 32)"
  create_admin "$(pg_dsn e2e_meta)"
  login
  add_targets
  smoke_check
  export_env
}

# start_keyless: a standalone server with no encryption key, for the spec
# that checks the dashboard reports the missing key.
start_keyless() {
  admin_pass=${PG_SAGE_ADMIN_PASS:-}
  [[ -n $admin_pass ]] || die "keyless needs PG_SAGE_ADMIN_PASS from the fixture run"
  check_port PG_SAGE_E2E_KEYLESS_PORT "$keyless_port"
  check_port PG_SAGE_E2E_KEYLESS_METRICS_PORT "$keyless_metrics_port"
  docker container inspect "$container" >/dev/null 2>&1 ||
    die "container '$container' is not running; start the fixture first"
  admin_bin="$workdir/create_admin$(cd "$root/sidecar" && go env GOEXE)"
  start_sidecar keyless standalone "$keyless_port" "$keyless_metrics_port" \
    "SAGE_DATABASE_URL=$(pg_dsn e2e_standalone)"
  create_admin "$(pg_dsn e2e_standalone)"
  echo "keyless standalone server ready at http://127.0.0.1:$keyless_port"
}

stop_all() {
  local name
  for name in keyless fixture; do
    if [[ -f $workdir/$name.pid ]]; then
      kill "$(cat "$workdir/$name.pid")" 2>/dev/null || true
      rm -f "$workdir/$name.pid"
    fi
  done
  docker rm -f -v "$container" >/dev/null 2>&1 || true
  rm -f "$workdir/e2e.env" "$workdir/cookies" "$workdir/api.out"
  echo "fixture stopped; container '$container' removed"
}

[[ $# -eq 1 ]] || usage
case $1 in
  down) stop_all ;;
  keyless) start_keyless ;;
  *) start_fixture "$1" ;;
esac

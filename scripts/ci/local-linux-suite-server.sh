# Server functions that local-linux-suite.sh sources (ci-19); not run on
# its own. CI's test job and integration-matrix start two servers from one
# image: the suite's, and a second one only the tests that measure
# cluster-wide state use (testdb.Exclusive, SAGE_TEST_EXCLUSIVE_URL, ci-12).
# Each major here gets both.

# The exclusive server shares the suite server's network namespace and
# listens on this port, so the Go container reaches both on 127.0.0.1.
readonly EXCLUSIVE_PORT=5433
readonly EXCLUSIVE_URL="postgres://postgres:postgres@127.0.0.1:$EXCLUSIVE_PORT/postgres"\
"?sslmode=disable"

# server_flags <image>: CI's settings as -c flags. The official postgres
# image has no pg_hint_plan, so it preloads pg_stat_statements only.
server_flags() {
  local line name value
  SERVER_FLAGS=()
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%%#*}
    line=$(echo "$line" | tr -d '\r' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    [ -n "$line" ] || continue
    name=$(echo "${line%%=*}" | sed 's/[[:space:]]*$//')
    value=$(echo "${line#*=}" | sed -e 's/^[[:space:]]*//' -e "s/^'//" -e "s/'$//")
    case "$1:$name" in
      postgres:*:shared_preload_libraries | */postgres:*:shared_preload_libraries)
        value=pg_stat_statements ;;
    esac
    SERVER_FLAGS+=(-c "$name=$value")
  done <"$OUT/stage/scripts/ci/images/postgresql.ci.conf"
}

# psql_srv <server> <psql args...>: psql over the server's socket, on the
# port its PGPORT names.
psql_srv() {
  dk 120 exec "$1" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX "${@:2}"
}

# loopback_scram <server>: the postgres images trust TCP logins from
# loopback without a password, while CI's client reaches its server over
# the bridge and must pass scram-sha-256. The suite connects to 127.0.0.1,
# so switch every host line to scram-sha-256 (socket lines stay trusted for
# docker exec), reload, and prove a password-less loopback login fails.
# pg_reload_conf only signals the postmaster, so the check allows it a few
# seconds to read the new file.
loopback_scram() {
  local i
  dk 60 exec "$1" sh -c 'sed -i "/^host/s/trust\$/scram-sha-256/" "$PGDATA/pg_hba.conf"' ||
    die 5 "could not switch $1's pg_hba.conf host lines to scram-sha-256"
  psql_srv "$1" -c "SELECT pg_reload_conf()" >/dev/null || die 5 "could not reload $1"
  for i in 1 2 3 4 5; do
    dk 60 exec "$1" psql -w -h 127.0.0.1 -U postgres -d postgres -qAtX -c 'SELECT 1' \
      >/dev/null 2>&1 || return 0
    sleep 1
  done
  die 5 "server $1 still accepts a loopback login without a password"
}

# run_server <name> <image> [docker run options...]: starts a server with
# CI's settings and the CPU caps of tools/pg-ephemeral.sh (no port is
# published), waits for TCP, switches loopback to scram-sha-256 and creates
# pg_stat_statements in postgres, as CI's image has it.
run_server() {
  local srv=$1 img=$2 ready= i
  shift 2
  server_flags "$img"
  claim "$srv"
  dk 180 run -d --rm --name "$srv" --label pg_sage.improve=1 --label "$RUN_LABEL=$RUN_ID" \
    --oom-score-adj=500 --cpu-shares=256 --cpus=2 -e POSTGRES_PASSWORD=postgres \
    --shm-size=256m -e PGDATA=/var/lib/postgresql/data/pgdata "$@" \
    "$img" postgres "${SERVER_FLAGS[@]}" >/dev/null || die 5 "server $srv ($img) did not start"
  for i in $(seq 1 90); do
    if dk 20 exec "$srv" pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  [ -n "$ready" ] || die 5 "server $srv did not become ready on TCP in 90s"
  sleep 1
  loopback_scram "$srv"
  psql_srv "$srv" -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements" >/dev/null ||
    die 5 "CREATE EXTENSION pg_stat_statements failed on $srv"
}

# start_server <major>: the suite's server, by default with its data
# directory in memory (LOCAL_SUITE_PG_TMPFS) under LOCAL_SUITE_PG_MEMORY.
start_server() {
  local data=()
  [ "$PG_TMPFS" = off ] || data=(--tmpfs "/var/lib/postgresql/data:rw,size=$PG_TMPFS")
  run_server "${PREFIX}pg$1" "$(pg_image "$1")" \
    --memory="$PG_MEMORY" --memory-swap="$PG_MEMORY" "${data[@]}"
}

# start_exclusive <major>: CI's second server, from the same image, on the
# suite server's network namespace at EXCLUSIVE_PORT. Its tests run one at
# a time on a fresh database each, so pg-ephemeral's 1536m cap holds it;
# its data directory stays on the container's disk (an anonymous volume
# that --rm and the cleanup's rm -v remove), so WAL cannot fill that cap.
start_exclusive() {
  run_server "${PREFIX}exclusive-pg$1" "$(pg_image "$1")" \
    --network "container:${PREFIX}pg$1" -e "PGPORT=$EXCLUSIVE_PORT" \
    --memory=1536m --memory-swap=1536m
}

# check_server <major>: records what the suite's server cannot cover; never
# fatal.
check_server() {
  local m=$1 srv=${PREFIX}pg$1 dir=$OUT/pg$1 st=0
  bash "$OUT/stage/scripts/ci/verify-test-postgres.sh" "$srv" "$m" \
    >"$dir/server-check.txt" 2>&1 || st=$?
  if ! psql_srv "$srv" -c BEGIN -c "CREATE EXTENSION vector" -c ROLLBACK >/dev/null 2>&1; then
    echo "  - CREATE EXTENSION vector fails (pgvector is not installed; local check)" \
      >>"$dir/server-check.txt"
    [ "$st" -ne 0 ] || st=1
  fi
  echo "server_check=$st" >>"$dir/status.txt"
}

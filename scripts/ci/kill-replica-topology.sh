#!/usr/bin/env bash
# Builds the streaming topology for the agent kill switch's G1-05 test: a
# primary and two hot standbys cloned from it with pg_basebackup. The
# standby "replica1" is the configured replica (databases[].replicas), and
# "standby2" is one pg_sage was not told about. Each standby sets
# cluster_name, which its walreceiver reports as application_name in the
# primary's pg_stat_replication.
#
# Usage: kill-replica-topology.sh up|down <major> <prefix> [primary-port] [replica-port]
#   up   starts <prefix>-primary, <prefix>-replica1 and <prefix>-standby2 on
#        the docker network <prefix>-net, publishing the primary and replica1
#        on 127.0.0.1 when ports are given; prints the env for the test.
#   down removes the containers and the network.
# The containers carry --label owner=<prefix>; replication is trusted only
# on the private network (test topology, no credentials).
set -euo pipefail

action=$1
major=$2
prefix=$3
primary_port=${4:-}
replica_port=${5:-}
image="pgvector/pgvector:0.8.2-pg${major}"
net="${prefix}-net"
pgdata=/var/lib/postgresql/data/pgdata

down() {
  for c in standby2 replica1 primary; do
    docker rm -f "${prefix}-${c}" >/dev/null 2>&1 || true
  done
  docker network rm "${net}" >/dev/null 2>&1 || true
}

publish() {
  if [ -n "$1" ]; then
    echo "-p 127.0.0.1:$1:5432"
  fi
}

wait_ready() {
  for _ in $(seq 1 90); do
    if docker exec "$1" pg_isready -U postgres >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "$1 did not become ready" >&2
  docker logs --tail 30 "$1" >&2
  return 1
}

start_primary() {
  # shellcheck disable=SC2046
  docker run -d --name "${prefix}-primary" --label "owner=${prefix}" --network "${net}" \
    $(publish "${primary_port}") -e POSTGRES_PASSWORD=postgres -e PGDATA="${pgdata}" \
    "${image}" -c wal_level=replica -c max_wal_senders=10 -c hot_standby=on \
    -c cluster_name=primary >/dev/null
  wait_ready "${prefix}-primary"
  docker exec "${prefix}-primary" bash -c \
    "echo 'host replication all samenet trust' >> ${pgdata}/pg_hba.conf"
  docker exec "${prefix}-primary" psql -U postgres -qAtc "SELECT pg_reload_conf()" >/dev/null
}

start_standby() {
  local name=$1 port=$2
  # The entrypoint is bypassed: the data directory is a base backup of the
  # primary, written as the postgres user, with standby.signal (-R).
  # shellcheck disable=SC2046
  docker run -d --name "${prefix}-${name}" --label "owner=${prefix}" --network "${net}" \
    $(publish "${port}") --user postgres --entrypoint bash "${image}" -c \
    "set -e; rm -rf ${pgdata}; pg_basebackup -h ${prefix}-primary -U postgres \
       -D ${pgdata} -R -X stream -c fast; chmod 700 ${pgdata}; \
     exec postgres -D ${pgdata} -c cluster_name=${name} -c hot_standby=on" >/dev/null
  wait_ready "${prefix}-${name}"
}

case "${action}" in
  up)
    down
    docker network create "${net}" >/dev/null
    start_primary
    start_standby replica1 "${replica_port}"
    start_standby standby2 ""
    docker exec "${prefix}-primary" psql -U postgres -qAtc \
      "SELECT application_name || ' ' || state FROM pg_stat_replication ORDER BY 1"
    echo "SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@${prefix}-primary:5432/postgres?sslmode=disable"
    echo "SAGE_TEST_KILL_REPLICA_URL=postgres://postgres:postgres@${prefix}-replica1:5432/postgres?sslmode=disable"
    echo "SAGE_TEST_KILL_STANDBY_URL=postgres://postgres:postgres@${prefix}-standby2:5432/postgres?sslmode=disable"
    ;;
  down)
    down
    ;;
  *)
    echo "usage: $0 up|down <major> <prefix> [primary-port] [replica-port]" >&2
    exit 2
    ;;
esac

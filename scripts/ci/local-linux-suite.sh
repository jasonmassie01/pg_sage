#!/usr/bin/env bash
# Runs CI's Go suite on Linux with cgo and the race detector, once per
# PostgreSQL major, in local containers (ci-19). For each major it starts
# CI's two servers with CI's settings (images/postgresql.ci.conf): the
# suite's, and the exclusive one (SAGE_TEST_EXCLUSIVE_URL, ci-12; see
# local-linux-suite-server.sh). It records what the suite's server cannot
# cover (verify-test-postgres.sh), runs the test job's
# go test command in a Go container on the servers' network, then the skip
# budget (and, on PG17, the coverage floors of cmd/covfloor, ci-20), and
# writes <out-dir>/summary.md.
#
# Usage: local-linux-suite.sh <out-dir> [majors...]   (default majors 14 15 17)
#
# It never pulls an image, downloads a module or writes inside the
# repository: the source is a git archive of LOCAL_SUITE_REV (default HEAD)
# under <out-dir>/stage, vendored offline from the host module cache. It
# creates and removes only containers named LOCAL_SUITE_PREFIX* (default
# improve-ci-) and refuses a name that is already taken. Each container it
# starts carries the label pg_sage.local-suite.run=<this run's id>, and the
# cleanup removes only containers with that label, so it cannot remove
# another run's container even when two runs race for one name.
#
# Environment:
#   GO_IMAGE               Go image, needs gcc (default golang:1.27.3-alpine3.24,
#                          which has none, so the run stops on exit 7)
#   GO_ROOT                Linux Go tree mounted read-only at /opt/go, set as
#                          GOROOT and put first on PATH (default: none)
#   GO_ROOT_VOLUME         Docker volume holding such a tree, used the same way
#                          instead of GO_ROOT (default: none; never created)
#   PG_IMAGE_<N>           server image (default improve-pg:<N>, postgres:15)
#   LOCAL_SUITE_PREFIX     container name prefix (must start with improve-)
#   LOCAL_SUITE_GOCACHE    Go build cache volume (default improve-ci-gocache)
#   LOCAL_SUITE_GO_MEMORY  Go container memory cap (default 6g)
#   LOCAL_SUITE_PG_MEMORY  server memory cap, its in-memory data directory
#                          included (default 1536m, as tools/pg-ephemeral.sh)
#   LOCAL_SUITE_PG_TMPFS   size of the in-memory data directory (default 2g,
#                          as pg-ephemeral.sh), or off to keep it on the
#                          container's disk, removed with the container
#   LOCAL_SUITE_REV        revision to test (default HEAD)
#   LOCAL_SUITE_DISK_GUARD disk guard script (default: tools/disk-guard.sh
#                          up to two directories above the repository)
#   LOCAL_SUITE_DRY_RUN=1  validate the inputs, print the plan and stop
#
# Exit codes: 0 every suite and gate passed, 1 a suite, e2e run, skip budget
# or coverage floor failed (see summary.md), 2 usage, 3 disk below the guard,
# 4 Docker not answering, 5 server not ready, 6 image missing locally,
# 7 GO_IMAGE has no C compiler, 8 a module is missing for go mod vendor,
# 9 the race runtime does not link, 10 a container name is taken.
set -uo pipefail

# The test job's commands in ci.yml (internal/startup/local_suite_contract_test.go
# keeps them equal; -p 4 is the one local addition, a resource cap).
readonly SUITE_CMD="go test ./... -race -cover -tags=integration -count=1 \
  -timeout 1500s -json -p 4"
readonly E2E_CMD='go test ./e2e -cover -tags=e2e -count=1 -timeout 900s -json'
readonly VALID_MAJORS="14 15 16 17 18"
readonly DEFAULT_MAJORS="14 15 17"
readonly TEST_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable'

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=local-linux-suite-server.sh
. "$here/local-linux-suite-server.sh"
repo=$(cd "$here/../.." && pwd -P)
GO_IMAGE=${GO_IMAGE-golang:1.27.3-alpine3.24}
GO_ROOT=${GO_ROOT:-}
GO_ROOT_VOLUME=${GO_ROOT_VOLUME:-}
PREFIX=${LOCAL_SUITE_PREFIX-improve-ci-}
GOCACHE_VOL=${LOCAL_SUITE_GOCACHE-improve-ci-gocache}
GO_MEMORY=${LOCAL_SUITE_GO_MEMORY-6g}
PG_MEMORY=${LOCAL_SUITE_PG_MEMORY-1536m}
PG_TMPFS=${LOCAL_SUITE_PG_TMPFS-2g}
REV=${LOCAL_SUITE_REV:-HEAD}
readonly RUN_LABEL=pg_sage.local-suite.run
RUN_ID=
CREATED=()
MAJORS=()

die() { echo "local-linux-suite: ${*:2}" >&2; exit "$1"; }
usage() { die 2 "usage: $0 <out-dir> [majors...] (majors: $VALID_MAJORS)"; }

# Docker on Git for Windows must see container paths unconverted.
dk() { local secs=$1; shift; MSYS_NO_PATHCONV=1 timeout "$secs" docker "$@"; }

host_path() {
  if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else echo "$1"; fi
}

# abs_path resolves a path that may not exist yet against its deepest
# existing ancestor.
abs_path() {
  local p=$1 rest=
  while [ ! -d "$p" ]; do
    rest="/$(basename "$p")$rest"
    p=$(dirname "$p")
  done
  echo "$(cd "$p" && pwd -P)$rest"
}

pg_image() {
  local var="PG_IMAGE_$1" def="improve-pg:$1"
  [ "$1" = 15 ] && def=postgres:15
  echo "${!var-$def}"
}

check_image_name() {
  [[ $2 =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]] || die 2 "$1 '$2' is not an image name"
}

parse_args() {
  [ $# -ge 1 ] && [ -n "$1" ] || usage
  OUT=$(abs_path "$1")
  shift
  [ $# -gt 0 ] || set -- $DEFAULT_MAJORS
  local m
  for m in "$@"; do
    [[ " $VALID_MAJORS " == *" $m "* ]] ||
      die 2 "unknown PostgreSQL major '$m': want one of $VALID_MAJORS"
    [[ " ${MAJORS[*]-} " == *" $m "* ]] && die 2 "PostgreSQL major $m is given twice"
    MAJORS+=("$m")
  done
  case "$OUT/" in "$repo"/*) die 2 "out-dir $OUT is inside the repository $repo" ;; esac
  if [ -d "$OUT" ] && [ -n "$(ls -A "$OUT")" ]; then
    die 2 "out-dir $OUT is not empty: give a new directory for each run"
  fi
}

validate_env() {
  [[ $PREFIX =~ ^improve-[a-z0-9-]*$ ]] ||
    die 2 "LOCAL_SUITE_PREFIX '$PREFIX' must start with improve- ([a-z0-9-] only)"
  [[ $GOCACHE_VOL =~ ^improve-[a-z0-9-]*$ ]] ||
    die 2 "LOCAL_SUITE_GOCACHE '$GOCACHE_VOL' must start with improve- ([a-z0-9-] only)"
  [[ $GO_MEMORY =~ ^[1-9][0-9]*[mg]$ ]] ||
    die 2 "LOCAL_SUITE_GO_MEMORY '$GO_MEMORY' must be a size such as 6g or 4096m"
  [[ $PG_MEMORY =~ ^[1-9][0-9]*[mg]$ ]] ||
    die 2 "LOCAL_SUITE_PG_MEMORY '$PG_MEMORY' must be a size such as 1536m or 4g"
  [[ $PG_TMPFS =~ ^([1-9][0-9]*[mg]|off)$ ]] ||
    die 2 "LOCAL_SUITE_PG_TMPFS '$PG_TMPFS' must be a size such as 2g, or off"
  if [ -n "$GO_ROOT" ] && [ ! -f "$GO_ROOT/bin/go" ]; then
    die 2 "GO_ROOT $GO_ROOT has no bin/go (a Linux Go tree is expected)"
  fi
  if [ -n "$GO_ROOT_VOLUME" ]; then
    [ -z "$GO_ROOT" ] || die 2 "GO_ROOT and GO_ROOT_VOLUME are both set: give one Go tree"
    [[ $GO_ROOT_VOLUME =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] ||
      die 2 "GO_ROOT_VOLUME '$GO_ROOT_VOLUME' is not a volume name"
  fi
  check_image_name GO_IMAGE "$GO_IMAGE"
  local m
  for m in "${MAJORS[@]}"; do check_image_name "PG_IMAGE_$m" "$(pg_image "$m")"; done
  REV_SHA=$(git -C "$repo" rev-parse --verify --quiet "$REV^{commit}") ||
    die 2 "LOCAL_SUITE_REV '$REV' is not a commit in $repo"
}

# go_root_desc: the Go tree the Go containers use, as run.txt records it.
go_root_desc() {
  if [ -n "$GO_ROOT_VOLUME" ]; then echo "volume:$GO_ROOT_VOLUME"; else echo "${GO_ROOT:-none}"; fi
}

print_plan() {
  local m root
  root=$(go_root_desc)
  echo "dry run: inputs valid"
  for m in "${MAJORS[@]}"; do
    echo "plan pg$m server=${PREFIX}pg$m image=$(pg_image "$m") go=${PREFIX}go-pg$m" \
      "exclusive=${PREFIX}exclusive-pg$m go-image=$GO_IMAGE go-root=$root"
  done
  echo "cache=$GOCACHE_VOL rev=$REV_SHA out=$OUT"
}

disk_guard() {
  local guard=${LOCAL_SUITE_DISK_GUARD:-} c
  if [ -z "$guard" ]; then
    for c in "$repo/tools" "$repo/../tools" "$repo/../../tools"; do
      [ -f "$c/disk-guard.sh" ] && { guard=$c/disk-guard.sh; break; }
    done
  fi
  [ -n "$guard" ] || { echo "local-linux-suite: no disk guard found" >&2; return; }
  bash "$guard" >&2 || die 3 "disk guard $guard failed: free disk space first"
}

docker_ok() {
  dk 60 info --format '{{.ServerVersion}}' >/dev/null 2>&1 ||
    die 4 "docker did not answer 'docker info' within 60s"
}

check_images() {
  local m
  dk 60 image inspect "$GO_IMAGE" >/dev/null 2>&1 ||
    die 6 "GO_IMAGE $GO_IMAGE is not present locally; the script never pulls"
  if [ -n "$GO_ROOT_VOLUME" ]; then
    dk 60 volume inspect "$GO_ROOT_VOLUME" >/dev/null 2>&1 ||
      die 6 "GO_ROOT_VOLUME $GO_ROOT_VOLUME is not present locally; the script never creates it"
  fi
  for m in "${MAJORS[@]}"; do
    dk 60 image inspect "$(pg_image "$m")" >/dev/null 2>&1 ||
      die 6 "PG_IMAGE_$m $(pg_image "$m") is not present locally; the script never pulls"
  done
}

# claim refuses a container name that is taken and records it for cleanup.
claim() {
  if dk 60 container inspect "$1" >/dev/null 2>&1; then
    die 10 "container $1 already exists (another run?): remove it or set LOCAL_SUITE_PREFIX"
  fi
  CREATED+=("$1")
}

# cleanup removes the containers this run claimed that still exist and
# carry this run's label; never a name outside improve-, and never a
# container that another run started under a name claimed here. It goes
# newest first, so a container that joined a server's network namespace
# is removed before that server.
cleanup() {
  local c owner i
  for ((i = ${#CREATED[@]} - 1; i >= 0; i--)); do
    c=${CREATED[i]}
    case "$c" in improve-?*) ;; *) continue ;; esac
    owner=$(dk 60 container inspect -f "{{index .Config.Labels \"$RUN_LABEL\"}}" "$c" \
      2>/dev/null) || continue
    [ "$owner" = "$RUN_ID" ] || continue
    dk 120 rm -f -v "$c" >/dev/null 2>&1 || echo "local-linux-suite: could not remove $c" >&2
  done
}

# go_args <name> <network>: the Go container's options.
go_args() {
  GO_ARGS=(--rm --name "$1" --label "$RUN_LABEL=$RUN_ID" --network "$2"
    --memory="$GO_MEMORY" --memory-swap="$GO_MEMORY"
    --cpus=4 --cpu-shares=256 --oom-score-adj=500
    -e CGO_ENABLED=1 -e GOPROXY=off -e GOTOOLCHAIN=local -e GOCACHE=/gocache
    -v "$GOCACHE_VOL:/gocache")
  local src=
  [ -n "$GO_ROOT" ] && src=$(host_path "$GO_ROOT")
  [ -n "$GO_ROOT_VOLUME" ] && src=$GO_ROOT_VOLUME
  if [ -n "$src" ]; then
    GO_ARGS+=(-v "$src:/opt/go:ro" -e GOROOT=/opt/go
      -e "PATH=/opt/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
  fi
}

check_toolchain() {
  claim "${PREFIX}probe"
  go_args "${PREFIX}probe" none
  dk 300 run "${GO_ARGS[@]}" "$GO_IMAGE" sh -c 'command -v gcc' >/dev/null 2>&1 ||
    die 7 "GO_IMAGE has no C compiler: cgo and -race cannot run ($GO_IMAGE)"
  mkdir -p "$OUT/probe"
  printf 'module raceprobe\n\ngo 1.21\n' >"$OUT/probe/go.mod"
  cat >"$OUT/probe/probe_test.go" <<'EOF'
package raceprobe

import (
	"sync"
	"testing"
)

func TestRaceRuntimeLinks(t *testing.T) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	n := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); mu.Lock(); n++; mu.Unlock() }()
	}
	wg.Wait()
	if n != 4 {
		t.Fatalf("n = %d, want 4", n)
	}
}
EOF
  if ! dk 600 run "${GO_ARGS[@]}" -e GOFLAGS= -v "$(host_path "$OUT/probe"):/probe" \
    -w /probe "$GO_IMAGE" sh -c 'go version && go test -race -count=1 ./...' \
    >"$OUT/race-probe.txt" 2>&1; then
    tail -20 "$OUT/race-probe.txt" >&2
    die 9 "the race runtime does not link in GO_IMAGE; use a glibc image ($GO_IMAGE)"
  fi
}

stage_source() {
  mkdir -p "$OUT/stage" || die 1 "cannot create $OUT/stage"
  git -c core.autocrlf=false -C "$repo" archive --format=tar "$REV_SHA" |
    tar -x -C "$OUT/stage"
  local st=("${PIPESTATUS[@]}")
  [ "${st[0]}" -eq 0 ] && [ "${st[1]}" -eq 0 ] ||
    die 1 "staging $REV_SHA into $OUT/stage failed (git archive ${st[0]}, tar ${st[1]})"
  printf 'rev=%s\ngo_image=%s\ngo_root=%s\ngo_version=%s\nmajors=%s\n' "$REV_SHA" \
    "$GO_IMAGE" "$(go_root_desc)" "$(grep -m1 '^go version ' "$OUT/race-probe.txt")" \
    "${MAJORS[*]}" >"$OUT/run.txt"
  echo "pg_memory=$PG_MEMORY pg_tmpfs=$PG_TMPFS" >>"$OUT/run.txt"
}

# pack_stage: one tar of the vendored stage for the Go containers. Docker
# Desktop's bind mounts of a Windows directory fail go's directory reads
# under load ("readdirent ...: cannot allocate memory"), so each container
# mounts this file read-only and unpacks it into its own filesystem.
pack_stage() {
  tar -cf "$OUT/stage.tar" -C "$OUT/stage" . || die 1 "packing $OUT/stage into stage.tar failed"
}

vendor_source() {
  local mod="$OUT/stage/sidecar"
  if ! (cd "$mod" && GOFLAGS=-mod=mod GOPROXY=off go mod vendor) >"$OUT/vendor.txt" 2>&1; then
    cat "$OUT/vendor.txt" >&2
    die 8 "go mod vendor failed: a module is missing from the host module cache" \
      "(nothing is downloaded); see $OUT/vendor.txt"
  fi
  if ! (cd "$mod" && GOFLAGS=-mod=mod GOPROXY=off go mod verify) >>"$OUT/vendor.txt" 2>&1; then
    cat "$OUT/vendor.txt" >&2
    die 8 "go mod verify failed in $mod; see $OUT/vendor.txt"
  fi
}

# go_test <major> <command> <output>: runs a go test command in the Go
# container on the servers' network and returns its status.
go_test() {
  local m=$1 name=${PREFIX}go-pg$1 st=0
  claim "$name"
  go_args "$name" "container:${PREFIX}pg$m"
  dk 5400 run "${GO_ARGS[@]}" -v "$(host_path "$OUT/stage.tar"):/stage.tar:ro" -w / \
    -e "GOFLAGS=-mod=vendor -buildvcs=false" -e "SAGE_TEST_DATABASE_URL=$TEST_URL" \
    -e "SAGE_TEST_EXCLUSIVE_URL=$EXCLUSIVE_URL" \
    -e PGHOST=127.0.0.1 -e PGPORT=5432 -e PGUSER=postgres -e PGPASSWORD=postgres \
    -e PGDATABASE=postgres "$GO_IMAGE" \
    sh -c "mkdir -p /work && tar -xf /stage.tar -C /work && cd /work/sidecar && $2" \
    >"$3" 2>"${3%.json}.stderr.txt" || st=$?
  return "$st"
}

# skip_budget <major> <inputs...>: CI's skip budget (ci.yml: test job and
# integration-matrix) on this run's -json streams.
skip_budget() {
  local m=$1 st=0
  shift
  (cd "$OUT/stage/sidecar" && cat "$@" | GOFLAGS=-mod=vendor GOPROXY=off \
    go run ./cmd/skipbudget -pg "$m" \
    -require 'internal/alerting TestEvaluate_.*' \
    -require 'internal/notify TestPhase2_(ProcessRule|LoadChannel|LoadMatchingRules)_.*') \
    >"$OUT/pg$m/skipbudget.txt" 2>&1 || st=$?
  echo "skipbudget=$st" >>"$OUT/pg$m/status.txt"
  return "$st"
}

# cov_floor <major>: CI's coverage floors (ci.yml: test job, ci-20) on the
# suite stream.
cov_floor() {
  local m=$1 st=0
  (cd "$OUT/stage/sidecar" && GOFLAGS=-mod=vendor GOPROXY=off \
    go run ./cmd/covfloor <"$OUT/pg$m/suite.json") >"$OUT/pg$m/covfloor.txt" 2>&1 || st=$?
  echo "covfloor=$st" >>"$OUT/pg$m/status.txt"
  return "$st"
}

run_major() {
  local m=$1 dir=$OUT/pg$1 st=0 inputs
  mkdir -p "$dir"
  echo "image=$(pg_image "$m")" >"$dir/status.txt"
  disk_guard
  start_server "$m"
  start_exclusive "$m"
  check_server "$m"
  go_test "$m" "$SUITE_CMD" "$dir/suite.json" || st=$?
  echo "suite=$st" >>"$dir/status.txt"
  inputs=("$dir/suite.json")
  if [ "$m" = 17 ]; then
    local e2e=0
    go_test "$m" "$E2E_CMD" "$dir/e2e.json" || e2e=$?
    echo "e2e=$e2e" >>"$dir/status.txt"
    [ "$e2e" -eq 0 ] || st=1
    inputs+=("$dir/e2e.json")
  fi
  skip_budget "$m" "${inputs[@]}" || st=1
  if [ "$m" = 17 ]; then
    cov_floor "$m" || st=1
  fi
  cleanup
  CREATED=()
  return "$st"
}

main() {
  parse_args "$@"
  validate_env
  if [ "${LOCAL_SUITE_DRY_RUN:-}" = 1 ]; then
    print_plan
    exit 0
  fi
  disk_guard
  docker_ok
  check_images
  mkdir -p "$OUT" || die 2 "cannot create out-dir $OUT"
  RUN_ID="$(date +%s)-$$-$RANDOM"
  trap cleanup EXIT
  trap 'exit 130' INT TERM
  check_toolchain
  stage_source
  vendor_source
  pack_stage
  local m failed=0
  for m in "${MAJORS[@]}"; do
    run_major "$m" || failed=1
  done
  bash "$here/local-linux-suite-summary.sh" "$OUT" "${MAJORS[@]}" ||
    die 1 "the summary failed; the raw results are under $OUT"
  echo "summary: $OUT/summary.md"
  exit "$failed"
}

main "$@"

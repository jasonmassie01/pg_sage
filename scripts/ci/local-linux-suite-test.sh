#!/usr/bin/env bash
# Tests local-linux-suite.sh (ci-19) without Docker or PostgreSQL: dry runs
# (LOCAL_SUITE_DRY_RUN=1 validates the inputs and stops before Docker) and
# runs against stub docker, go and disk-guard commands on PATH, which log
# every call and fail on demand. Also checks local-linux-suite-summary.sh
# on a fixture go test -json stream.
#
# Concurrency: the script runs once per machine; two runs differ by their
# container names, which the dry-run cases below assert. State transitions:
# none, the script keeps no state between runs.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/local-linux-suite.sh"
summary="$here/local-linux-suite-summary.sh"
repo=$(cd "$here/../.." && pwd -P)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
log="$work/docker.log"
state="$work/containers" # the stub's live containers: "<name> <run label>"

fail() { echo "FAIL $1" >&2; failures=$((failures + 1)); }

# expect <want-status> <output-pattern> <description> -- <env/args...>
expect() {
  local want=$1 pattern=$2 name=$3
  shift 4
  local status
  : >"$log"
  : >"$state"
  out=$(env PATH="$work/bin:$PATH" STUB_LOG="$log" STUB_STATE="$state" "$@" 2>&1)
  status=$?
  if [[ $status -ne $want ]]; then
    fail "$name: exit $status, want $want; output: $out"
  elif [[ -n $pattern ]] && ! grep -Eq -- "$pattern" <<<"$out"; then
    fail "$name: output does not match /$pattern/: $out"
  else
    echo "ok   $name"
  fi
}

# no_docker_call <description>: the last case never ran docker.
no_docker_call() {
  if [[ -s $log ]]; then fail "$1: docker was called: $(cat "$log")"; else echo "ok   $1"; fi
}

mkdir -p "$work/bin"
cat >"$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >>"$STUB_LOG"
case "$1" in
info) [[ -z ${STUB_DOCKER_DOWN:-} ]] || exit 1; echo 27.0.0 ;;
image)
  for img in ${STUB_MISSING_IMAGES:-}; do
    [[ $3 == "$img" ]] && { echo "Error: No such image: $3" >&2; exit 1; }
  done
  echo sha256:stub ;;
volume) # inspect: fails for a volume named in STUB_MISSING_VOLUMES
  for v in ${STUB_MISSING_VOLUMES:-}; do
    [[ $3 == "$v" ]] && { echo "Error: No such volume: $3" >&2; exit 1; }
  done
  echo "[]" ;;
container) # inspect: the owner label, or the IP, of a known container
  name=${!#}
  for c in ${STUB_EXISTING:-}; do [[ $name == "$c" ]] && { echo other-run; exit 0; }; done
  line=$(grep "^$name " "$STUB_STATE") || exit 1
  if [[ $* == *IPAddress* ]]; then echo "${STUB_SERVER_IP-172.17.0.9}"
  else echo "${line#* }"; fi ;;
rm)
  grep -v "^${!#} " "$STUB_STATE" >"$STUB_STATE.new"; mv "$STUB_STATE.new" "$STUB_STATE" ;;
run)
  args=("$@") name= label=
  for ((i = 0; i < ${#args[@]}; i++)); do
    case ${args[i]} in
    --name) name=${args[i + 1]} ;;
    --label) [[ ${args[i + 1]} == pg_sage.local-suite.run=* ]] && label=${args[i + 1]#*=} ;;
    esac
  done
  if [[ $name == "${STUB_TAKEN_ON_RUN:-}" ]]; then
    # Another run took the name between the claim and this run.
    echo "$name other-run" >>"$STUB_STATE"
    echo "docker: Conflict. The container name \"/$name\" is already in use" >&2
    exit 125
  fi
  if [[ $* == *"command -v gcc"* ]]; then
    [[ -z ${STUB_NO_GCC:-} ]] || exit 1; echo /usr/bin/gcc
  elif [[ $* == *"-race"*"/probe"* || $* == *"/probe"*"-race"* ]]; then
    [[ -z ${STUB_RACE_FAIL:-} ]] || { echo "ld: cannot find -lracego" >&2; exit 1; }
    echo "go version go1.27.3 linux/amd64"
  elif [[ $2 == -d ]]; then
    echo "$name $label" >>"$STUB_STATE"
  fi ;;
esac
exit 0
EOF
cat >"$work/bin/go" <<'EOF'
#!/usr/bin/env bash
echo "go $*" >>"$STUB_LOG"
if [[ $1 == mod && $2 == vendor ]]; then
  if [[ -n ${STUB_GO_MISSING:-} ]]; then
    echo "go: example.com/missing@v1.2.3: module lookup disabled by GOPROXY=off" >&2
    exit 1
  fi
  mkdir -p vendor && echo "# stub" >vendor/modules.txt
fi
if [[ $1 == run && $2 == ./cmd/covfloor ]]; then
  echo "covfloor: stub"
  exit "${STUB_COVFLOOR_EXIT:-0}"
fi
exit 0
EOF
cat >"$work/bin/guard.sh" <<'EOF'
#!/usr/bin/env bash
# Mirrors tools/disk-guard.sh: exit 3 when free space is below the floor.
min=${1:-${PG_SAGE_DISK_GUARD_GB:-40}}
if [ "$FAKE_FREE_GB" -lt "$min" ]; then
  echo "BLOCKED: C: has ${FAKE_FREE_GB} GB free, under the ${min} GB guard" >&2; exit 3
fi
echo "disk ok: C: has ${FAKE_FREE_GB} GB free (guard ${min} GB)"
EOF
chmod +x "$work/bin/docker" "$work/bin/go" "$work/bin/guard.sh"
guard="LOCAL_SUITE_DISK_GUARD=$work/bin/guard.sh"
dry="LOCAL_SUITE_DRY_RUN=1"
newout() { local d; d=$(mktemp -d "$work/out.XXXX"); rmdir "$d"; echo "$d"; }

# Happy path and nil/empty: no majors selects 14 15 17.
expect 0 'plan pg14 server=improve-ci-pg14 image=improve-pg:14 go=improve-ci-go-pg14' \
  "dry run: default majors" -- $dry bash "$script" "$(newout)"
grep -q 'plan pg15 server=improve-ci-pg15 image=postgres:15 go=improve-ci-go-pg15' <<<"$out" ||
  fail "dry run: pg15 plan line missing: $out"
grep -q 'plan pg17 server=improve-ci-pg17 image=improve-pg:17 go=improve-ci-go-pg17' <<<"$out" ||
  fail "dry run: pg17 plan line missing: $out"
[[ $(grep -c '^plan pg' <<<"$out") -eq 3 ]] || fail "dry run: want 3 plan lines: $out"
grep -q 'go-image=golang:1.27.3-alpine3.24 go-root=none' <<<"$out" ||
  fail "dry run: empty GO_ROOT must mount nothing: $out"
no_docker_call "dry run touches no docker"

# Concurrency: two runs with other prefixes or majors get distinct names.
expect 0 'plan pg17 server=improve-t4-a-pg17 .* go=improve-t4-a-go-pg17' "dry run: prefix A" -- \
  $dry LOCAL_SUITE_PREFIX=improve-t4-a- bash "$script" "$(newout)" 17
names_a=$(grep '^plan' <<<"$out" | grep -oE '(server|go)=improve-[a-z0-9-]*' | sort -u)
expect 0 'plan pg17 server=improve-t4-b-pg17' "dry run: prefix B" -- \
  $dry LOCAL_SUITE_PREFIX=improve-t4-b- bash "$script" "$(newout)" 17
names_b=$(grep '^plan' <<<"$out" | grep -oE '(server|go)=improve-[a-z0-9-]*' | sort -u)
[[ -n $(comm -12 <(echo "$names_a") <(echo "$names_b")) ]] &&
  fail "two prefixes share a container name: $names_a / $names_b"
mkdir -p "$work/goroot/bin" && touch "$work/goroot/bin/go"
expect 0 'go-root=.*goroot' "dry run: GO_ROOT with bin/go" -- \
  $dry GO_ROOT="$work/goroot" bash "$script" "$(newout)" 14
expect 0 'go-image=golang:1.25 go-root=volume:improve-go1273-root' \
  "dry run: GO_ROOT_VOLUME" -- \
  $dry GO_IMAGE=golang:1.25 GO_ROOT_VOLUME=improve-go1273-root bash "$script" "$(newout)" 14
no_docker_call "a GO_ROOT_VOLUME dry run touches no docker"

# Invalid input.
expect 2 'usage: .*local-linux-suite.sh <out-dir>' "missing out-dir" -- $dry bash "$script"
expect 2 "unknown PostgreSQL major '13'" "major 13" -- $dry bash "$script" "$(newout)" 13
expect 2 "unknown PostgreSQL major 'abc'" "major abc" -- $dry bash "$script" "$(newout)" abc
expect 2 "PostgreSQL major 17 is given twice" "duplicate major" -- \
  $dry bash "$script" "$(newout)" 17 17
expect 2 'out-dir .* is inside the repository' "out-dir inside the repo" -- \
  $dry bash "$script" "$repo/sidecar/local-suite-out"
mkdir -p "$work/full" && touch "$work/full/x"
expect 2 'out-dir .*full is not empty' "non-empty out-dir" -- $dry bash "$script" "$work/full"
expect 2 "LOCAL_SUITE_PREFIX 'lifeos-' must start with improve-" "prefix outside improve-" -- \
  $dry LOCAL_SUITE_PREFIX=lifeos- bash "$script" "$(newout)"
no_docker_call "a refused prefix touches no container"
expect 2 "LOCAL_SUITE_GOCACHE 'gocache' must start with improve-" "cache volume name" -- \
  $dry LOCAL_SUITE_GOCACHE=gocache bash "$script" "$(newout)"
expect 2 'GO_ROOT .*nope has no bin/go' "GO_ROOT without go" -- \
  $dry GO_ROOT="$work/nope" bash "$script" "$(newout)"
expect 2 'GO_ROOT and GO_ROOT_VOLUME are both set' "GO_ROOT and GO_ROOT_VOLUME" -- \
  $dry GO_ROOT="$work/goroot" GO_ROOT_VOLUME=improve-go1273-root bash "$script" "$(newout)"
expect 2 "GO_ROOT_VOLUME 'bad/vol' is not a volume name" "bad GO_ROOT_VOLUME" -- \
  $dry GO_ROOT_VOLUME=bad/vol bash "$script" "$(newout)"
expect 2 "GO_ROOT_VOLUME '-v' is not a volume name" "GO_ROOT_VOLUME as an option" -- \
  $dry GO_ROOT_VOLUME=-v bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_PG_TMPFS '2' must be a size such as 2g, or off" "bad tmpfs size" -- \
  $dry LOCAL_SUITE_PG_TMPFS=2 bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_PG_TMPFS 'OFF' must be a size" "tmpfs off is lower case" -- \
  $dry LOCAL_SUITE_PG_TMPFS=OFF bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_PG_MEMORY '1.5g' must be a size" "bad server memory cap" -- \
  $dry LOCAL_SUITE_PG_MEMORY=1.5g bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_PG_MEMORY '0m' must be a size" "zero server memory cap" -- \
  $dry LOCAL_SUITE_PG_MEMORY=0m bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_GO_MEMORY '6 g'" "bad memory cap" -- \
  $dry "LOCAL_SUITE_GO_MEMORY=6 g" bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_REV 'no-such-rev' is not a commit" "unknown revision" -- \
  $dry LOCAL_SUITE_REV=no-such-rev bash "$script" "$(newout)"
expect 2 "GO_IMAGE '' is not an image name" "empty GO_IMAGE" -- \
  $dry GO_IMAGE= bash "$script" "$(newout)"

# Boundary: the disk guard just below and at its floor.
expect 3 'BLOCKED: C: has 39 GB free' "disk guard below its floor" -- \
  $guard FAKE_FREE_GB=39 bash "$script" "$(newout)" 17
no_docker_call "a blocked disk guard touches no docker"
expect 4 'docker did not answer' "disk guard at its floor passes; docker down" -- \
  $guard FAKE_FREE_GB=40 STUB_DOCKER_DOWN=1 bash "$script" "$(newout)" 17

# Error propagation from the image checks, the race probe and vendoring.
expect 6 "GO_IMAGE golang:9.9-none is not present locally" "GO_IMAGE missing locally" -- \
  $guard FAKE_FREE_GB=99 GO_IMAGE=golang:9.9-none STUB_MISSING_IMAGES=golang:9.9-none \
  bash "$script" "$(newout)" 17
grep -q 'docker pull' "$log" && fail "the script pulled an image: $(cat "$log")"
expect 6 "PG_IMAGE_15 postgres:15 is not present locally" "PG image missing locally" -- \
  $guard FAKE_FREE_GB=99 STUB_MISSING_IMAGES=postgres:15 bash "$script" "$(newout)"
expect 6 "GO_ROOT_VOLUME improve-none is not present locally" "GO_ROOT_VOLUME missing" -- \
  $guard FAKE_FREE_GB=99 GO_ROOT_VOLUME=improve-none STUB_MISSING_VOLUMES=improve-none \
  bash "$script" "$(newout)" 17
grep -q 'docker volume create' "$log" && fail "the script created a volume: $(cat "$log")"
grep -q 'docker run' "$log" && fail "a container started without its Go volume: $(cat "$log")"
expect 7 'GO_IMAGE has no C compiler: cgo and -race cannot run' "no gcc" -- \
  $guard FAKE_FREE_GB=99 STUB_NO_GCC=1 bash "$script" "$(newout)" 17
grep -q 'CGO_ENABLED=0' "$log" && fail "no gcc fell back to CGO_ENABLED=0: $(cat "$log")"
expect 9 'the race runtime does not link in GO_IMAGE; use a glibc image' "race probe fails" -- \
  $guard FAKE_FREE_GB=99 STUB_RACE_FAIL=1 bash "$script" "$(newout)" 17
grep -q '/opt/go' "$log" && fail "empty GO_ROOT mounted /opt/go: $(cat "$log")"
grep 'docker run' "$log" | grep -vq -- '--name improve-ci-' &&
  fail "a probe container is not named improve-ci-*: $(grep 'docker run' "$log")"

before=$(git -C "$repo" status --porcelain)
o=$(newout)
expect 8 'example.com/missing@v1.2.3' "missing module in go mod vendor" -- \
  $guard FAKE_FREE_GB=99 STUB_GO_MISSING=1 bash "$script" "$o" 17
grep -q 'go mod vendor failed' <<<"$out" || fail "exit 8 without its message: $out"
[[ -f $o/stage/sidecar/go.mod ]] || fail "the source was not staged under $o/stage"
grep -q 'docker run.*improve-ci-pg17' "$log" && fail "a server started after exit 8"

# Boundary: a container that already holds the server's name is never
# removed, and the run refuses to share it.
o=$(newout)
expect 10 'container improve-ci-pg17 already exists' "existing server name" -- \
  $guard FAKE_FREE_GB=99 STUB_EXISTING=improve-ci-pg17 bash "$script" "$o" 17
grep -q 'docker rm' "$log" && fail "a container the run did not create was removed: $(cat "$log")"
[[ -f $o/stage/sidecar/vendor/modules.txt ]] || fail "vendor/ was not made in the stage"

# Error propagation: a server with no container IP stops the run before
# any suite starts.
expect 5 'server improve-ci-pg17 has no container IP address' "server without an IP" -- \
  $guard FAKE_FREE_GB=99 STUB_SERVER_IP= bash "$script" "$(newout)" 17
grep -q -- '--name improve-ci-go-pg17' "$log" && fail "a suite started without a server IP"
grep -q 'docker rm -f -v improve-ci-pg17$' "$log" || fail "the server was left after exit 5"

# Boundary: another run takes the name between the claim and docker run.
# The cleanup must leave that run's container alone.
expect 5 'server improve-ci-pg17 .* did not start' "name taken after the claim" -- \
  $guard FAKE_FREE_GB=99 STUB_TAKEN_ON_RUN=improve-ci-pg17 bash "$script" "$(newout)" 17
grep -q 'docker rm .*improve-ci-pg17' "$log" &&
  fail "the cleanup removed another run's improve-ci-pg17: $(cat "$log")"
grep -q '^improve-ci-pg17 other-run$' "$state" || fail "the other run's container is gone"

# Happy path through stubs: every container carries this run's label, the
# server is removed at the end, and the summary is written.
o=$(newout)
expect 0 'summary: .*summary.md' "stubbed full run" -- \
  $guard FAKE_FREE_GB=99 bash "$script" "$o" 17
runs=$(grep '^docker run' "$log")
[[ $(grep -c . <<<"$runs") -eq 5 ]] || fail "want 5 docker runs (gcc, race probe, server, suite,"\
" e2e): $runs"
ids=$(grep -oE 'pg_sage.local-suite.run=[^ ]+' <<<"$runs" | sort -u)
[[ $(grep -c . <<<"$ids") -eq 1 ]] ||
  fail "every container needs one run label with a value: $ids"
grep -q 'docker rm -f -v improve-ci-pg17$' "$log" ||
  fail "the server was not removed: $(cat "$log")"
[[ -s $state ]] && fail "containers left after the run: $(cat "$state")"
srvrun=$(grep '^docker run' "$log" | grep -- '--name improve-ci-pg17 ')
[[ $srvrun == *" --memory=1536m --memory-swap=1536m "* ]] ||
  fail "the server is not capped at the default 1536m: $srvrun"
[[ $srvrun == *" --tmpfs /var/lib/postgresql/data:rw,size=2g "* ]] ||
  fail "the server's data directory is not the default 2g tmpfs: $srvrun"
grep -q 'suite=0' "$o/pg17/status.txt" && grep -q 'skipbudget=0' "$o/pg17/status.txt" ||
  fail "status.txt lacks the suite and skip budget results: $(cat "$o/pg17/status.txt")"
[[ -f $o/summary.md ]] || fail "no summary.md after a full run"
# A bind-mounted stage directory fails go's directory reads on Docker
# Desktop ("readdirent ...: cannot allocate memory"), so the suite and e2e
# containers get one read-only tar of the stage and unpack it inside.
gotests=$(grep '^docker run' "$log" | grep -- '--name improve-ci-go-pg17 ')
[[ $(grep -c . <<<"$gotests") -eq 2 ]] || fail "want 2 suite/e2e containers: $gotests"
while IFS= read -r r; do
  [[ $r == *"/stage.tar:/stage.tar:ro "* ]] || fail "the stage tar is not mounted read-only: $r"
  [[ $r == *":/src "* ]] && fail "the stage directory is bind-mounted: $r"
  [[ $r == *"tar -xf /stage.tar -C /work && cd /work/sidecar && go test "* ]] ||
    fail "the container does not unpack the stage before go test: $r"
done <<<"$gotests"
tar -tf "$o/stage.tar" >"$work/stage-list.txt" || fail "stage.tar is not a readable tar"
grep -qx './sidecar/go.mod' "$work/stage-list.txt" || fail "stage.tar lacks ./sidecar/go.mod"
grep -qx './sidecar/vendor/modules.txt' "$work/stage-list.txt" ||
  fail "stage.tar lacks the vendored modules"
# The suite reaches the server by its container IP, never by loopback: the
# postgres images trust 127.0.0.1 without a password, so a loopback client
# would pass every password check that CI's (bridge-routed) client fails.
while IFS= read -r r; do
  [[ $r == *" -e SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@172.17.0.9:5432/"* &&
    $r == *" -e PGHOST=172.17.0.9 "* ]] ||
    fail "the suite does not reach the server by its container IP: $r"
  [[ $r == *127.0.0.1* ]] && fail "the suite still names loopback: $r"
done <<<"$gotests"
grep -q 'covfloor=0' "$o/pg17/status.txt" ||
  fail "status.txt lacks the coverage floors result: $(cat "$o/pg17/status.txt")"
grep -q '^go run ./cmd/covfloor' "$log" || fail "covfloor did not run: $(cat "$log")"

# GO_ROOT_VOLUME: every Go container mounts the volume read-only at /opt/go
# with GOROOT and PATH pointing at it; the server never mounts it.
o=$(newout)
expect 0 'summary: .*summary.md' "stubbed full run with GO_ROOT_VOLUME" -- \
  $guard FAKE_FREE_GB=99 GO_IMAGE=golang:1.25 GO_ROOT_VOLUME=improve-go1273-root \
  LOCAL_SUITE_PG_MEMORY=4g LOCAL_SUITE_PG_TMPFS=off bash "$script" "$o" 17
grep -q '^docker volume inspect improve-go1273-root$' "$log" ||
  fail "the Go volume was not checked: $(cat "$log")"
goruns=$(grep '^docker run' "$log" | grep -v -- '--name improve-ci-pg17 ')
[[ $(grep -c . <<<"$goruns") -eq 4 ]] || fail "want 4 Go containers: $goruns"
while IFS= read -r r; do
  [[ $r == *" -v improve-go1273-root:/opt/go:ro "* && $r == *" -e GOROOT=/opt/go "* &&
    $r == *" -e PATH=/opt/go/bin:"* ]] ||
    fail "a Go container lacks the read-only Go volume, GOROOT or PATH: $r"
done <<<"$goruns"
# Here-strings, not pipes into grep -q: under pipefail an early grep -q exit
# can fail the pipe and hide the very match these checks look for.
srvrun=$(grep '^docker run' "$log" | grep -- '--name improve-ci-pg17 ')
grep -q improve-go1273-root <<<"$srvrun" && fail "the server mounted the Go volume: $srvrun"
[[ $srvrun == *" --memory=4g --memory-swap=4g "* ]] ||
  fail "LOCAL_SUITE_PG_MEMORY=4g did not cap the server at 4g: $srvrun"
# LOCAL_SUITE_PG_TMPFS=off: the data directory stays on the container's
# disk (an anonymous volume that docker rm -v removes), not in memory.
[[ $srvrun == *"--tmpfs"* ]] && fail "LOCAL_SUITE_PG_TMPFS=off still mounted a tmpfs: $srvrun"
[[ $srvrun == *" -e PGDATA=/var/lib/postgresql/data/pgdata "* ]] ||
  fail "the server lost its PGDATA: $srvrun"
grep -qx 'pg_memory=4g pg_tmpfs=off' "$o/run.txt" ||
  fail "run.txt lacks the server caps: $(cat "$o/run.txt")"
mounts=$(grep -oE 'improve-go1273-root:[^ ]*' "$log")
grep -vqx 'improve-go1273-root:/opt/go:ro' <<<"$mounts" &&
  fail "the Go volume was mounted writable: $mounts"
grep -qx 'go_root=volume:improve-go1273-root' "$o/run.txt" ||
  fail "run.txt lacks the Go volume: $(cat "$o/run.txt")"
grep -qx 'go_version=go version go1.27.3 linux/amd64' "$o/run.txt" ||
  fail "run.txt lacks the toolchain's go version: $(cat "$o/run.txt")"

# ci-20: a package under its floor fails the run; covfloor runs on PG17 only.
o=$(newout)
expect 1 'summary: .*summary.md' "coverage floor failure" -- \
  $guard FAKE_FREE_GB=99 STUB_COVFLOOR_EXIT=1 bash "$script" "$o" 15 17
grep -q 'covfloor=1' "$o/pg17/status.txt" ||
  fail "status.txt lacks covfloor=1: $(cat "$o/pg17/status.txt")"
grep -q 'covfloor' "$o/pg15/status.txt" && fail "covfloor ran on PG15: $(cat "$o/pg15/status.txt")"
[[ $(grep -c '^go run ./cmd/covfloor' "$log") -eq 1 ]] ||
  fail "want one covfloor call for majors 15 and 17: $(grep covfloor "$log")"

after=$(git -C "$repo" status --porcelain)
[[ $before == "$after" ]] || fail "a run changed the worktree: $before -> $after"

bash "$here/local-linux-suite-summary-test.sh" || failures=$((failures + 1))

if [[ $failures -gt 0 ]]; then
  echo "$failures local suite check(s) failed" >&2
  exit 1
fi
echo "all local suite checks passed"

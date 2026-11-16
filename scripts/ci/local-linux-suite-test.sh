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
container) # inspect: prints the owner label of a known container
  name=${!#}
  for c in ${STUB_EXISTING:-}; do [[ $name == "$c" ]] && { echo other-run; exit 0; }; done
  line=$(grep "^$name " "$STUB_STATE") || exit 1
  echo "${line#* }" ;;
exec) # a password-less loopback login fails unless STUB_LOOPBACK_TRUST is set
  if [[ $* == *" psql -w -h 127.0.0.1 "* ]]; then
    [[ -n ${STUB_LOOPBACK_TRUST:-} ]] || { echo "fe_sendauth: no password supplied" >&2; exit 2; }
  fi
  [[ $* == *"SHOW pg_stat_statements.max"* ]] && echo "${STUB_PSS_MAX:-100}" ;;
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
grep -q 'go=improve-ci-go-pg17 exclusive=improve-ci-exclusive-pg17 ' <<<"$out" ||
  fail "dry run: pg17 lacks its exclusive server (SAGE_TEST_EXCLUSIVE_URL): $out"
no_docker_call "dry run touches no docker"

# Concurrency: two runs with other prefixes or majors get distinct names.
expect 0 'plan pg17 server=improve-t4-a-pg17 .* go=improve-t4-a-go-pg17' "dry run: prefix A" -- \
  $dry LOCAL_SUITE_PREFIX=improve-t4-a- bash "$script" "$(newout)" 17
names_a=$(grep '^plan' <<<"$out" | grep -oE '(server|exclusive|go)=improve-[a-z0-9-]*' | sort -u)
expect 0 'plan pg17 server=improve-t4-b-pg17' "dry run: prefix B" -- \
  $dry LOCAL_SUITE_PREFIX=improve-t4-b- bash "$script" "$(newout)" 17
names_b=$(grep '^plan' <<<"$out" | grep -oE '(server|exclusive|go)=improve-[a-z0-9-]*' | sort -u)
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

# Error propagation: a server that still lets a password-less loopback
# login in stops the run before any suite starts, and is removed.
expect 5 'server improve-ci-pg17 still accepts a loopback login without a password' \
  "loopback still trusted" -- $guard FAKE_FREE_GB=99 STUB_LOOPBACK_TRUST=1 bash "$script" \
  "$(newout)" 17
grep -q -- '--name improve-ci-go-pg17' "$log" && fail "a suite started on a trusting server"
grep -q 'docker rm -f -v improve-ci-pg17$' "$log" || fail "the server was left after exit 5"

# Boundary: another run takes the name between the claim and docker run.
# The cleanup must leave that run's container alone.
expect 5 'server improve-ci-pg17 .* did not start' "name taken after the claim" -- \
  $guard FAKE_FREE_GB=99 STUB_TAKEN_ON_RUN=improve-ci-pg17 bash "$script" "$(newout)" 17
grep -q 'docker rm .*improve-ci-pg17' "$log" &&
  fail "the cleanup removed another run's improve-ci-pg17: $(cat "$log")"
grep -q '^improve-ci-pg17 other-run$' "$state" || fail "the other run's container is gone"

# Error propagation: an exclusive server that cannot start stops the run
# before any suite, removes the suite server and leaves alone the other
# run's container that took the name.
expect 5 'server improve-ci-exclusive-pg17 .* did not start' "exclusive server fails" -- \
  $guard FAKE_FREE_GB=99 STUB_TAKEN_ON_RUN=improve-ci-exclusive-pg17 bash "$script" \
  "$(newout)" 17
grep -q -- '--name improve-ci-go-pg17' "$log" && fail "a suite started without its exclusive server"
grep -q 'docker rm -f -v improve-ci-pg17$' "$log" || fail "the server was left after exit 5"
grep -q 'docker rm .*improve-ci-exclusive-pg17' "$log" &&
  fail "the cleanup removed another run's improve-ci-exclusive-pg17: $(cat "$log")"
grep -q '^improve-ci-exclusive-pg17 other-run$' "$state" || fail "the other run's container is gone"

# Happy path through stubs: every container carries this run's label, the
# server is removed at the end, and the summary is written.
o=$(newout)
expect 0 'summary: .*summary.md' "stubbed full run" -- \
  $guard FAKE_FREE_GB=99 bash "$script" "$o" 17
runs=$(grep '^docker run' "$log")
[[ $(grep -c . <<<"$runs") -eq 6 ]] || fail "want 6 docker runs (gcc, race probe, server,"\
" exclusive server, suite, e2e): $runs"
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
# The postgres images trust loopback TCP without a password; CI's client
# must pass scram-sha-256. The server's loopback host lines are switched to
# scram-sha-256 and reloaded before the suite, which connects to 127.0.0.1.
hba=$(grep -n '^docker exec improve-ci-pg17 ' "$log")
grep -q 'sh -c .*pg_hba.conf' <<<"$hba" || fail "the server's pg_hba.conf was not changed: $hba"
grep -q 'pg_reload_conf' <<<"$hba" || fail "the server did not reload pg_hba.conf: $hba"
grep -q 'psql -w -h 127.0.0.1 ' <<<"$hba" || fail "a password-less loopback login was not tried"
first_suite=$(grep -n -- '^docker run .*--name improve-ci-go-pg17 ' "$log" | head -1 | cut -d: -f1)
last_hba=$(grep 'psql -w -h 127.0.0.1 ' <<<"$hba" | tail -1 | cut -d: -f1)
[[ -n $first_suite && -n $last_hba && $last_hba -lt $first_suite ]] ||
  fail "the loopback check must run before the suite (check $last_hba, suite $first_suite)"
while IFS= read -r r; do
  [[ $r == *" -e SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/"* &&
    $r == *" -e PGHOST=127.0.0.1 "* ]] || fail "the suite does not connect to 127.0.0.1: $r"
done <<<"$gotests"
# ci-12: CI's test job gives the exclusive tests a second server from the
# same image (SAGE_TEST_EXCLUSIVE_URL). Here it joins the suite server's
# network namespace on port 5433, with the same scram and pg_stat_statements
# setup, and the cleanup removes it before the server whose namespace it uses.
xrun=$(grep '^docker run' "$log" | grep -- '--name improve-ci-exclusive-pg17 ')
[[ -n $xrun ]] || fail "no exclusive server was started: $(grep '^docker run' "$log")"
for want in " --network container:improve-ci-pg17 " " -e PGPORT=5433 " \
  " --memory=1536m --memory-swap=1536m " " -e POSTGRES_PASSWORD=postgres " \
  " improve-pg:17 postgres -c shared_preload_libraries=" " --label pg_sage.local-suite.run="; do
  [[ $xrun == *"$want"* ]] || fail "the exclusive server lacks '$want': $xrun"
done
[[ $xrun == *"--tmpfs"* ]] && fail "the exclusive server's data is in memory: $xrun"
[[ $xrun == *" -p "* || $xrun == *"--publish"* ]] && fail "the exclusive server publishes a port"
xexec=$(grep '^docker exec improve-ci-exclusive-pg17 ' "$log")
for want in 'pg_hba.conf' 'pg_reload_conf' 'psql -w -h 127.0.0.1 ' 'pg_stat_statements'; do
  grep -qF -- "$want" <<<"$xexec" || fail "the exclusive server's setup lacks '$want': $xexec"
done
xurl='postgres://postgres:postgres@127.0.0.1:5433/postgres?sslmode=disable'
while IFS= read -r r; do
  [[ $r == *" -e SAGE_TEST_EXCLUSIVE_URL=$xurl "* ]] ||
    fail "a Go test container lacks SAGE_TEST_EXCLUSIVE_URL on port 5433: $r"
done <<<"$gotests"
rm_x=$(grep -n 'docker rm -f -v improve-ci-exclusive-pg17$' "$log" | cut -d: -f1)
rm_s=$(grep -n 'docker rm -f -v improve-ci-pg17$' "$log" | cut -d: -f1)
[[ -n $rm_x && -n $rm_s && $rm_x -lt $rm_s ]] ||
  fail "the exclusive server must be removed before the suite server ($rm_x, $rm_s)"
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
goruns=$(grep '^docker run' "$log" | grep -v '^docker run -d ')
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
xrun=$(grep '^docker run' "$log" | grep -- '--name improve-ci-exclusive-pg17 ')
grep -q improve-go1273-root <<<"$xrun" && fail "the exclusive server mounted the Go volume: $xrun"
[[ $xrun == *" --memory=1536m --memory-swap=1536m "* && $xrun != *"--tmpfs"* ]] ||
  fail "the exclusive server must keep its 1536m cap and on-disk data: $xrun"
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

# agentgov-12: LOCAL_SUITE_PSS_SMALL=1 adds a third server with
# pg_stat_statements.max=100 (SAGE_TEST_PSS_SMALL_URL, as CI's agent-servers
# job), and LOCAL_SUITE_PACKAGES scopes the suite to some packages.
expect 0 'exclusive=improve-ci-exclusive-pg17 .* pss-small=off packages=all$' \
  "dry run: PSS server and package scope off by default" -- $dry bash "$script" "$(newout)" 17
expect 0 'pss-small=improve-ci-pss-pg17 packages=./internal/agentguard/broker/ ./internal/x/...$' \
  "dry run: PSS server and two packages" -- $dry LOCAL_SUITE_PSS_SMALL=1 \
  "LOCAL_SUITE_PACKAGES=./internal/agentguard/broker/ ./internal/x/..." bash "$script" \
  "$(newout)" 17
expect 2 "LOCAL_SUITE_PSS_SMALL 'yes' must be 0 or 1" "bad PSS switch" -- \
  $dry LOCAL_SUITE_PSS_SMALL=yes bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_PACKAGES pattern 'internal/x' must start with ./" "package without ./" -- \
  $dry LOCAL_SUITE_PACKAGES=internal/x bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_PACKAGES pattern './x;rm' must start with ./" "package with a ;" -- \
  $dry "LOCAL_SUITE_PACKAGES=./x;rm -rf ." bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_PACKAGES pattern '\./\*' must start" "a glob is refused, not expanded" \
  -- $dry "LOCAL_SUITE_PACKAGES=./*" bash "$script" "$(newout)"
expect 2 "LOCAL_SUITE_PACKAGES ' ' names no package" "blank package list" -- \
  $dry "LOCAL_SUITE_PACKAGES= " bash "$script" "$(newout)"
pssurl='SAGE_TEST_PSS_SMALL_URL=postgres://postgres:postgres@127.0.0.1:5434/postgres'\
'?sslmode=disable'
# Happy path, full suite: the PSS server joins the suite server's namespace
# on port 5434 with CI's flags and max=100 last, and every Go test gets its URL.
o=$(newout)
expect 0 'summary: .*summary.md' "stubbed full run with the PSS server" -- \
  $guard FAKE_FREE_GB=99 LOCAL_SUITE_PSS_SMALL=1 bash "$script" "$o" 17
prun=$(grep '^docker run' "$log" | grep -- '--name improve-ci-pss-pg17 ')
for want in " --network container:improve-ci-pg17 " " -e PGPORT=5434 " \
  " --memory=1536m --memory-swap=1536m " " -c shared_preload_libraries=" \
  " -c pg_stat_statements.max=100"; do
  [[ $prun == *"$want"* ]] || fail "the PSS server lacks '$want': $prun"
done
[[ $(grep -o 'pg_stat_statements.max=[0-9]*' <<<"$prun" | tail -1) == *=100 ]] ||
  fail "a later flag overrides pg_stat_statements.max=100: $prun"
[[ $prun == *"--tmpfs"* || $prun == *" -p "* ]] && fail "PSS server in memory or published"
grep -q '^docker exec improve-ci-pss-pg17 .*SHOW pg_stat_statements.max' "$log" ||
  fail "the PSS server's setting was not checked"
gotests=$(grep '^docker run' "$log" | grep -- '--name improve-ci-go-pg17 ')
[[ $(grep -c -- " -e $pssurl " <<<"$gotests") -eq 2 ]] ||
  fail "the suite and e2e containers lack $pssurl: $gotests"
grep -q '^go run ./cmd/skipbudget .*-require' "$log" || fail "a full run dropped -require"
rm_p=$(grep -n 'docker rm -f -v improve-ci-pss-pg17$' "$log" | cut -d: -f1)
rm_s=$(grep -n 'docker rm -f -v improve-ci-pg17$' "$log" | cut -d: -f1)
[[ -n $rm_p && -n $rm_s && $rm_p -lt $rm_s ]] || fail "PSS server removal order ($rm_p, $rm_s)"
grep -qx 'pss_small=1 packages=all' "$o/run.txt" || fail "run.txt: $(cat "$o/run.txt")"
# A scoped run without the PSS server: no third server, no PSS URL, no e2e,
# and the skip budget drops the -require lines of packages it did not run.
o=$(newout)
expect 0 'summary: .*summary.md' "stubbed scoped run" -- $guard FAKE_FREE_GB=99 \
  LOCAL_SUITE_PACKAGES=./internal/agentguard/broker/ bash "$script" "$o" 17
gotests=$(grep '^docker run' "$log" | grep -- '--name improve-ci-go-pg17 ')
[[ $(grep -c . <<<"$gotests") -eq 1 ]] || fail "a scoped run wants one Go test: $gotests"
[[ $gotests == *"go test ./internal/agentguard/broker/ -race -cover -tags=integration "* &&
  $gotests != *"./..."* ]] || fail "the suite is not scoped to the broker: $gotests"
grep -q 'pss-pg17\|PSS_SMALL' "$log" && fail "the PSS server started while off"
grep -q '^go run ./cmd/skipbudget .*-stale=off' "$log" && ! grep -q -- '-require' \
  <(grep '^go run ./cmd/skipbudget' "$log") || fail "scoped skip budget: $(grep skipb "$log")"
grep -q '^go run ./cmd/covfloor' "$log" || fail "a scoped run skipped covfloor"
grep -qx 'pss_small=0 packages=./internal/agentguard/broker/' "$o/run.txt" ||
  fail "run.txt: $(cat "$o/run.txt")"
grep -q 'packages ./internal/agentguard/broker/' "$o/summary.md" ||
  fail "the summary does not name the scope: $(head -20 "$o/summary.md")"
# Error propagation: a PSS server that did not take max=100 stops the run.
expect 5 'improve-ci-pss-pg17 runs pg_stat_statements.max=50000, want 100' "PSS max ignored" \
  -- $guard FAKE_FREE_GB=99 STUB_PSS_MAX=50000 LOCAL_SUITE_PSS_SMALL=1 bash "$script" \
  "$(newout)" 17
grep -q -- '--name improve-ci-go-pg17' "$log" && fail "a suite started on a wrong PSS server"
grep -q 'docker rm -f -v improve-ci-pss-pg17$' "$log" || fail "the PSS server was left"

after=$(git -C "$repo" status --porcelain)
[[ $before == "$after" ]] || fail "a run changed the worktree: $before -> $after"

bash "$here/local-linux-suite-summary-test.sh" || failures=$((failures + 1))

if [[ $failures -gt 0 ]]; then
  echo "$failures local suite check(s) failed" >&2
  exit 1
fi
echo "all local suite checks passed"

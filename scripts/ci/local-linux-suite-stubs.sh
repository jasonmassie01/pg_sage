#!/usr/bin/env bash
# Stub docker, go and disk-guard commands for local-linux-suite-test.sh
# (ci-19). Sourced, not run: write_stubs <dir> writes them into <dir>, which
# the test puts first on PATH. Each stub logs every call to $STUB_LOG and
# fails on demand through the STUB_* variables the test sets per case; the
# docker stub keeps its live containers in $STUB_STATE.

# write_stubs <dir>: writes <dir>/docker, <dir>/go and <dir>/guard.sh.
write_stubs() {
local bin=$1
mkdir -p "$bin"
cat >"$bin/docker" <<'EOF'
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
    echo "go version go1.27.4 linux/amd64"
  elif [[ -n ${STUB_GO_SLEEP:-} && $* == *"go test"* ]]; then
    # A Go container that outlives the runner's timeout keeps running.
    echo "$name $label" >>"$STUB_STATE"
    sleep "$STUB_GO_SLEEP"
  elif [[ $2 == -d ]]; then
    echo "$name $label" >>"$STUB_STATE"
  fi ;;
esac
exit 0
EOF
cat >"$bin/go" <<'EOF'
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
cat >"$bin/guard.sh" <<'EOF'
#!/usr/bin/env bash
# Mirrors tools/disk-guard.sh: exit 3 when free space is below the floor.
min=${1:-${PG_SAGE_DISK_GUARD_GB:-40}}
if [ "$FAKE_FREE_GB" -lt "$min" ]; then
  echo "BLOCKED: C: has ${FAKE_FREE_GB} GB free, under the ${min} GB guard" >&2; exit 3
fi
echo "disk ok: C: has ${FAKE_FREE_GB} GB free (guard ${min} GB)"
EOF
chmod +x "$bin/docker" "$bin/go" "$bin/guard.sh"
}

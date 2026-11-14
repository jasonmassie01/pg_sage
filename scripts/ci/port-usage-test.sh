#!/usr/bin/env bash
# Tests port-usage.sh (ci-16) with a stub socket table: what a sample counts
# (dynamic range edges, listening ports, server ports, Windows and Linux
# netstat layouts), per-group peaks and the verdict, start/stop state, and
# the errors for bad input and a failing netstat.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
pu="$here/port-usage.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
fail() { echo "FAIL port-usage: $1" >&2; failures=$((failures + 1)); }
expect() { # expect <name> <want> <got>
  [ "$2" = "$3" ] || fail "$1: want [$2], got [$3]"
}

# The stub prints the table named by the current group; after $STUB_LIMIT
# calls it asks the sampler to stop.
cat >"$work/stub.sh" <<'EOF'
dir=$1
n=$(( $(cat "$dir/calls" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$dir/calls"
group=$(cat "$dir/port-usage.group" 2>/dev/null || echo none)
cat "$dir/table-$group"
[ "$n" -lt "${STUB_LIMIT:-1}" ] || touch "$dir/port-usage.stop"
EOF

windows_table() { # windows_table <time_wait to 54508> <time_wait elsewhere>
  printf '\nActive Connections\n\n  Proto  Local Address          Foreign Address        State\n'
  printf '  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING\n'
  printf '  TCP    127.0.0.1:54508        0.0.0.0:0              LISTENING\n'
  printf '  TCP    127.0.0.1:54508        127.0.0.1:50001        ESTABLISHED\n'
  printf '  TCP    127.0.0.1:50001        127.0.0.1:54508        ESTABLISHED\n'
  local i
  for ((i = 0; i < $1; i++)); do
    printf '  TCP    127.0.0.1:%d        127.0.0.1:54508        TIME_WAIT\n' $((55000 + i))
  done
  for ((i = 0; i < $2; i++)); do
    printf '  TCP    10.0.0.2:%d          93.184.216.34:443      TIME_WAIT\n' $((51000 + i))
  done
}

run_sample() { # run_sample <dir> <limit> [ports...]
  local dir=$1 limit=$2
  shift 2
  STUB_LIMIT=$limit PORT_USAGE_INTERVAL=0 PORT_USAGE_RANGE="49152 16384" \
    PORT_USAGE_NETSTAT="bash $work/stub.sh $dir" bash "$pu" sample "$dir" "$@"
}

# Happy path: one sample of a Windows table. The accepted server socket
# (local 54508, a listening port) is not a dynamic allocation.
d=$work/one
mkdir -p "$d"
windows_table 3 2 >"$d/table-none"
run_sample "$d" 1 54508 || fail "sample exited $?"
row=$(tail -n 1 "$d/port-usage.csv" | cut -d, -f2-)
expect "windows sample" "none,6,5,2,3,1" "$row"
header=time,group,dynamic_in_use,time_wait,established,watched_time_wait,watched_established
expect "csv header" "$header" "$(head -n 1 "$d/port-usage.csv")"
expect "range file" "49152 16384" "$(cat "$d/port-usage.range")"

# No server ports given: the watched columns stay zero.
d=$work/unwatched
mkdir -p "$d"
windows_table 3 2 >"$d/table-none"
run_sample "$d" 1 || fail "unwatched sample exited $?"
expect "unwatched" "none,6,5,2,0,0" "$(tail -n 1 "$d/port-usage.csv" | cut -d, -f2-)"

# Boundary: the first port of the range counts, the port after its end and
# the one before its start do not. Linux layout and IPv6 addresses parse.
d=$work/edges
mkdir -p "$d"
cat >"$d/table-none" <<'EOF'
Proto Recv-Q Send-Q Local Address           Foreign Address         State
tcp        0      0 0.0.0.0:5432            0.0.0.0:*               LISTEN
tcp        0      0 127.0.0.1:1000          127.0.0.1:5432          TIME_WAIT
tcp        0      0 127.0.0.1:1009          127.0.0.1:5432          TIME_WAIT
tcp        0      0 127.0.0.1:1010          127.0.0.1:5432          ESTABLISHED
tcp6       0      0 ::1:999                 ::1:5432                TIME_WAIT
EOF
STUB_LIMIT=1 PORT_USAGE_INTERVAL=0 PORT_USAGE_RANGE="1000 10" \
  PORT_USAGE_NETSTAT="bash $work/stub.sh $d" bash "$pu" sample "$d" 5432 ||
  fail "edges sample exited $?"
expect "range edges" "none,2,3,1,3,1" "$(tail -n 1 "$d/port-usage.csv" | cut -d, -f2-)"

# Groups: samples carry the label current when they were taken; the summary
# gives each group's peak and names the round's peak group.
d=$work/groups
mkdir -p "$d"
windows_table 10 0 >"$d/table-unit-g0"
windows_table 8200 0 >"$d/table-unit-g1"
bash "$pu" group "$d" unit-g0
run_sample "$d" 2 54508 || fail "group g0 exited $?"
bash "$pu" group "$d" unit-g1
rm -f "$d/calls"
run_sample "$d" 2 54508 || fail "group g1 exited $?"
expect "rows appended" 5 "$(wc -l <"$d/port-usage.csv" | tr -d ' ')"
sum=$(bash "$pu" summary "$d") || fail "summary exited $?"
grep -q '^| unit-g0 | 2 | 11 | 0.1% | .* | 10 | 10 | 1 |$' <<<"$sum" ||
  fail "g0 row missing: $sum"
grep -q '^| unit-g1 | 2 | 8201 | 50.1% | .* | 8200 | 8200 | 1 |$' <<<"$sum" ||
  fail "g1 row missing: $sum"
grep -q 'Peak: 8201 of 16384 dynamic ports (50.1%) at .* during group unit-g1.' <<<"$sum" ||
  fail "peak line missing: $sum"
grep -q '^Verdict: near the dynamic range' <<<"$sum" || fail "want near verdict: $sum"
[ -s "$d/port-usage.md" ] || fail "summary not written to port-usage.md"

# Boundary on the verdict: one port under the threshold is far.
d=$work/far
mkdir -p "$d"
windows_table 8190 0 >"$d/table-none"
run_sample "$d" 1 || fail "far sample exited $?"
grep -q '^Verdict: far from the dynamic range' <<<"$(bash "$pu" summary "$d")" ||
  fail "8191 of 16384 (49.99%) must be far"
grep -q '^Verdict: near' <<<"$(PORT_USAGE_NEAR_PCT=40 bash "$pu" summary "$d")" ||
  fail "PORT_USAGE_NEAR_PCT=40 must make 49.99% near"

# Invalid input and empty state.
bash "$pu" bogus "$work/x" 2>/dev/null
expect "unknown command" 2 $?
bash "$pu" summary 2>/dev/null
expect "missing round dir" 2 $?
mkdir -p "$work/empty"
bash "$pu" summary "$work/empty" 2>/dev/null
expect "summary without samples" 4 $?
bash "$pu" stop "$work/empty" 2>/dev/null
expect "stop without sampler" 4 $?

# Error propagation: a failing socket table command stops the sampler with
# exit 5 and a message, and writes no sample row.
d=$work/broken
err=$(PORT_USAGE_INTERVAL=0 PORT_USAGE_RANGE="49152 16384" PORT_USAGE_NETSTAT="false" \
  bash "$pu" sample "$d" 2>&1)
expect "netstat failure" 5 $?
grep -q 'socket table command failed' <<<"$err" || fail "netstat failure message: $err"
expect "no row after failure" 1 "$(wc -l <"$d/port-usage.csv" | tr -d ' ')"

# State: start runs one background sampler, refuses a second, and stop ends it.
d=$work/bg
mkdir -p "$d"
windows_table 1 0 >"$d/table-none"
STUB_LIMIT=1000000 PORT_USAGE_INTERVAL=1 PORT_USAGE_NETSTAT="bash $work/stub.sh $d" \
  bash "$pu" start "$d" 54508 || fail "start exited $?"
pid=$(cat "$d/port-usage.pid")
STUB_LIMIT=1000000 PORT_USAGE_NETSTAT="bash $work/stub.sh $d" bash "$pu" start "$d" 2>/dev/null
expect "second start" 3 $?
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [ -f "$d/port-usage.csv" ] && [ "$(wc -l <"$d/port-usage.csv" | tr -d ' ')" -ge 2 ] && break
  sleep 1
done
bash "$pu" stop "$d" || fail "stop exited $?"
kill -0 "$pid" 2>/dev/null && fail "sampler $pid still runs after stop"
[ -e "$d/port-usage.pid" ] && fail "pid file left after stop"
[ "$(wc -l <"$d/port-usage.csv" | tr -d ' ')" -ge 2 ] || fail "background sampler wrote no row"

# No concurrency checks beyond the single-sampler guard: one sampler owns a
# round directory, and the summary reads a finished file.
if [ "$failures" -ne 0 ]; then
  echo "port-usage-test: $failures failure(s)" >&2
  exit 1
fi
echo "port-usage-test: all checks passed"

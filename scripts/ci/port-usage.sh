#!/usr/bin/env bash
# Samples client TCP port usage during a test round (ci-16), so a round can
# tell ephemeral-port exhaustion from other connection failures.
#
# Windows keeps an actively closed client socket in TIME_WAIT for up to
# 120 s, and every outbound connection takes a port from one dynamic range
# (by default 16,384 ports from 49152). A round that opens connections
# faster than about 136 per second runs out, and dials then fail with
# "Only one usage of each socket address" or "lacked sufficient buffer
# space". The sampler records how close a round comes.
#
# Usage:
#   port-usage.sh start   <round-dir> [server-port...]  sample in the background
#   port-usage.sh group   <round-dir> <name>             label the samples that follow
#   port-usage.sh stop    <round-dir>                    stop the background sampler
#   port-usage.sh sample  <round-dir> [server-port...]  sample in the foreground
#   port-usage.sh summary <round-dir>                    peak per group
#
# Files in <round-dir>: port-usage.csv (one row per sample), port-usage.group
# (the current label), port-usage.range ("<first port> <count>"),
# port-usage.pid, port-usage.stop and port-usage.md (the summary).
#
# A sample counts the sockets whose local port is in the dynamic range and is
# not a listening port (dynamic_in_use, the number exhaustion is about), all
# TIME_WAIT and ESTABLISHED sockets, and those of each to the server ports
# given (the test PostgreSQL servers' published ports).
#
# Environment:
#   PORT_USAGE_INTERVAL  seconds between samples (default 10)
#   PORT_USAGE_NETSTAT   command that prints the socket table
#                        (default "netstat -an -p tcp", or "netstat -ant" off Windows)
#   PORT_USAGE_RANGE     "<first port> <count>" (default: netsh, then
#                        /proc/sys/net/ipv4/ip_local_port_range, then "49152 16384")
#   PORT_USAGE_NEAR_PCT  peak share of the range reported as near exhaustion
#                        (default 50; 10 s samples miss the peaks between them)
set -euo pipefail

usage() {
  sed -n '12,18p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

interval=${PORT_USAGE_INTERVAL:-10}
near_pct=${PORT_USAGE_NEAR_PCT:-50}

netstat_cmd() {
  if [ -n "${PORT_USAGE_NETSTAT:-}" ]; then
    eval "$PORT_USAGE_NETSTAT"
  elif command -v netsh >/dev/null 2>&1; then
    netstat -an -p tcp
  else
    netstat -ant
  fi
}

# dynamic_range: prints "<first port> <count>".
dynamic_range() {
  if [ -n "${PORT_USAGE_RANGE:-}" ]; then
    echo "$PORT_USAGE_RANGE"
    return
  fi
  local first count
  if command -v netsh >/dev/null 2>&1; then
    read -r first count < <(netsh int ipv4 show dynamicport tcp 2>/dev/null |
      awk -F: '/Start Port/ {s=$2} /Number of Ports/ {n=$2}
        END {gsub(/[^0-9]/, "", s); gsub(/[^0-9]/, "", n); print s, n}') || true
  elif [ -r /proc/sys/net/ipv4/ip_local_port_range ]; then
    read -r first last </proc/sys/net/ipv4/ip_local_port_range
    count=$((last - first + 1))
  fi
  if [ -n "${first:-}" ] && [ -n "${count:-}" ]; then
    echo "$first $count"
  else
    echo "49152 16384"
  fi
}

# count_sockets <first> <count> <ports>: one CSV tail from a socket table on
# stdin: dynamic_in_use,time_wait,established,watched_time_wait,watched_established.
count_sockets() {
  awk -v first="$1" -v count="$2" -v ports="$3" '
    function port(addr,   n, parts) { n = split(addr, parts, ":"); return parts[n] + 0 }
    BEGIN {
      n = split(ports, list, ",")
      for (i = 1; i <= n; i++) if (list[i] != "") w[list[i] + 0] = 1
    }
    toupper($1) !~ /^TCP/ || NF < 4 { next }
    { rows[++r] = $0; if ($NF ~ /^LISTEN/) listen[port($(NF - 2))] = 1 }
    END {
      for (i = 1; i <= r; i++) {
        nf = split(rows[i], f, " "); state = f[nf]
        if (state ~ /^LISTEN/) continue
        lp = port(f[nf - 2]); fp = port(f[nf - 1])
        if (lp >= first && lp < first + count && !(lp in listen)) dyn++
        if (state == "TIME_WAIT") { tw++; if (fp in w) wtw++ }
        if (state == "ESTABLISHED") { est++; if (fp in w) west++ }
      }
      printf "%d,%d,%d,%d,%d\n", dyn, tw, est, wtw, west
    }'
}

sample() {
  local dir=$1
  shift
  local ports
  ports=$(IFS=,; echo "$*")
  mkdir -p "$dir"
  local range
  range=$(dynamic_range)
  echo "$range" >"$dir/port-usage.range"
  [ -f "$dir/port-usage.csv" ] ||
    echo "time,group,dynamic_in_use,time_wait,established,watched_time_wait,watched_established" \
      >"$dir/port-usage.csv"
  rm -f "$dir/port-usage.stop"
  while [ ! -e "$dir/port-usage.stop" ]; do
    local group=none counts
    [ -s "$dir/port-usage.group" ] && group=$(tr -d ',\r\n' <"$dir/port-usage.group")
    counts=$(netstat_cmd | count_sockets ${range} "$ports") ||
      { echo "port-usage: socket table command failed" >&2; exit 5; }
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ),$group,$counts" >>"$dir/port-usage.csv"
    local waited=0
    while [ "$waited" -lt "$interval" ] && [ ! -e "$dir/port-usage.stop" ]; do
      sleep 1
      waited=$((waited + 1))
    done
  done
}

start() {
  local dir=$1
  mkdir -p "$dir"
  if [ -s "$dir/port-usage.pid" ] && kill -0 "$(cat "$dir/port-usage.pid")" 2>/dev/null; then
    echo "port-usage: a sampler already runs for $dir (pid $(cat "$dir/port-usage.pid"))" >&2
    exit 3
  fi
  nohup bash "$0" sample "$@" >>"$dir/port-usage.log" 2>&1 &
  echo $! >"$dir/port-usage.pid"
}

stop() {
  local dir=$1 pid tries=0
  [ -s "$dir/port-usage.pid" ] || { echo "port-usage: no sampler pid in $dir" >&2; exit 4; }
  pid=$(cat "$dir/port-usage.pid")
  touch "$dir/port-usage.stop"
  while kill -0 "$pid" 2>/dev/null && [ "$tries" -lt 30 ]; do
    sleep 1
    tries=$((tries + 1))
  done
  kill "$pid" 2>/dev/null || true
  rm -f "$dir/port-usage.pid"
}

summary() {
  local dir=$1 range
  [ -s "$dir/port-usage.csv" ] || { echo "port-usage: no samples in $dir" >&2; exit 4; }
  range=$(cat "$dir/port-usage.range" 2>/dev/null || dynamic_range)
  awk -F, -v range="$range" -v near="$near_pct" -f "$(dirname "$0")/port-usage-summary.awk" \
    "$dir/port-usage.csv" | tee "$dir/port-usage.md"
}

[ $# -ge 2 ] || usage
cmd=$1
shift
case $cmd in
  start) start "$@" ;;
  sample) sample "$@" ;;
  stop) stop "$1" ;;
  group)
    [ $# -eq 2 ] || usage
    mkdir -p "$1"
    printf '%s\n' "$2" >"$1/port-usage.group"
    ;;
  summary) summary "$1" ;;
  *) usage ;;
esac

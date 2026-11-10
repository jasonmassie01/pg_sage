#!/usr/bin/env bash
# ci-05: smoke-test a built pg_sage before it is published. Runs
# CMD [ARGS...] --version and checks that the version line names
# EXPECTED_VERSION exactly and the libpg_query SQL parser (the cgo build).
#
# usage: smoke-version.sh EXPECTED_VERSION CMD [ARGS...]
#   EXPECTED_VERSION is the tag without its v, e.g. 2.6.0 or 2.6.0-rc.1.
# Examples:
#   smoke-version.sh 2.6.0 ./pg_sage
#   smoke-version.sh 2.6.0 docker run --rm ghcr.io/jasonmassie01/pg_sage@sha256:...
set -euo pipefail

usage() {
  echo "usage: $0 EXPECTED_VERSION CMD [ARGS...] (the version without its v, e.g. 2.6.0)" >&2
  exit 2
}

[ "$#" -ge 2 ] || usage
expected="$1"
shift
[ -n "$expected" ] || usage
case "$expected" in
  v*)
    echo "smoke-version: pass the version without its v (got $expected)" >&2
    exit 2
    ;;
esac

status=0
out="$("$@" --version 2>&1)" || status=$?
if [ "$status" -ne 0 ]; then
  printf '%s\n' "$out" >&2
  echo "smoke-version: '$*' --version exited $status" >&2
  exit 1
fi

line="$(grep -m1 '^pg_sage ' <<<"$out" || true)"
if [ -z "$line" ]; then
  echo "smoke-version: no version line (pg_sage <version> (...)) from '$*' --version:" >&2
  printf '%s\n' "$out" >&2
  exit 1
fi
echo "$line"

case "$line" in
  "pg_sage $expected ("*) ;;
  *)
    echo "smoke-version: want version $expected, got: $line" >&2
    exit 1
    ;;
esac
case "$line" in
  *"sql-ast: libpg_query"*) ;;
  *)
    echo "smoke-version: the build lacks sql-ast: libpg_query (cgo): $line" >&2
    exit 1
    ;;
esac

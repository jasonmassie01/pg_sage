#!/usr/bin/env bash
# File-length ratchet (ci-30). CLAUDE.md caps files at 500 lines and
# functions at 50. This checks every git-tracked non-test Go file under
# sidecar/ (skipping _test.go, testdata/ and files whose first line is
# "// Code generated") and every sidecar/web/src *.js and *.jsx file (skipping
# *.test.*).
#
# It fails (exit 1) when a file over 500 lines has no allowlist row, when an
# allowlisted file grows past its listed count, or when the number of
# top-level Go functions over 50 lines exceeds the allowlist's
# functions_over_50 row. Each offender is printed as "path lines limit"; a
# long function as "path:line name lines 50". A PR may shrink a row, never
# add one. Bad usage or an unreadable file exits 2.
#
# Usage: line-limits.sh [ALLOWLIST]   (default: scripts/ci/line-limits.allow)
set -euo pipefail

me=line-limits.sh
usage() {
  echo "usage: $me [ALLOWLIST]  (default: <repo>/scripts/ci/line-limits.allow)" >&2
  echo "$me: $1" >&2
  exit 2
}

[[ $# -le 1 ]] || usage "expected at most one argument, got $#"
top=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "$me: not inside a git repository (run it from the pg_sage checkout)" >&2
  exit 2
}
allow=${1:-$top/scripts/ci/line-limits.allow}
[[ -f $allow && -r $allow ]] || usage "allowlist $allow is missing or unreadable"
cd "$top"

tracked=$(git -c core.quotePath=false ls-files -- \
  'sidecar/*.go' 'sidecar/web/src/*.js' 'sidecar/web/src/*.jsx') || {
  echo "$me: git ls-files failed in $top" >&2
  exit 2
}
list=$(grep -Ev '_test\.go$|(^|/)testdata/|^sidecar/web/src/.*\.test\.[^/]*$' \
  <<<"$tracked" || true)

# The awk program reads the allowlist in BEGIN, one tracked path per input
# record, and judges everything in END.
read -r -d '' program <<'AWK' || true
function fatal(msg) { print me ": " msg > "/dev/stderr"; failed_usage = 1; exit 2 }
function parse_allow(   line, nf, f, rowno) {
  rowno = 0
  while ((r = (getline line < allow)) > 0) {
    rowno++
    sub(/\r$/, "", line)
    sub(/^[ \t]+/, "", line)
    sub(/[ \t]+$/, "", line)
    if (line ~ /^(#|$)/) continue
    nf = split(line, f, /[ \t]+/)
    if (nf != 2 || f[2] !~ /^[0-9]+$/)
      fatal("malformed row " rowno " in " allow ": '" line "' (want 'path count')")
    if (f[1] in limit) fatal("duplicate row " rowno " in " allow ": " f[1])
    limit[f[1]] = f[2] + 0
    if (f[1] != "functions_over_50") rows++
  }
  if (r < 0) fatal("cannot read allowlist " allow)
  close(allow)
  if (!("functions_over_50" in limit))
    fatal("no functions_over_50 row in " allow " (want 'functions_over_50 count')")
}
function name_of(sig,   s) {
  s = sig
  sub(/^func[ \t]*/, "", s)
  if (s ~ /^\(/) sub(/^\([^)]*\)[ \t]*/, "", s)
  if (match(s, /^[A-Za-z_][A-Za-z0-9_]*/)) return substr(s, 1, RLENGTH)
  return "?"
}
# scan counts the lines of path and, for Go, records each top-level function
# over 50 lines. A func line ending in "}" is a one-line function. Only a
# column-0 "}" alone (or before a comment) closes a function: a signature
# line such as "}, error) {" after an inline interface does not.
function scan(path,   line, n, isgo, start, sig, nlong, k, r) {
  isgo = path ~ /\.go$/
  n = 0; start = 0; nlong = 0
  while ((r = (getline line < path)) > 0) {
    n++
    sub(/\r$/, "", line)
    if (n == 1 && isgo && line ~ /^\/\/ Code generated /) { close(path); return -1 }
    if (!isgo) continue
    if (start == 0 && line ~ /^func[ \t(]/) {
      if (line !~ /}[ \t]*$/) { start = n; sig = line }
    } else if (start > 0 && line ~ /^}[ \t]*($|\/\/)/) {
      if (n - start + 1 > 50) {
        nlong++
        pending[nlong] = path ":" start " " name_of(sig) " " (n - start + 1) " 50"
      }
      start = 0
    }
  }
  if (r < 0) fatal("cannot read " path " (tracked by git but missing or unreadable)")
  close(path)
  for (k = 1; k <= nlong; k++) longfn[++nfuncs] = pending[k]
  return n
}
BEGIN {
  allow = ENVIRON["LL_ALLOW"]; rows = 0; nfuncs = 0; checked = 0
  parse_allow()
}
$0 != "" {
  n = scan($0)
  if (n < 0) next
  checked++
  if ($0 in limit) {
    seen[$0] = 1
    if (n > limit[$0]) { print $0 " " n " " limit[$0]; bad++ }
    else if (n <= 500)
      notes[++nnotes] = $0 " " n " " limit[$0] ": now within 500 lines, delete its row"
    else if (n < limit[$0])
      notes[++nnotes] = $0 " " n " " limit[$0] ": lower its row to " n
  } else if (n > 500) { print $0 " " n " 500"; bad++ }
}
END {
  if (failed_usage) exit 2
  for (p in limit)
    if (p != "functions_over_50" && !(p in seen))
      notes[++nnotes] = p " 0 " limit[p] ": no longer tracked, delete its row"
  budget = limit["functions_over_50"]
  if (nfuncs > budget) {
    for (k = 1; k <= nfuncs; k++) print longfn[k]
    bad++
  }
  for (k = 1; k <= nnotes; k++) print "note: " notes[k]
  print checked " files checked, " rows " allowlisted files"
  print "functions_over_50 " nfuncs "/" budget
  if (bad) {
    print me ": FAIL: split the files or functions named above; the allowlist " \
      "may shrink a row, never add one or raise it" > "/dev/stderr"
    exit 1
  }
  if (nfuncs < budget) print "note: functions_over_50 can be lowered to " nfuncs
}
AWK

printf '%s\n' "$list" | LL_ALLOW="$allow" awk -v me="$me" "$program"

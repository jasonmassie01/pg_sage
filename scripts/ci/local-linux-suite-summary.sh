#!/usr/bin/env bash
# Writes <out-dir>/summary.md for a local-linux-suite.sh run (ci-19), in
# the CLAUDE.md report format: totals, failures, data races, per-package
# coverage, UNLISTED skips and packages under their coverage floor, per
# PostgreSQL major.
#
# A failure is labelled image-limited only when its own output (or every
# failing subtest's) shows a missing-extension error for an extension that
# the major's server-check.txt names as missing; otherwise it is real.
# Reads <out-dir>/pg<N>/{suite.json,e2e.json,server-check.txt,
# skipbudget.txt,covfloor.txt,status.txt} and <out-dir>/run.txt. Plain awk: no jq.
#
# Usage: local-linux-suite-summary.sh <out-dir> <majors...>
set -euo pipefail

[ $# -ge 2 ] || { echo "usage: $0 <out-dir> <majors...>" >&2; exit 2; }
out=$1
shift
[ -d "$out" ] || { echo "out-dir $out is not a directory" >&2; exit 2; }

# missing_exts <server-check.txt>: the extensions the server lacks.
missing_exts() {
  local f=$1 exts=
  [ -f "$f" ] || return 0
  grep -q 'CREATE EXTENSION hypopg fails' "$f" && exts+=" hypopg"
  grep -q 'pg_hint_plan is not loaded' "$f" && exts+=" pg_hint_plan"
  grep -q 'pgaudit is not installed' "$f" && exts+=" pgaudit"
  grep -q 'CREATE EXTENSION vector fails' "$f" && exts+=" vector"
  echo "$exts"
}

# scan <exts> <json files...>: one tab-separated record per finding:
# TOTAL pass fail skip | FAIL pkg test label | RACE pkg test n | COV pkg pct
read -r -d '' SCAN <<'AWK' || true
function field(line, key,   re, n) {
  re = "\"" key "\":\"[^\"]*\""
  if (!match(line, re)) return ""
  n = length(key) + 4
  return substr(line, RSTART + n, RLENGTH - n - 1)
}
function output(line,   i) {
  i = index(line, "\"Output\":\"")
  if (i == 0) return ""
  return substr(line, i + 10)
}
# ext_error: the line reports an error and names a missing extension as a
# word of its own (hypopg_create_index and "vector" count; vectorlab and the
# "hypopg_test.go:12:" location prefix of t.Errorf do not).
function ext_error(text,   lower, i) {
  lower = tolower(text)
  if (lower ~ /^ *(=== |--- )/) return 0
  sub(/^ *[a-z0-9_.-]+\.go:[0-9]+: /, "", lower)
  if (lower !~ errre) return 0
  for (i = 1; i <= next_ext; i++)
    if (lower ~ ("(^|[^a-z0-9_])" ext[i] "([^a-z0-9]|$)")) return 1
  return 0
}
BEGIN {
  next_ext = split(exts, ext, " ")
  errre = "(not available|not installed|does not exist|could not open extension|"
  errre = errre "unavailable|unrecognized configuration parameter|no such)"
}
{
  act = field($0, "Action"); pkg = field($0, "Package"); t = field($0, "Test")
  key = pkg "\t" t
  if (act == "output") {
    o = output($0)
    if (t != "" && ext_error(o)) limited[key] = 1
    if (index(o, "WARNING: DATA RACE")) { if (!(key in race)) rorder[++nr] = key; race[key]++ }
    if (t == "" && match(o, /coverage: [0-9.]+% of statements/))
      cov[pkg] = substr(o, RSTART + 10, RLENGTH - 24)
  } else if (t != "" && (act == "pass" || act == "fail" || act == "skip")) {
    count[act]++
    if (act == "fail") { forder[++nf] = key; failpkg[pkg] = 1 }
  } else if (act == "fail" && t == "") pkgfail[pkg] = 1
}
END {
  printf "TOTAL\t%d\t%d\t%d\n", count["pass"], count["fail"], count["skip"]
  for (i = 1; i <= nf; i++) {
    k = forder[i]; lab = (k in limited) ? "image-limited" : ""
    if (lab == "") {
      kids = 0; bad = 0
      for (j = 1; j <= nf; j++) if (index(forder[j], k "/") == 1) {
        kids++; if (!(forder[j] in limited)) bad++
      }
      lab = (kids > 0 && bad == 0) ? "image-limited" : "real"
    }
    printf "FAIL\t%s\t%s\n", k, lab
  }
  for (p in pkgfail) if (!(p in failpkg)) printf "FAIL\t%s\t(package)\treal\n", p
  for (i = 1; i <= nr; i++) printf "RACE\t%s\t%d\n", rorder[i], race[rorder[i]]
  for (p in cov) printf "COV\t%s\t%s\n", p, cov[p]
}
AWK

# major_section <major>: prints the markdown section and adds the totals.
major_section() {
  local m=$1 dir=$out/pg$1 files=() scan
  echo "## PostgreSQL $m ($(sed -n 's/^image=//p' "$dir/status.txt" 2>/dev/null))"
  echo
  [ -f "$dir/suite.json" ] && files+=("$dir/suite.json")
  [ -f "$dir/e2e.json" ] && files+=("$dir/e2e.json")
  if [ ${#files[@]} -eq 0 ]; then
    echo "no suite.json: the run stopped before the suite (see the script's exit message)"
    echo
    return
  fi
  scan=$(awk -v exts="$(missing_exts "$dir/server-check.txt")" "$SCAN" "${files[@]}")
  read -r _ p f s < <(grep '^TOTAL' <<<"$scan" | tr '\t' ' ')
  TOT_P=$((TOT_P + p)); TOT_F=$((TOT_F + f)); TOT_S=$((TOT_S + s))
  echo "**Total:** $p passed, $f failed, $s skipped"
  echo "**Exit statuses:** $(grep -v '^image=' "$dir/status.txt" 2>/dev/null | tr '\n' ' ')"
  echo
  section "Failures" "$(grep '^FAIL' <<<"$scan" | sort -u |
    awk -F'\t' '{printf "- [%s] %s %s\n", $NF, $2, $3}')"
  section "Data races" "$(grep '^RACE' <<<"$scan" |
    awk -F'\t' '{printf "- %s %s (%d report%s)\n", $2, ($3 == "" ? "(package)" : $3), $4,
      ($4 == 1 ? "" : "s")}')"
  section "Coverage" "$(grep '^COV' <<<"$scan" | sort | awk -F'\t' '{printf "- %s: %s\n", $2, $3}')"
  skips "$dir/skipbudget.txt"
  floors "$dir/covfloor.txt" "$m"
  section "What this server cannot cover (server-check.txt)" \
    "$(sed -n 's/^  - /- /p' "$dir/server-check.txt" 2>/dev/null)"
}

section() {
  echo "### $1"
  echo
  if [ -n "$2" ]; then echo "$2"; else echo "none"; fi
  echo
}

skips() {
  if [ ! -f "$1" ]; then
    section "UNLISTED skips" "no skipbudget.txt: the skip budget did not run"
    return
  fi
  section "UNLISTED skips and unmet requirements" \
    "$(head -1 "$1"; grep -E '^ +(UNLISTED|MISSING)' "$1" | sed 's/^ */- /')"
}

# floors <covfloor.txt> <major>: packages under their coverage floor (ci-20;
# the runner gates on PostgreSQL 17 only, as CI's test job does).
floors() {
  if [ -f "$1" ]; then
    section "Coverage floors (cmd/covfloor)" \
      "$(tail -1 "$1"; grep -E '^  (BELOW|FAILED) ' "$1" | sed 's/^  /- /')"
  elif [ "$2" = 17 ]; then
    section "Coverage floors (cmd/covfloor)" "no covfloor.txt: the floors gate did not run"
  fi
}

TOT_P=0 TOT_F=0 TOT_S=0
body=$(for m in "$@"; do major_section "$m"; done; echo "TOTALS $TOT_P $TOT_F $TOT_S")
read -r _ TOT_P TOT_F TOT_S < <(tail -1 <<<"$body")
{
  echo "# Local Linux suite run (ci-19)"
  echo
  [ -f "$out/run.txt" ] && sed 's/^/- /' "$out/run.txt" && echo
  echo "## Test Results"
  echo
  echo "**Command:** \`go test ./... -race -cover -tags=integration -count=1" \
    "-timeout 1500s -json -p 4\` (PostgreSQL 17 also runs the e2e step)"
  echo "**Total:** $TOT_P passed, $TOT_F failed, $TOT_S skipped"
  echo
  sed '$d' <<<"$body"
} >"$out/summary.md"
echo "$out/summary.md"

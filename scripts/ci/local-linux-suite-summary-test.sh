#!/usr/bin/env bash
# Tests local-linux-suite-summary.sh (ci-19) on a fixture go test -json
# stream: totals, failure labels (real or image-limited), data races,
# coverage, UNLISTED skips, and a major whose run stopped early.
# No concurrency or state: the summary reads finished files once.
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
summary="$here/local-linux-suite-summary.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
fail() { echo "FAIL summary: $1" >&2; failures=$((failures + 1)); }

ev() { # ev <action> <package> [test] [output]
  local t=${3:+,\"Test\":\"$3\"} o=${4:+,\"Output\":\"$4\"}
  printf '{"Time":"2026-11-13T00:00:00Z","Action":"%s","Package":"%s"%s%s}\n' \
    "$1" "$2" "$t" "$o"
}
a=example.com/sidecar/internal/a
b=example.com/sidecar/internal/b
fixture() {
  ev run "$a" TestPass
  ev pass "$a" TestPass
  ev output "$a" TestRealFail '    a_test.go:9: boom: want 3, got 2\n'
  ev fail "$a" TestRealFail
  ev output "$a" TestHypo '    a_test.go:12: ERROR: extension \"hypopg\" is not available\n'
  ev fail "$a" TestHypo
  ev output "$a" TestHypoPGUnavailable '=== RUN   TestHypoPGUnavailable\n'
  ev output "$a" TestHypoPGUnavailable '--- FAIL: TestHypoPGUnavailable (0.00s)\n'
  ev output "$a" TestHypoPGUnavailable '    a_test.go:20: unexpected count 3\n'
  ev fail "$a" TestHypoPGUnavailable
  ev output "$a" TestParent/hint '    a_test.go:30: unrecognized configuration parameter'\
' \"pg_hint_plan.enable_hint\"\n'
  ev fail "$a" TestParent/hint
  ev fail "$a" TestParent
  ev output "$a" TestRace 'WARNING: DATA RACE\n'
  ev output "$a" TestRace 'WARNING: DATA RACE\n'
  ev output "$a" TestRace '    testing.go:1490: race detected during execution of test\n'
  ev fail "$a" TestRace
  ev skip "$a" TestSkipped
  ev output "$a" '' 'coverage: 71.2% of statements\n'
  ev fail "$a"
  ev output "$b" '' 'b.go:3:2: undefined: nope\n'
  ev fail "$b"
}

out=$work/out
mkdir -p "$out/pg17" "$out/pg14"
fixture >"$out/pg17/suite.json"
ev pass example.com/sidecar/e2e TestE2E >"$out/pg17/e2e.json"
cat >"$out/pg17/server-check.txt" <<'EOF'
PostgreSQL in improve-ci-pg17 is not CI's test server (postgresql.ci.conf):
  - CREATE EXTENSION hypopg fails (HypoPG is not installed)
  - pg_hint_plan is not loaded (pg_hint_plan.enable_hint is '')
  - pgaudit is not installed
EOF
cat >"$out/pg17/skipbudget.txt" <<'EOF'
skip budget: 1 skipped, 0 allowed, 1 unlisted, 0 stale rules, 0 unmet requirements
  UNLISTED example.com/sidecar/internal/a TestSkipped: needs pgvector
  STALE    line 17: internal/azure TestAzureLive (matched no skip)
  MISSING  internal/notify TestPhase2_.*: no matching test passed
EOF
cat >"$out/pg17/covfloor.txt" <<'EOF'
covfloor: checked 3 packages against .coverage-floors (default floor 70%)
  BELOW     internal/a  40.0% < 70% (default)
  FAILED    internal/b  build failed
  EXEMPT    e2e  no tests (line 9: runs in its own step)
covfloor: 2 packages under their floor
EOF
printf 'image=improve-pg:17\nserver_check=1\nsuite=1\ne2e=0\nskipbudget=1\ncovfloor=1\n' \
  >"$out/pg17/status.txt"
fixture >"$out/pg14/suite.json"
echo '  - pgaudit is not installed' >"$out/pg14/server-check.txt"
printf 'image=improve-pg:14\nsuite=1\n' >"$out/pg14/status.txt"

bash "$summary" "$out" 17 14 15 >/dev/null 2>"$work/err" || fail "exit $?: $(cat "$work/err")"
s=$(awk '/^## PostgreSQL 17/{f=1} /^## PostgreSQL 14/{f=0} f' "$out/summary.md")
s14=$(awk '/^## PostgreSQL 14/{f=1} /^## PostgreSQL 15/{f=0} f' "$out/summary.md")
s15=$(awk '/^## PostgreSQL 15/{f=1} f' "$out/summary.md")

has() { # has <text> <fixed-string> <what>
  grep -qF -- "$2" <<<"$1" || fail "$3: '$2' missing from:\n$1"
}
lacks() { grep -qF -- "$2" <<<"$1" && fail "$3: '$2' present in:\n$1"; }

has "$s" '**Total:** 2 passed, 6 failed, 1 skipped' "pg17 totals (suite and e2e)"
has "$s" "- [real] $a TestRealFail" "plain failure"
has "$s" "- [image-limited] $a TestHypo" "missing HypoPG"
has "$s" "- [real] $a TestHypoPGUnavailable" "a test name alone is no extension error"
has "$s" "- [image-limited] $a TestParent/hint" "missing pg_hint_plan"
has "$s" "- [image-limited] $a TestParent" "parent of image-limited subtests"
has "$s" "- [real] $a TestRace" "race failure"
has "$s" "- [real] $b (package)" "package-level failure"
lacks "$s" "$a (package)" "a package with failing tests is not listed again"
has "$s" "- $a TestRace (2 reports)" "data race"
has "$s" "- $a: 71.2%" "coverage"
has "$s" "UNLISTED example.com/sidecar/internal/a TestSkipped: needs pgvector" "unlisted skip"
has "$s" "- MISSING  internal/notify TestPhase2_.*: no matching test passed" "unmet requirement"
lacks "$s" "STALE    line 17" "stale allow-list rules are a warning, not a finding"
has "$s" "CREATE EXTENSION hypopg fails" "server check carried into the summary"
has "$s" "suite=1" "exit statuses carried into the summary"
has "$s14" "- [real] $a TestHypo" "pg14 lists no missing HypoPG, so the failure is real"
has "$s14" "no skipbudget.txt" "missing skip budget output is named"
has "$s" "- BELOW     internal/a  40.0% < 70% (default)" "package under its floor (ci-20)"
has "$s" "- FAILED    internal/b  build failed" "build failure in the floors gate"
has "$s" "covfloor: 2 packages under their floor" "floors gate verdict"
lacks "$s" "EXEMPT" "exempt packages are not findings"
has "$s" "covfloor=1" "floors gate status carried into the summary"
lacks "$s14" "Coverage floors" "the floors gate runs on PostgreSQL 17 only"
has "$s15" "no suite.json: the run stopped before the suite" "major that never ran"
has "$(cat "$out/summary.md")" "**Total:** 3 passed, 12 failed, 2 skipped" "overall totals"

# Invalid input to the label heuristic: an extension name inside a longer
# word or in t.Errorf's file:line prefix is no missing-extension error.
o2=$work/out2
mkdir -p "$o2/pg16"
{
  ev output "$a" TestFileName '    hypopg_test.go:40: relation \"sage.findings\" does not exist\n'
  ev fail "$a" TestFileName
  ev output "$a" TestLongerWord '    lab_test.go:5: vectorlab table does not exist\n'
  ev fail "$a" TestLongerWord
  ev output "$a" TestHypoFunc '    x_test.go:7: ERROR: function hypopg_create_index(unknown)'\
' does not exist (SQLSTATE 42883)\n'
  ev fail "$a" TestHypoFunc
  ev output "$a" TestVectorType '    v_test.go:3: ERROR: type \"vector\" does not exist\n'
  ev fail "$a" TestVectorType
} >"$o2/pg16/suite.json"
printf '  - CREATE EXTENSION hypopg fails\n  - CREATE EXTENSION vector fails\n' \
  >"$o2/pg16/server-check.txt"
echo 'image=improve-pg:16' >"$o2/pg16/status.txt"
bash "$summary" "$o2" 16 >/dev/null 2>"$work/err" || fail "exit $?: $(cat "$work/err")"
s16=$(cat "$o2/summary.md")
has "$s16" "- [real] $a TestFileName" "an extension in the file:line prefix"
has "$s16" "- [real] $a TestLongerWord" "an extension inside a longer word"
has "$s16" "- [image-limited] $a TestHypoFunc" "a missing HypoPG function"
has "$s16" "- [image-limited] $a TestVectorType" "a missing pgvector type"

bash "$summary" >/dev/null 2>"$work/err"
[[ $? -eq 2 ]] && grep -q usage "$work/err" || fail "no arguments: want exit 2 and usage"
bash "$summary" "$work/none" 17 >/dev/null 2>"$work/err"
[[ $? -eq 2 ]] && grep -q 'is not a directory' "$work/err" || fail "missing out-dir: want exit 2"

if [[ $failures -gt 0 ]]; then
  echo "$failures summary check(s) failed" >&2
  exit 1
fi
echo "ok   summary checks"

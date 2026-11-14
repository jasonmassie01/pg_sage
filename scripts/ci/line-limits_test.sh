#!/usr/bin/env bash
# Tests line-limits.sh (ci-30). Every case builds a throwaway git repository
# under mktemp -d, stages files of known sizes and runs the script inside it.
# Concurrency and state transitions do not apply: the script is a single
# process that reads the index and the working tree and keeps no state.
#
# Usage: line-limits_test.sh
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/line-limits.sh"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
failures=0
cases=0
repo=""

# new_repo: an empty git repository with an allowlist budget of
# <functions_over_50> (default 0) and no file rows.
new_repo() {
  cases=$((cases + 1))
  repo="$scratch/repo$cases"
  mkdir -p "$repo/scripts/ci"
  git init -q "$repo"
  allow "functions_over_50 ${1:-0}"
}

# allow <rows...>: writes the repository's allowlist, one row per argument.
allow() {
  printf '# test allowlist\n' >"$repo/scripts/ci/line-limits.allow"
  printf '%s\n' "$@" >>"$repo/scripts/ci/line-limits.allow"
}

# lines <path> <n>: a file of exactly n lines (a Go file starts with package).
lines() {
  local path="$repo/$1" n=$2 i
  mkdir -p "$(dirname "$path")"
  {
    [[ $path == *.go ]] && { echo "package x"; n=$((n - 1)); }
    for ((i = 0; i < n; i++)); do echo "// line $i"; done
  } >"$path"
}

# func_lines <n>: a top-level Go function of exactly n lines (n >= 2).
func_lines() {
  local n=$1 i
  echo "func f$n() {"
  for ((i = 0; i < n - 2; i++)); do printf '\t_ = %d\n' "$i"; done
  echo "}"
}

stage() { git -C "$repo" -c core.autocrlf=false add -A; }

# expect <want-status> <output-pattern> <description> -- <command...>
# Runs the command inside $repo; output is stdout and stderr together.
expect() {
  local want=$1 pattern=$2 name=$3
  shift 4
  local out status
  out=$(cd "$repo" && "$@" 2>&1)
  status=$?
  if [[ $status -ne $want ]]; then
    echo "FAIL $name: exit $status, want $want; output: $out" >&2
    failures=$((failures + 1))
  elif [[ -n $pattern ]] && ! grep -Eq -- "$pattern" <<<"$out"; then
    echo "FAIL $name: output does not match /$pattern/: $out" >&2
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

# refute <pattern> <description> -- <command...>: output must not match.
refute() {
  local pattern=$1 name=$2
  shift 3
  local out
  out=$(cd "$repo" && "$@" 2>&1)
  if grep -Eq -- "$pattern" <<<"$out"; then
    echo "FAIL $name: output matches /$pattern/: $out" >&2
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

# Happy path.
new_repo
lines sidecar/internal/a/a.go 499
lines sidecar/web/src/pages/Page.jsx 500
stage
expect 0 '^2 files checked' "499-line Go file and 500-line JSX file pass" -- bash "$script"
refute 'a\.go|Page\.jsx' "files at or under the limit are not named" -- bash "$script"

new_repo
lines sidecar/internal/big/big.go 600
allow "sidecar/internal/big/big.go 600" "functions_over_50 0"
stage
expect 0 '1 allowlisted file' "allowlisted 600-line file at its listed count passes" -- \
  bash "$script"

new_repo
lines sidecar/internal/big/big.go 550
allow "sidecar/internal/big/big.go 600" "functions_over_50 0"
stage
expect 0 'note: sidecar/internal/big/big.go 550 600' \
  "allowlisted file below its count passes with a note to lower the row" -- bash "$script"

new_repo
lines sidecar/internal/big/big.go 120
allow "sidecar/internal/big/big.go 600" "functions_over_50 0"
stage
expect 0 'note: .*big\.go.* delete' "row for a file now under the limit is a note" -- \
  bash "$script"

# Boundary: line counts.
new_repo
lines sidecar/internal/x/x.go 501
stage
expect 1 '^sidecar/internal/x/x\.go 501 500$' "501-line file fails and is named" -- \
  bash "$script"

new_repo
lines sidecar/web/src/pages/Big.js 501
stage
expect 1 '^sidecar/web/src/pages/Big\.js 501 500$' "501-line JS file fails" -- bash "$script"

new_repo
lines sidecar/internal/big/big.go 601
allow "sidecar/internal/big/big.go 600" "functions_over_50 0"
stage
expect 1 '^sidecar/internal/big/big\.go 601 600$' "allowlisted file at count + 1 fails" -- \
  bash "$script"

new_repo
mkdir -p "$repo/sidecar/internal/n"
{ echo "package n"; for ((i = 0; i < 499; i++)); do echo "// $i"; done; printf '// last'; } \
  >"$repo/sidecar/internal/n/n.go"
stage
expect 1 '^sidecar/internal/n/n\.go 501 500$' \
  "a last line without a newline still counts" -- bash "$script"

# Boundary: function lengths.
new_repo
mkdir -p "$repo/sidecar/f"
{ echo "package f"; func_lines 50; } >"$repo/sidecar/f/f.go"
stage
expect 0 'functions_over_50 0/0' "a 50-line function is not counted" -- bash "$script"

new_repo
mkdir -p "$repo/sidecar/f"
{ echo "package f"; func_lines 51; } >"$repo/sidecar/f/f.go"
stage
expect 1 'functions_over_50 1/0' "a 51-line function raises the count by one" -- \
  bash "$script"
expect 1 '^sidecar/f/f\.go:2 f51 51 50$' "the over-budget run names each long function" -- \
  bash "$script"
allow "functions_over_50 1"
expect 0 'functions_over_50 1/1' "a 51-line function within the budget passes" -- \
  bash "$script"

new_repo
mkdir -p "$repo/sidecar/f"
{
  echo "package f"
  echo "func one() { x() }"
  for ((i = 0; i < 49; i++)); do echo "// filler $i"; done
  func_lines 3
} >"$repo/sidecar/f/f.go"
stage
expect 0 'functions_over_50 0/0' "a one-line func is not paired with the next brace" -- \
  bash "$script"

new_repo
mkdir -p "$repo/sidecar/f"
{
  echo "package f"
  echo "func (r *recv) method("
  echo "	a int,"
  echo ") error {"
  for ((i = 0; i < 48; i++)); do printf '\t_ = %d\n' "$i"; done
  echo "}"
} >"$repo/sidecar/f/f.go"
stage
expect 1 'f\.go:2 method 52 50' "a method with a multi-line signature counts all lines" -- \
  bash "$script"

new_repo
mkdir -p "$repo/sidecar/f"
{
  echo "package f"
  echo "func (r *recv) collect(rows interface {"
  echo "	Next() bool"
  echo "}, n int) error {"
  for ((i = 0; i < 48; i++)); do printf '\t_ = %d\n' "$i"; done
  echo "} // collect"
} >"$repo/sidecar/f/f.go"
stage
expect 1 'f\.go:2 collect 52 50' \
  "an inline interface in the signature does not close the function early" -- bash "$script"

new_repo
mkdir -p "$repo/sidecar/f"
{ echo "package f"; func_lines 51; } | sed 's/$/\r/' >"$repo/sidecar/f/f.go"
stage
expect 1 'functions_over_50 1/0' "CRLF line endings still close a function" -- \
  bash "$script"

# Invalid input.
new_repo
rm "$repo/scripts/ci/line-limits.allow"
expect 2 'usage: .*line-limits\.sh' "missing allowlist exits 2 with usage" -- bash "$script"
expect 2 'usage: ' "two arguments exit 2 with usage" -- bash "$script" a b

new_repo
allow "sidecar/internal/big/big.go" "functions_over_50 0"
expect 2 'malformed row 2.*big\.go' "row without a count exits 2" -- bash "$script"
allow "sidecar/internal/big/big.go 6x0" "functions_over_50 0"
expect 2 'malformed row' "row with a non-numeric count exits 2" -- bash "$script"
allow "sidecar/internal/big/big.go 600 extra" "functions_over_50 0"
expect 2 'malformed row' "row with three fields exits 2" -- bash "$script"
allow "a.go 600" "a.go 700" "functions_over_50 0"
expect 2 'duplicate row.*a\.go' "duplicate row exits 2" -- bash "$script"
allow "a.go 600"
expect 2 'no functions_over_50 row' "allowlist without the function budget exits 2" -- \
  bash "$script"
allow "functions_over_50 0" "functions_over_50 3"
expect 2 'duplicate row.*functions_over_50' "two function budgets exit 2" -- bash "$script"

new_repo
lines sidecar/internal/big/big.go 600
printf 'functions_over_50 0\nsidecar/internal/big/big.go 600\n' >"$scratch/other.allow"
stage
expect 0 '1 allowlisted file' "an allowlist path argument is used" -- \
  bash "$script" "$scratch/other.allow"
mkdir -p "$repo/sidecar/internal"
cp "$scratch/other.allow" "$repo/sidecar/internal/rel.allow"
expect 0 '1 allowlisted file' "a relative allowlist path works from a subdirectory" -- \
  bash -c 'cd sidecar/internal && bash "$1" rel.allow' _ "$script"

cases=$((cases + 1))
repo="$scratch/not-a-repo"
mkdir -p "$repo"
printf 'functions_over_50 0\n' >"$repo/x.allow"
expect 2 'not inside a git repository' "outside a git repository exits 2" -- \
  env GIT_CEILING_DIRECTORIES="$scratch" bash "$script" x.allow

# Nil and empty.
new_repo
expect 0 '^0 files checked' "empty repository passes with 0 files checked" -- bash "$script"

# Excluded files.
new_repo
lines sidecar/internal/a/a.go 10
lines sidecar/internal/a/a_test.go 900
lines sidecar/internal/a/testdata/big.go 900
mkdir -p "$repo/sidecar/internal/gen"
{
  echo "// Code generated by gen. DO NOT EDIT."
  for ((i = 0; i < 900; i++)); do echo "//"; done
} >"$repo/sidecar/internal/gen/gen.go"
lines sidecar/web/src/pages/Page.test.jsx 900
lines sidecar/web/e2e/flow.js 900
lines sidecar/web/src/styles.css 900
lines tools/other/big.go 900
lines sidecar/internal/a/untracked.go 900
stage
git -C "$repo" rm -q --cached sidecar/internal/a/untracked.go
expect 0 '^1 files checked' \
  "_test.go, testdata/, generated, *.test.*, non-src, non-sidecar and untracked are skipped" \
  -- bash "$script"

new_repo
mkdir -p "$repo/sidecar/internal/gen"
{
  echo "package gen"
  echo "// Code generated by gen. DO NOT EDIT."
  for ((i = 0; i < 599; i++)); do echo "//"; done
} >"$repo/sidecar/internal/gen/gen.go"
stage
expect 1 '^sidecar/internal/gen/gen\.go 601 500$' \
  "a generated marker below line 1 does not exempt the file" -- bash "$script"

# Error propagation.
new_repo
lines sidecar/internal/gone/gone.go 10
stage
rm "$repo/sidecar/internal/gone/gone.go"
expect 2 'cannot read sidecar/internal/gone/gone\.go' \
  "a tracked file missing from the working tree fails with its path" -- bash "$script"

new_repo
lines sidecar/internal/locked/locked.go 10
stage
chmod 000 "$repo/sidecar/internal/locked/locked.go"
if [[ -r $repo/sidecar/internal/locked/locked.go ]]; then
  echo "ok   unreadable file (chmod 000 does not apply here; covered by the missing file)"
else
  expect 2 'cannot read sidecar/internal/locked/locked\.go' \
    "an unreadable file fails with its path" -- bash "$script"
fi
chmod 644 "$repo/sidecar/internal/locked/locked.go"

# Integration: the real tree at HEAD stays green.
cases=$((cases + 1))
repo=$(git -C "$here" rev-parse --show-toplevel)
expect 0 'functions_over_50 [0-9]+/[0-9]+' "the repository's own tree passes" -- bash "$script"

if [[ $failures -ne 0 ]]; then
  echo "$failures line-limits case(s) failed" >&2
  exit 1
fi
echo "all line-limits cases passed"

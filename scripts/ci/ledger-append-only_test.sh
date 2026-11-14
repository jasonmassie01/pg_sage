#!/usr/bin/env bash
# Tests ledger-append-only.sh (ci-30). Every case builds a throwaway git
# repository under mktemp -d, tags a baseline ledger and edits the working
# copy. Concurrency and state transitions do not apply: the script is a
# single process that compares two files and keeps no state.
#
# Usage: ledger-append-only_test.sh
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/ledger-append-only.sh"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
ledger=sidecar/internal/schema/testdata/migration_checksums.txt
failures=0
cases=0
repo=""

g() {
  git -C "$repo" -c core.autocrlf=false -c user.name=ci-test \
    -c user.email=ci-test@example.invalid -c commit.gpgsign=false -c tag.gpgsign=false "$@"
}

# baseline_lines <n>: a header comment and n-1 step rows.
baseline_lines() {
  local n=$1 i
  echo "# ledger header"
  for ((i = 1; i < n; i++)); do printf 'step_%02d %064d\n' "$i" "$i"; done
}

# new_repo [<n>]: a repository tagged v1.0.0. With n, the tag holds a ledger
# of n lines and the working copy starts identical to it; without, the tag
# has no ledger.
new_repo() {
  cases=$((cases + 1))
  repo="$scratch/repo$cases"
  git init -q "$repo"
  mkdir -p "$repo/$(dirname "$ledger")"
  echo "readme" >"$repo/README"
  if [[ $# -gt 0 ]]; then baseline_lines "$1" >"$repo/$ledger"; fi
  g add -A && g commit -q -m base && g tag v1.0.0
}

# expect <want-status> <output-pattern> <description> -- <command...>
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

# Happy path.
new_repo 12
expect 0 '12 baseline lines unchanged' "unchanged ledger passes" -- bash "$script" v1.0.0

new_repo 12
printf 'step_12 %064d\nstep_13 %064d\n' 12 13 >>"$repo/$ledger"
expect 0 '12 baseline lines unchanged, 2 appended' "appended lines pass" -- \
  bash "$script" v1.0.0

new_repo 12
sed -i 's/$/\r/' "$repo/$ledger"
expect 0 'unchanged' "a CRLF working copy of the same lines passes" -- bash "$script" v1.0.0

new_repo 12
git -C "$repo" rev-parse HEAD >"$scratch/sha"
expect 0 'unchanged' "a commit SHA works as the base ref" -- bash "$script" "$(cat "$scratch/sha")"

# Nil and empty.
new_repo
mkdir -p "$repo/$(dirname "$ledger")"
baseline_lines 5 >"$repo/$ledger"
expect 0 'no baseline' "ledger absent at the base ref passes with no baseline" -- \
  bash "$script" v1.0.0

new_repo 1
: >"$repo/$ledger"
expect 1 'line 1 deleted' "emptying the ledger fails" -- bash "$script" v1.0.0

new_repo 1
: >"$repo/$ledger"
g add -A && g commit -q -m "empty ledger" && g tag v1.1.0
printf 'step_01 %064d\nstep_02 %064d\n' 1 2 >"$repo/$ledger"
expect 0 '0 baseline lines unchanged, 2 appended since v1\.1\.0' \
  "an empty ledger at the base ref accepts any appended lines" -- bash "$script" v1.1.0

# Ledger edits.
new_repo 12
sed -i '10s/9$/7/' "$repo/$ledger"
expect 1 'line 10 changed' "a changed checksum on line 10 fails and names line 10" -- \
  bash "$script" v1.0.0
expect 1 'base: +step_09 0+9$' "the failure shows the baseline line" -- bash "$script" v1.0.0
expect 1 "line 10 changed in $ledger since v1\\.0\\.0" \
  "the failure names the ledger path and the base ref" -- bash "$script" v1.0.0
expect 1 'now: +step_09 0+7$' "the failure shows the edited line" -- bash "$script" v1.0.0

new_repo 12
sed -i '5d' "$repo/$ledger"
expect 1 'line 5 changed' "a deleted middle line fails" -- bash "$script" v1.0.0

new_repo 12
sed -i '$d' "$repo/$ledger"
expect 1 'line 12 deleted' "a deleted last line fails" -- bash "$script" v1.0.0
expect 1 "has 11 lines, v1\.0\.0 had 12" "a deleted last line names both lengths" -- \
  bash "$script" v1.0.0

new_repo 12
sed -i '3{h;d};4{G}' "$repo/$ledger"
expect 1 'line 3 changed' "two swapped lines fail" -- bash "$script" v1.0.0

new_repo 12
sed -i '1i # a new header line' "$repo/$ledger"
expect 1 'line 1 changed' "a line inserted before the baseline fails" -- \
  bash "$script" v1.0.0

new_repo 12
sed -i '1s/$/ /' "$repo/$ledger"
expect 1 'line 1 changed' "trailing whitespace on line 1 fails" -- bash "$script" v1.0.0

# Invalid input.
new_repo 12
expect 2 'usage: .*ledger-append-only\.sh BASE_REF' "no argument exits 2 with usage" -- \
  bash "$script"
expect 2 'usage: ' "two arguments exit 2 with usage" -- bash "$script" v1.0.0 extra
expect 2 'usage: ' "an empty base ref exits 2 with usage" -- bash "$script" ""
expect 2 "base ref 'v9\\.9\\.9' does not exist" "a non-existent base ref exits 2 and names it" \
  -- bash "$script" v9.9.9
expect 2 "base ref '--all' does not exist" "a ref that looks like an option is refused" \
  -- bash "$script" --all

cases=$((cases + 1))
repo="$scratch/not-a-repo"
mkdir -p "$repo"
expect 2 'not inside a git repository' "outside a git repository exits 2" -- \
  env GIT_CEILING_DIRECTORIES="$scratch" bash "$script" v1.0.0

# Error propagation.
new_repo 12
rm "$repo/$ledger"
expect 1 "cannot read $ledger" "ledger removed from the working tree fails with its path" -- \
  bash "$script" v1.0.0

new_repo 12
chmod 000 "$repo/$ledger"
if [[ -r $repo/$ledger ]]; then
  echo "ok   unreadable ledger (chmod 000 does not apply here; covered by the removed file)"
else
  expect 1 "cannot read $ledger" "an unreadable ledger fails with its path" -- \
    bash "$script" v1.0.0
fi
chmod 644 "$repo/$ledger"

if [[ $failures -ne 0 ]]; then
  echo "$failures ledger-append-only case(s) failed" >&2
  exit 1
fi
echo "all ledger-append-only cases passed"

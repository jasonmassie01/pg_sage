#!/usr/bin/env bash
# Tests release-notes.sh (ci-05; item release-notes). Every case writes a
# throwaway CHANGELOG under mktemp -d; the last cases read the repository's
# own CHANGELOG.md (or the file given as $1). Concurrency and state
# transitions do not apply: the script is one process that reads one file
# and keeps no state.
#
# Usage: release-notes_test.sh [CHANGELOG]
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/release-notes.sh"
repo_changelog="${1:-$here/../../CHANGELOG.md}"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
failures=0
cases=0
file=""

# new_changelog: the next scratch CHANGELOG path, filled from stdin.
new_changelog() {
  cases=$((cases + 1))
  file="$scratch/CHANGELOG$cases.md"
  cat >"$file"
}

# expect <want-status> <output-pattern> <description> -- <command...>
# The pattern is an extended regex matched against stdout and stderr.
expect() {
  local want=$1 pattern=$2 name=$3
  shift 4
  local out status
  out=$("$@" 2>&1)
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

# expect_stdout <want-file> <description> -- <command...>: exit 0 and stdout
# equal to the file, byte for byte.
expect_stdout() {
  local want=$1 name=$2
  shift 3
  local got="$scratch/got$cases"
  if ! "$@" >"$got" 2>"$got.err"; then
    echo "FAIL $name: exit non-zero; stderr: $(cat "$got.err")" >&2
    failures=$((failures + 1))
  elif ! cmp -s "$want" "$got"; then
    echo "FAIL $name: stdout differs:" >&2
    diff "$want" "$got" >&2
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

# The shape of the real CHANGELOG: intro, per-release subsections of
# [Unreleased], each starting with its upgrade notes, then released sections.
unreleased_shape() {
  cat <<'EOF'
# Changelog

## [Unreleased]

These changes are planned as two releases: v2.5.1 and v2.6.0.

### v2.5.1 (tranche T0a, safety)

#### Upgrade notes

- Close old PagerDuty pages once (alerting-01).

#### Fixed

- **Pages.** A resolved incident closes its page (alerting-01).

### v2.6.0 (tranche T1)

#### Upgrade notes

- Back up data_dir/encryption.key (security-08).

## v2.5.0 (2026-11-03) -- The Databricks release

### Upgrade notes

- Old note.
EOF
}

# Happy path.
new_changelog < <(unreleased_shape)
cat >"$scratch/want-rc" <<'EOF'
These changes are planned as two releases: v2.5.1 and v2.6.0.

### v2.5.1 (tranche T0a, safety)

#### Upgrade notes

- Close old PagerDuty pages once (alerting-01).

#### Fixed

- **Pages.** A resolved incident closes its page (alerting-01).

### v2.6.0 (tranche T1)

#### Upgrade notes

- Back up data_dir/encryption.key (security-08).
EOF
expect_stdout "$scratch/want-rc" \
  "a release candidate falls back to [Unreleased]: intro, then v2.5.1, then its upgrade notes" \
  -- bash "$script" v2.5.1-rc.1 "$file"

cat >"$scratch/want-stable" <<'EOF'
### Upgrade notes

- Old note.
EOF
expect_stdout "$scratch/want-stable" \
  "a stable tag prints its own section without the heading or the blank edges" \
  -- bash "$script" v2.5.0 "$file"

new_changelog <<'EOF'
## [Unreleased]

- Unreleased line.

## v2.6.0 (2026-12-01)

- Release line.

## v2.5.0

- Older line.
EOF
printf '%s\n' '- Release line.' >"$scratch/want-own"
expect_stdout "$scratch/want-own" \
  "a release candidate uses its release's own section before [Unreleased]" \
  -- bash "$script" v2.6.0-rc.2 "$file"

sed 's/$/\r/' "$file" >"$scratch/crlf.md"
expect_stdout "$scratch/want-own" "a CRLF CHANGELOG gives the same notes" \
  -- bash "$script" v2.6.0 "$scratch/crlf.md"

# Boundary conditions.
new_changelog < <(unreleased_shape)
expect 1 'has no "## v2\.5\.1" section; add it before tagging v2\.5\.1' \
  "a stable tag fails while its release is only a ### subsection of [Unreleased]" \
  -- bash "$script" v2.5.1 "$file"

new_changelog <<'EOF'
## v2.6.10 (2026-12-01)

- Not this release.
EOF
expect 1 'has no "## v2\.6\.1" section' \
  "a heading for v2.6.10 does not count as v2.6.1" -- bash "$script" v2.6.1 "$file"

new_changelog <<'EOF'
## v2.6.1
- Heading without a date or a blank line.
## v2.6.0
EOF
expect 0 '^- Heading without a date or a blank line\.$' \
  "a bare heading counts and the section ends at the next ## heading" \
  -- bash "$script" v2.6.1 "$file"

# Invalid input.
new_changelog < <(unreleased_shape)
expect 2 '^usage: ' "a tag without the v prefix exits 2" -- bash "$script" 2.5.1 "$file"
expect 2 '^usage: ' "a tag that is not a version exits 2" -- bash "$script" vnext "$file"
expect 2 '^usage: ' "one argument exits 2" -- bash "$script" v2.5.1
expect 2 '^usage: ' "three arguments exit 2" -- bash "$script" v2.5.1 "$file" extra
expect 1 "release-notes: cannot read $scratch/missing\.md" \
  "a missing CHANGELOG exits 1 naming the file" \
  -- bash "$script" v2.5.0 "$scratch/missing.md"
mkdir "$scratch/dir.md"
expect 1 "release-notes: cannot read $scratch/dir\.md" \
  "a directory given as the CHANGELOG is refused as unreadable, not as missing a section" \
  -- bash "$script" v2.5.0 "$scratch/dir.md"
unreadable="$scratch/unreadable.md"
unreleased_shape >"$unreadable"
chmod 000 "$unreadable"
if [[ -r $unreadable ]]; then
  echo "ok   unreadable CHANGELOG (chmod 000 does not apply here; covered by the missing file)"
else
  expect 1 "cannot read $unreadable" "an unreadable CHANGELOG exits 1" \
    -- bash "$script" v2.5.0 "$unreadable"
fi
chmod 644 "$unreadable"

# Nil and empty.
new_changelog <<'EOF'
# Changelog

## [Unreleased]



## v2.5.0

- Older line.
EOF
tried='tried ## v2\.5\.1-rc\.1 ## v2\.5\.1 ## \[Unreleased\]'
expect 1 "no release notes for v2\.5\.1-rc\.1 .*$tried" \
  "an [Unreleased] with an empty body fails a release candidate" \
  -- bash "$script" v2.5.1-rc.1 "$file"

new_changelog <<'EOF'
## v2.6.0 (2026-12-01)


## v2.5.0

- Older line.
EOF
expect 1 'the "## v2\.6\.0" section of .* is empty' \
  "a stable section that is present but empty fails" -- bash "$script" v2.6.0 "$file"

new_changelog </dev/null
expect 1 'has no "## v2\.6\.0" section' "an empty CHANGELOG fails a stable tag" \
  -- bash "$script" v2.6.0 "$file"
expect 1 'no release notes for v2\.6\.0-rc\.1' "an empty CHANGELOG fails a release candidate" \
  -- bash "$script" v2.6.0-rc.1 "$file"

# The repository's CHANGELOG: v2.5.1 is still a subsection of [Unreleased],
# so its candidate gets the [Unreleased] notes and its stable tag waits for
# release-split.
expect 1 'has no "## v2\.5\.1" section' \
  "the repository CHANGELOG has no stable v2.5.1 section yet" \
  -- bash "$script" v2.5.1 "$repo_changelog"
rc_out="$scratch/rc-out"
if bash "$script" v2.5.1-rc.1 "$repo_changelog" >"$rc_out" 2>&1 \
  && head -1 "$rc_out" | grep -q '^These changes are planned as five releases' \
  && grep -qx '### v2.5.1 (tranche T0a, safety)' "$rc_out" \
  && ! grep -q '^## ' "$rc_out"; then
  echo "ok   the repository CHANGELOG gives v2.5.1-rc.1 the [Unreleased] notes"
else
  echo "FAIL the repository CHANGELOG v2.5.1-rc.1 notes: $(head -5 "$rc_out")" >&2
  failures=$((failures + 1))
fi

if [[ $failures -ne 0 ]]; then
  echo "$failures release-notes case(s) failed" >&2
  exit 1
fi
echo "all release-notes cases passed"

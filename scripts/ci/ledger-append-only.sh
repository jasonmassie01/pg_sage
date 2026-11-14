#!/usr/bin/env bash
# Append-only migration ledger (ci-30, ledger-pin). A released migration step
# never changes: every upgraded database would fail its next boot. When
# sidecar/internal/schema/testdata/migration_checksums.txt exists at BASE_REF,
# every line there must appear unchanged and in the same order as a prefix of
# the working copy, so new steps can only be appended. When the file is absent
# at BASE_REF there is no baseline yet and the check passes.
#
# CI passes the last release tag: $(git describe --tags --abbrev=0 --match 'v*').
# Exit 0 when append-only, 1 when a baseline line changed or went missing,
# 2 on bad usage or an unknown BASE_REF.
#
# Usage: ledger-append-only.sh BASE_REF
set -euo pipefail

me=ledger-append-only.sh
ledger=sidecar/internal/schema/testdata/migration_checksums.txt

if [[ $# -ne 1 || -z ${1:-} ]]; then
  echo "usage: $me BASE_REF  (a tag, branch or commit holding the released ledger)" >&2
  exit 2
fi
ref=$1
top=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "$me: not inside a git repository (run it from the pg_sage checkout)" >&2
  exit 2
}
cd "$top"

if [[ $ref == -* ]] || ! git rev-parse --verify --quiet "$ref^{commit}" >/dev/null; then
  echo "$me: base ref '$ref' does not exist (fetch tags: git fetch --tags)" >&2
  exit 2
fi
if ! git cat-file -e "$ref:$ledger" 2>/dev/null; then
  echo "no baseline: $ledger does not exist at $ref; nothing to compare"
  exit 0
fi
if [[ ! -f $ledger || ! -r $ledger ]]; then
  echo "$me: FAIL: cannot read $ledger, which exists at $ref (a released ledger" \
    "cannot be removed)" >&2
  exit 1
fi

base=$(mktemp)
trap 'rm -f "$base"' EXIT
git show "$ref:$ledger" >"$base"

# Lines are compared without a trailing CR, so a CRLF checkout of an
# unchanged ledger passes.
LEDGER_REF="$ref" LEDGER_PATH="$ledger" awk '
  BEGIN { ref = ENVIRON["LEDGER_REF"]; path = ENVIRON["LEDGER_PATH"] }
  FILENAME == ARGV[1] { sub(/\r$/, ""); want[++n] = $0; next }
  { sub(/\r$/, ""); got[++m] = $0 }
  END {
    for (i = 1; i <= n; i++) {
      if (i > m) {
        printf "line %d deleted: %s has %d lines, %s had %d\n", i, path, m, ref, n
        printf "  base: %s\n", want[i]
        exit 1
      }
      if (got[i] != want[i]) {
        printf "line %d changed in %s since %s (released lines are append-only)\n", \
          i, path, ref
        printf "  base: %s\n  now:  %s\n", want[i], got[i]
        exit 1
      }
    }
    printf "%s: %d baseline lines unchanged, %d appended since %s\n", path, n, m - n, ref
  }
' "$base" "$ledger" || {
  echo "$me: FAIL: append a new step instead of editing a released one" >&2
  exit 1
}

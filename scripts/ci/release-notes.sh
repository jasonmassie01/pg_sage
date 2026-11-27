#!/usr/bin/env bash
# ci-05: print the release notes for a tag: the body of its "## v<version>"
# section of the CHANGELOG (heading excluded, up to the next "## " heading,
# trimmed of blank lines at either end).
#
# A stable tag fails without a non-empty section of its own. A release
# candidate (v2.6.0-rc.1) falls back to its release's section (v2.6.0),
# then to "## [Unreleased]".
#
# usage: release-notes.sh TAG CHANGELOG   (TAG like v2.6.0)
set -euo pipefail

usage() {
  echo "usage: $0 TAG CHANGELOG (TAG like v2.6.0)" >&2
  exit 2
}

[ "$#" -eq 2 ] || usage
tag="$1"
changelog="$2"
case "$tag" in
  v[0-9]*) ;;
  *) usage ;;
esac
if [ ! -f "$changelog" ] || [ ! -r "$changelog" ]; then
  echo "release-notes: cannot read $changelog" >&2
  exit 1
fi

# has_heading HEADING: a line is HEADING or starts with "HEADING ".
has_heading() {
  awk -v h="$1" '{ sub(/\r$/, "") }
    $0 == h || index($0, h " ") == 1 { found = 1; exit }
    END { exit !found }' "$changelog"
}

# section HEADING: the body under the first such heading.
section() {
  awk -v h="$1" '{ sub(/\r$/, "") }
    !found { if ($0 == h || index($0, h " ") == 1) found = 1; next }
    /^## / { exit }
    { lines[++n] = $0; if ($0 ~ /[^[:space:]]/) { if (!first) first = n; last = n } }
    END { for (i = first; first && i <= last; i++) print lines[i] }' "$changelog"
}

version="${tag#v}"
candidates=("## v$version")
case "$version" in
  *-*) candidates+=("## v${version%%-*}" "## [Unreleased]") ;;
esac

for heading in "${candidates[@]}"; do
  notes="$(section "$heading")"
  if [ -n "$notes" ]; then
    printf '%s\n' "$notes"
    exit 0
  fi
done

if [ "${#candidates[@]}" -gt 1 ]; then
  echo "release-notes: no release notes for $tag in $changelog:" \
    "tried ${candidates[*]}" >&2
elif has_heading "${candidates[0]}"; then
  echo "release-notes: the \"${candidates[0]}\" section of $changelog is empty" >&2
else
  echo "release-notes: $changelog has no \"${candidates[0]}\" section;" \
    "add it before tagging $tag" >&2
fi
exit 1

package startup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ci-05 (then): release notes are the tag's CHANGELOG section, run with
// bash on fixtures. No concurrent-access tests: each run gets its own
// temporary directory.

const changelogFixture = `# Changelog

## [Unreleased]

- Unreleased work.

## v2.6.0 (2026-11-20) -- The next release

### Added

- Signed checksums.

## v2.5.01 (2026-11-10) -- Not 2.5.0

- Wrong section.

## v2.5.0 (2026-11-03) -- The Databricks release

- Databricks.

## v2.4.0 (2026-10-23) -- Empty below

## v2.3.0 (2026-10-07) -- Last

- Oldest.
`

func writeChangelog(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write changelog: %v", err)
	}
	return dir, path
}

func TestReleaseNotesTakeTheTagsChangelogSection(t *testing.T) {
	notes := ciScript(t, "release-notes.sh")
	dir, path := writeChangelog(t, changelogFixture)
	cases := []struct{ tag, has, hasNot string }{
		{"v2.5.0", "- Databricks.", "Wrong section"},
		{"v2.6.0", "- Signed checksums.", "Databricks"},
		{"v2.3.0", "- Oldest.", "Databricks"},
		// A release candidate takes its release's section...
		{"v2.6.0-rc.1", "- Signed checksums.", "Unreleased work"},
		// ...or, before that is written, [Unreleased].
		{"v2.7.0-rc.1", "- Unreleased work.", "Signed checksums"},
	}
	for _, c := range cases {
		out, code := runBash(t, dir, notes, c.tag, path)
		if code != 0 {
			t.Errorf("%s: exit %d: %s", c.tag, code, out)
			continue
		}
		if !strings.Contains(out, c.has) || strings.Contains(out, c.hasNot) {
			t.Errorf("%s: notes %q, want %q and not %q", c.tag, out, c.has, c.hasNot)
		}
		// A "## " line is a section heading; "### Added" is the body's.
		if strings.HasPrefix(out, "## ") || strings.Contains(out, "\n## ") {
			t.Errorf("%s: notes %q carry a section heading", c.tag, out)
		}
	}
}

func TestReleaseNotesFailClosedForAStableTag(t *testing.T) {
	notes := ciScript(t, "release-notes.sh")
	dir, path := writeChangelog(t, changelogFixture)
	cases := []struct{ name, tag, path, says string }{
		{"no section", "v2.7.0", path, "## v2.7.0"},
		{"empty section", "v2.4.0", path, "empty"},
		{"tag without v", "2.5.0", path, "usage"},
		{"empty tag", "", path, "usage"},
		{"missing changelog", "v2.5.0", filepath.Join(dir, "nope.md"), "nope.md"},
	}
	for _, c := range cases {
		out, code := runBash(t, dir, notes, c.tag, c.path)
		if code == 0 {
			t.Errorf("%s: exit 0, want a failure: %q", c.name, out)
			continue
		}
		if !strings.Contains(out, c.says) {
			t.Errorf("%s: output %q does not say %q", c.name, out, c.says)
		}
	}
	// A release candidate with neither its section nor [Unreleased] fails too.
	dir, path = writeChangelog(t, "# Changelog\n\n## v2.5.0 (x)\n\n- old\n")
	if out, code := runBash(t, dir, notes, "v2.6.0-rc.1", path); code == 0 {
		t.Errorf("rc with no section: exit 0, want a failure: %q", out)
	}
}

// The repository's own CHANGELOG yields notes for its newest release.
func TestReleaseNotesReadTheRepositoryChangelog(t *testing.T) {
	notes := ciScript(t, "release-notes.sh")
	root := filepath.Join(wave5SidecarRoot(t), "..")
	out, code := runBash(t, root, notes, "v2.5.0", "CHANGELOG.md")
	if code != 0 {
		t.Fatalf("v2.5.0 notes: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "Databricks") || strings.Contains(out, "## v2.4.0") {
		t.Fatalf("v2.5.0 notes do not hold just the v2.5.0 section: %.300q", out)
	}
}

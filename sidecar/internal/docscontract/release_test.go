package docscontract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ci-10: VERSION, the newest released CHANGELOG heading and the supported
// line in SECURITY.md name the same release, so bumping VERSION fails
// until the other two agree.
//
// No concurrent-access or state-transition tests: these are static checks
// over three repository files.

var (
	semverPattern     = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	changelogRelease  = regexp.MustCompile(`(?m)^## v([0-9]+\.[0-9]+\.[0-9]+)\b`)
	securityTableRows = regexp.MustCompile(
		`(?m)^\|[ \t]*([^|\n]*?)[ \t]*\|[ \t]*([^|\n]*?)[ \t]*\|[ \t]*$`)
)

// readReleaseFile reads one release-metadata file with LF line endings; the
// error names the path.
func readReleaseFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read release metadata %s: %w", path, err)
	}
	return strings.ReplaceAll(string(body), "\r\n", "\n"), nil
}

// topChangelogRelease is the version of the first "## vX.Y.Z" heading,
// skipping "## [Unreleased]".
func topChangelogRelease(changelog string) (string, error) {
	m := changelogRelease.FindStringSubmatch(changelog)
	if m == nil {
		return "", fmt.Errorf("CHANGELOG.md has no \"## vX.Y.Z\" release heading")
	}
	return m[1], nil
}

// supportedRows returns the version column of the first two data rows of
// the SECURITY.md supported-versions table (supported line, EOL line).
func supportedRows(security string) ([]string, error) {
	var rows []string
	for _, m := range securityTableRows.FindAllStringSubmatch(security, -1) {
		if m[1] == "Version" || strings.HasPrefix(m[1], "-") {
			continue
		}
		rows = append(rows, m[1]+" | "+m[2])
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("SECURITY.md supported-versions table has %d data rows, "+
			"want a supported line and an EOL line", len(rows))
	}
	return rows[:2], nil
}

// releaseMetadataProblem returns why the three files disagree, or nil.
func releaseMetadataProblem(version, changelog, security string) error {
	version = strings.TrimSpace(version)
	m := semverPattern.FindStringSubmatch(version)
	if m == nil {
		return fmt.Errorf("VERSION %q is not MAJOR.MINOR.PATCH", version)
	}
	top, err := topChangelogRelease(changelog)
	if err != nil {
		return err
	}
	if top != version {
		return fmt.Errorf("VERSION is %s but the newest CHANGELOG.md release is v%s", version, top)
	}
	rows, err := supportedRows(security)
	if err != nil {
		return err
	}
	line := m[1] + "." + m[2]
	if want := line + ".x | Yes"; !strings.HasPrefix(rows[0], want) {
		return fmt.Errorf("SECURITY.md first row is %q, want %q for VERSION %s",
			rows[0], want, version)
	}
	if want := "< " + line + " | No"; !strings.HasPrefix(rows[1], want) {
		return fmt.Errorf("SECURITY.md EOL row is %q, want %q for VERSION %s",
			rows[1], want, version)
	}
	return nil
}

func TestReleaseMetadataAgrees(t *testing.T) {
	root := repoRoot(t)
	files := map[string]string{}
	for _, name := range []string{"VERSION", "CHANGELOG.md", "SECURITY.md"} {
		body, err := readReleaseFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = body
	}
	err := releaseMetadataProblem(files["VERSION"], files["CHANGELOG.md"], files["SECURITY.md"])
	if err != nil {
		t.Fatalf("release metadata disagree: %v; update SECURITY.md and the CHANGELOG "+
			"heading in the same change as VERSION", err)
	}
}

func securityTable(supported, eol string) string {
	return "# Security Policy\n\n| Version | Supported          |\n" +
		"| ------- | ------------------ |\n" +
		"| " + supported + "   | Yes                |\n" +
		"| " + eol + "   | No (EOL)           |\n\nWhen a new minor version...\n"
}

func TestReleaseMetadataProblemCases(t *testing.T) {
	changelog := func(v string) string {
		return "# Changelog\n\n## [Unreleased]\n\n- next\n\n## v" + v + " (2026-11-03)\n\n" +
			"## v2.4.0 (2026-10-23)\n"
	}
	cases := []struct {
		name, version, changelog, security, wantErr string
	}{
		{"same release", "2.5.0", changelog("2.5.0"), securityTable("2.5.x", "< 2.5"), ""},
		{"patch release", "2.5.1\n", changelog("2.5.1"), securityTable("2.5.x", "< 2.5"), ""},
		{"minor bump without SECURITY.md", "2.6.0", changelog("2.6.0"),
			securityTable("2.5.x", "< 2.5"), `want "2.6.x | Yes"`},
		{"stale supported line", "2.5.0", changelog("2.5.0"),
			securityTable("1.5.x", "< 1.5"), `want "2.5.x | Yes"`},
		{"stale EOL line", "2.5.0", changelog("2.5.0"),
			securityTable("2.5.x", "< 2.4"), `want "< 2.5 | No"`},
		{"CHANGELOG behind VERSION", "2.5.1", changelog("2.5.0"),
			securityTable("2.5.x", "< 2.5"), "newest CHANGELOG.md release is v2.5.0"},
		{"no release heading", "2.5.0", "# Changelog\n\n## [Unreleased]\n",
			securityTable("2.5.x", "< 2.5"), "no \"## vX.Y.Z\" release heading"},
		{"empty VERSION", "", changelog("2.5.0"), securityTable("2.5.x", "< 2.5"),
			"not MAJOR.MINOR.PATCH"},
		{"v-prefixed VERSION", "v2.5.0", changelog("2.5.0"), securityTable("2.5.x", "< 2.5"),
			"not MAJOR.MINOR.PATCH"},
		{"no table", "2.5.0", changelog("2.5.0"), "# Security Policy\n", "0 data rows"},
	}
	for _, tc := range cases {
		err := releaseMetadataProblem(tc.version, tc.changelog, tc.security)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected problem %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: problem = %v, want one containing %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestReadReleaseFileNamesThePath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"VERSION", "SECURITY.md"} {
		missing := filepath.Join(dir, name)
		got, err := readReleaseFile(missing)
		if err == nil || !strings.Contains(err.Error(), missing) || got != "" {
			t.Errorf("readReleaseFile(%s) = %q, %v; want an error naming the path",
				missing, got, err)
		}
	}
	path := filepath.Join(dir, "VERSION")
	if err := os.WriteFile(path, []byte("2.5.0\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readReleaseFile(path); err != nil || got != "2.5.0\n" {
		t.Fatalf("readReleaseFile = %q, %v; want LF content", got, err)
	}
}

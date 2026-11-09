package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// SECURITY.md and docs/security.md are where operators decide which
// privileges pg_sage gets and what it may touch, so they must match the
// code: the supported release line is VERSION's minor, the policy never
// recommends a superuser, and the security page states what pg_sage can
// change and read instead of a "never" list the code contradicts.
//
// No concurrency, error-propagation or integration tests: this is a static
// check of files in the repository, with no runtime path.

const (
	securityPolicyPath = "../../../SECURITY.md"
	versionPath        = "../../../VERSION"
	securityDocPath    = "../../../docs/security.md"
	installationPath   = "../../../docs/installation.md"
)

var (
	// supportedRow is one "| X.Y.x | Yes |" row of the version table.
	supportedRow = regexp.MustCompile(`(?m)^\|\s*(\d+)\.(\d+)\.x\s*\|\s*Yes\b`)
	releaseLine  = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)\s*$`)
	// superuserAdvice matches text telling operators to run pg_sage as a
	// superuser; "non-superuser" and "not a superuser" do not match.
	superuserAdvice = regexp.MustCompile(`(?i)((designed|meant|intended|needs?) to|should|must)` +
		` (run|connect) as an? (PostgreSQL )?superuser|requires? an? (PostgreSQL )?superuser`)
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// checkSupportedVersion fails unless the policy supports exactly one
// release line and it is VERSION's major.minor.
func checkSupportedVersion(policy, version string) error {
	v := releaseLine.FindStringSubmatch(strings.TrimSpace(version))
	if v == nil {
		return fmt.Errorf("VERSION %q is not MAJOR.MINOR.PATCH", strings.TrimSpace(version))
	}
	want := v[1] + "." + v[2] + ".x"
	rows := supportedRow.FindAllStringSubmatch(policy, -1)
	if len(rows) == 0 {
		return fmt.Errorf("SECURITY.md has no supported row (| %s | Yes |) for VERSION %s",
			want, v[0])
	}
	if len(rows) > 1 {
		return fmt.Errorf("SECURITY.md supports %d release lines; only %s (VERSION %s) "+
			"may be supported", len(rows), want, v[0])
	}
	got := rows[0][1] + "." + rows[0][2] + ".x"
	if got != want {
		return fmt.Errorf("SECURITY.md supports %s but VERSION is %s: make the supported "+
			"row %s", got, v[0], want)
	}
	return nil
}

func TestSecurityDocsSupportedVersionMatchesVERSION(t *testing.T) {
	policy := readRepoFile(t, securityPolicyPath)
	version := readRepoFile(t, versionPath)
	if err := checkSupportedVersion(policy, version); err != nil {
		t.Fatal(err)
	}
	v := releaseLine.FindStringSubmatch(strings.TrimSpace(version))
	eol := "< " + v[1] + "." + v[2]
	if !strings.Contains(policy, eol) {
		t.Errorf("SECURITY.md must mark %q as end-of-life", eol)
	}
}

func TestSecurityDocsCheckSupportedVersion(t *testing.T) {
	table := func(rows ...string) string {
		return "| Version | Supported |\n| --- | --- |\n" + strings.Join(rows, "\n") + "\n"
	}
	cases := []struct {
		name, policy, version string
		wantErr               []string // substrings; nil means no error
	}{
		{"happy path", table("| 2.5.x   | Yes                |", "| < 2.5 | No (EOL) |"),
			"2.5.0\n", nil},
		{"stale line names both versions", table("| 1.5.x | Yes |", "| < 1.5 | No |"),
			"2.5.0", []string{"1.5.x", "2.5.0", "2.5.x"}},
		{"no table", "# Security Policy\n\nNo table here.\n", "2.5.0",
			[]string{"no supported row", "2.5.x"}},
		{"empty policy", "", "2.5.0", []string{"no supported row"}},
		{"only unsupported rows", table("| < 2.5 | No (EOL) |"), "2.5.0",
			[]string{"no supported row"}},
		{"two supported lines", table("| 2.5.x | Yes |", "| 2.4.x | Yes |"), "2.5.0",
			[]string{"2 release lines"}},
		{"two-digit minor", table("| 2.10.x | Yes |"), "2.10.0", nil},
		{"prefix of a two-digit minor", table("| 2.1.x | Yes |"), "2.10.0",
			[]string{"2.1.x", "2.10.x"}},
		{"two-digit minor for one-digit VERSION", table("| 2.10.x | Yes |"), "2.1.3",
			[]string{"2.10.x", "2.1.x"}},
		{"major differs", table("| 12.5.x | Yes |"), "2.5.0", []string{"12.5.x"}},
		{"patch row is not a line", table("| 2.5.0 | Yes |"), "2.5.0",
			[]string{"no supported row"}},
		{"empty VERSION", table("| 2.5.x | Yes |"), "", []string{"not MAJOR.MINOR.PATCH"}},
		{"malformed VERSION", table("| 2.5.x | Yes |"), "v2.5", []string{`"v2.5"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSupportedVersion(tc.policy, tc.version)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error naming %v, got none", tc.wantErr)
			}
			for _, s := range tc.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not name %q", err, s)
				}
			}
		})
	}
}

func TestSecurityDocsSuperuserAdvice(t *testing.T) {
	for _, bad := range []string{
		"pg_sage is designed to run as a PostgreSQL superuser so that it can execute DDL",
		"pg_sage must connect as a superuser.",
		"It requires a superuser.",
	} {
		if !superuserAdvice.MatchString(bad) {
			t.Errorf("superuser advice not detected in %q", bad)
		}
	}
	for _, good := range []string{
		"Connect pg_sage as a dedicated non-superuser role.",
		"pg_sage connects as a regular database user -- no superuser required.",
		"Run this as a superuser, or as your provider's admin role",
	} {
		if superuserAdvice.MatchString(good) {
			t.Errorf("%q is not superuser advice but was flagged", good)
		}
	}
}

func TestSecurityDocsPolicyRecommendsDedicatedRole(t *testing.T) {
	policy := readRepoFile(t, securityPolicyPath)
	if m := superuserAdvice.FindString(policy); m != "" {
		t.Errorf("SECURITY.md recommends a superuser (%q); pg_sage warns against it "+
			"(posture AP-14)", m)
	}
	for _, want := range []string{"non-superuser", "jmass0729@gmail.com", "72 hours"} {
		if !strings.Contains(policy, want) {
			t.Errorf("SECURITY.md no longer says %q", want)
		}
	}
}

func TestSecurityDocsPageMatchesCode(t *testing.T) {
	doc := readRepoFile(t, securityDocPath)
	for _, gone := range []string{
		"designed to run as a PostgreSQL superuser",
		"Never runs `INSERT`",
		"Never reads table row data",
		"Resource URIs",
		"## What pg_sage Never Does",
	} {
		if strings.Contains(doc, gone) {
			t.Errorf("docs/security.md still says %q, which the code contradicts", gone)
		}
	}
	for _, want := range []string{
		"## What pg_sage can change",
		"## What pg_sage can read",
		"(mcp.md",
		"EXPLAIN ANALYZE",
		"retention",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/security.md must contain %q", want)
		}
	}
}

// TestSecurityDocsGrantComments pins the grant comment in both docs:
// CREATE on a schema makes new objects there, but CREATE INDEX and
// CREATE STATISTICS need the table owner's privileges.
func TestSecurityDocsGrantComments(t *testing.T) {
	for _, path := range []string{securityDocPath, installationPath} {
		doc := readRepoFile(t, path)
		if !strings.Contains(doc, "GRANT <table owner role> TO sage_agent") {
			t.Errorf("%s must show the table-owner membership CREATE INDEX needs", path)
		}
		found := 0
		for _, line := range strings.Split(doc, "\n") {
			if !strings.Contains(line, "GRANT CREATE ON SCHEMA") {
				continue
			}
			found++
			if strings.Contains(line, "CREATE INDEX") ||
				strings.Contains(line, "STATISTICS") {
				t.Errorf("%s: %q credits schema CREATE with CREATE INDEX/STATISTICS, "+
					"which need table ownership", path, strings.TrimSpace(line))
			}
		}
		if found == 0 {
			t.Errorf("%s has no GRANT CREATE ON SCHEMA line to check", path)
		}
	}
}

// TestSecurityDocsListRoleManagementGrant pins CREATEROLE in both
// grant lists: agent governance cannot create or retire agent roles
// without it (agentguard self-check), so an operator choosing pg_sage's
// privileges from either page must see it.
func TestSecurityDocsListRoleManagementGrant(t *testing.T) {
	for _, path := range []string{securityPolicyPath, securityDocPath} {
		doc := readRepoFile(t, path)
		if !strings.Contains(doc, "`CREATEROLE` (PostgreSQL 16+)") &&
			!strings.Contains(doc, "`CREATEROLE` on PostgreSQL 16+") {
			t.Errorf("%s must say agent role management needs CREATEROLE on "+
				"PostgreSQL 16+", path)
		}
	}
}

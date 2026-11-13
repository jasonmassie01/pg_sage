package docscontract

import (
	"strings"
	"testing"
)

// footprint-10b: the claim scanner, on fixture pages.
//
// No concurrent-access, state-transition or integration tests: these are
// pure functions over strings.

var tested14To18 = []int{14, 15, 16, 17, 18}

func TestPGVersionsClaimsThatAgreeWithCI(t *testing.T) {
	for _, line := range []string{
		"", "No versions here.", "Requires PostgreSQL 14-18.", "PostgreSQL 14–18 works.",
		"from PostgreSQL 14 to 18", "PG14-18", "PG 14-18 and PG14-PG18",
		"PostgreSQL 14+", "PG 14+", "PostgreSQL 17+ adds it", "PostgreSQL 18+",
		"PG 15-16 only", "PostgreSQL 16, 17, 18", "PostgreSQL 14, 15, 16, 17 and 18",
		"PostgreSQL 14, 15, 16, 17, 18", "image pg17-r1 and postgres:17",
	} {
		if got, _ := versionClaimProblems("docs/x.md", line, tested14To18); len(got) != 0 {
			t.Errorf("%q: got problems %v, want none", line, got)
		}
	}
}

func TestPGVersionsClaimsThatDisagreeWithCI(t *testing.T) {
	cases := []struct {
		line, phrase, why string
	}{
		{"- PostgreSQL 14-17, or Docker", "PostgreSQL 14-17", "the range stops at 17"},
		{"Validated on PostgreSQL 14, 15, 16, 17. Zero code changes.",
			"PostgreSQL 14, 15, 16, 17", "the list names [14 15 16 17]"},
		{"PostgreSQL 13-18", "PostgreSQL 13-18", "PostgreSQL 13 is not tested"},
		{"PG14-19", "PG14-19", "PostgreSQL 19 is not tested"},
		{"PostgreSQL 13+", "PostgreSQL 13+", "PostgreSQL 13 is not tested"},
		{"PostgreSQL 19+", "PostgreSQL 19+", "PostgreSQL 19 is not tested"},
		{"PostgreSQL 18-14", "PostgreSQL 18-14", "the range is reversed"},
		{"PostgreSQL 12, 13", "PostgreSQL 12, 13", "PostgreSQL 12 is not tested"},
		{"PG14, 15, 16 and 18", "PG14, 15, 16 and 18", "the list names [14 15 16 18]"},
		{"Postgres 14 to 16", "Postgres 14 to 16", "the range stops at 16"},
	}
	for _, tc := range cases {
		got, n := versionClaimProblems("docs/x.md", tc.line, tested14To18)
		if n != 1 || len(got) != 1 {
			t.Errorf("%q: got %d claims, problems %v; want one problem", tc.line, n, got)
			continue
		}
		p := got[0]
		if p.File != "docs/x.md" || p.Line != 1 || p.Phrase != tc.phrase ||
			!strings.Contains(p.Why, tc.why) ||
			!strings.Contains(p.Why, "CI tests PostgreSQL 14-18") {
			t.Errorf("%q: got %+v; want phrase %q, why containing %q", tc.line, p, tc.phrase,
				tc.why)
		}
	}
}

func TestPGVersionsProblemNamesFileAndLine(t *testing.T) {
	body := "# Prerequisites\r\n\r\nUse PostgreSQL 14-18.\r\nOr a local PostgreSQL 14-17.\r\n"
	got, n := versionClaimProblems("docs/walkthrough-windows.md", body, tested14To18)
	if n != 2 || len(got) != 1 {
		t.Fatalf("got %d claims, problems %v; want 2 claims and one problem", n, got)
	}
	want := `docs/walkthrough-windows.md:4: "PostgreSQL 14-17": the range stops at 17; ` +
		"CI tests PostgreSQL 14-18"
	if got[0].String() != want {
		t.Fatalf("problem = %q, want %q", got[0].String(), want)
	}
}

func TestPGVersionsNoTestedMajorsFails(t *testing.T) {
	for _, majors := range [][]int{nil, {}} {
		got, _ := versionClaimProblems("docs/x.md", "PostgreSQL 14-18", majors)
		if len(got) != 1 || !strings.Contains(got[0].Why, "no tested majors") {
			t.Errorf("majors %v: got %v; want a no-tested-majors problem", majors, got)
		}
		if err := selfManagedRowProblem(selfManagedTable("14-18", "Verified (CI matrix)"),
			majors); err == nil {
			t.Errorf("majors %v: selfManagedRowProblem passed with nothing to check", majors)
		}
	}
}

func selfManagedTable(versions, status string) string {
	return "| Platform | PG Versions | Status |\n|---|---|---|\n" +
		"| Google Cloud SQL | 14, 15, 16, 17 | Verified |\n" +
		"| Self-managed | " + versions + " | " + status + " |\n" +
		"| Amazon RDS | 14-17 | Supported |\n"
}

func TestPGVersionsSelfManagedRowProblem(t *testing.T) {
	if err := selfManagedRowProblem(selfManagedTable("14-18", "Verified (CI matrix)"),
		tested14To18); err != nil {
		t.Fatalf("matching row: %v", err)
	}
	crlf := strings.ReplaceAll(selfManagedTable("14-18", "Verified (CI matrix)"), "\n", "\r\n")
	if err := selfManagedRowProblem(crlf, tested14To18); err != nil {
		t.Fatalf("CRLF row: %v", err)
	}
	cases := []struct{ body, want string }{
		{selfManagedTable("14, 15, 16, 17", "Verified"), `says "14, 15, 16, 17"`},
		{selfManagedTable("14-17", "Verified (CI matrix)"), `says "14-17"; CI tests 14-18`},
		{selfManagedTable("14-18", "Verified"), `status "Verified"`},
		{"| Platform | PG Versions |\n| Google AlloyDB | 17 | Verified |\n", "no Self-managed"},
		{"", "no Self-managed"},
	}
	for _, tc := range cases {
		err := selfManagedRowProblem(tc.body, tested14To18)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("selfManagedRowProblem(%q) = %v; want an error containing %q",
				tc.body, err, tc.want)
		}
	}
}

func TestPGVersionsAllowlistDropsOnlyItsClaimsAndReportsStaleEntries(t *testing.T) {
	problems := []claimProblem{
		{File: "docs/a.md", Line: 3, Phrase: "PG 14-16"},
		{File: "docs/b.md", Line: 9, Phrase: "PG 14-16"},
	}
	allowed := map[string]string{"docs/a.md|PG 14-16": "why", "docs/c.md|PG14-15": "gone"}
	kept, unused := withoutVersionSpecific(problems, allowed)
	if len(kept) != 1 || kept[0].File != "docs/b.md" {
		t.Errorf("kept = %v; want only the docs/b.md claim", kept)
	}
	if len(unused) != 1 || unused[0] != "docs/c.md|PG14-15" {
		t.Errorf("unused = %v; want the stale docs/c.md entry", unused)
	}
	kept, unused = withoutVersionSpecific(nil, nil)
	if len(kept) != 0 || len(unused) != 0 {
		t.Errorf("empty input: kept %v, unused %v; want none", kept, unused)
	}
}

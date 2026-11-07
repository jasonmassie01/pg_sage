package safetybench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No concurrent-access tests: the baseline functions are pure and the
// bench runs serially.

const pg17 = "PostgreSQL 17.10 (Debian 17.10-1.pgdg12+1) on x86_64-pc-linux-gnu"

func att(design string, class RefusalClass, intact bool) Attempt {
	return Attempt{Design: design, Observed: class, ChecksumsIntact: intact}
}

// diffReport is a small cgo report: two cases under three designs.
func diffReport(version string) Report {
	return Report{
		ServerVersion: version,
		SQLParser:     true,
		Designs:       []string{"read_only_txn", "privilege_role", "explain_guard"},
		ReadOnly: []CaseResult{
			{ID: "RO-06", Attempts: []Attempt{
				att("read_only_txn", ClassExecuted, true),
				att("privilege_role", ClassPrivilege, true),
				att("explain_guard", ClassBeforeExecution, true),
			}},
			{ID: "RO-14", Attempts: []Attempt{
				att("read_only_txn", ClassReadOnly, true),
				att("privilege_role", ClassPrivilege, true),
				att("explain_guard", ClassBeforeExecution, true),
			}},
		},
	}
}

func mustBaseline(t *testing.T, r Report) Baseline {
	t.Helper()
	b, err := BaselineFromReport(r, nil)
	if err != nil {
		t.Fatalf("BaselineFromReport: %v", err)
	}
	return b
}

func mustDiff(t *testing.T, r Report, b Baseline) BaselineDiff {
	t.Helper()
	d, err := DiffBaseline(r, b)
	if err != nil {
		t.Fatalf("DiffBaseline: %v", err)
	}
	return d
}

func joined(lines []string) string { return strings.Join(lines, "\n") }

func TestBaselineDiff_ReportMatchesItsOwnBaseline(t *testing.T) {
	r := diffReport(pg17)
	b := mustBaseline(t, r)
	if got := b.Designs["read_only_txn"]["RO-06"]; got != ClassExecuted {
		t.Fatalf("baseline read_only_txn RO-06 = %q, want executed", got)
	}
	if n := len(b.Designs) * len(b.Designs["read_only_txn"]); n != 6 {
		t.Fatalf("baseline has %d cells, want 6", n)
	}
	if lines := mustDiff(t, r, b).Failures(); len(lines) != 0 {
		t.Fatalf("a report against its own baseline: %v, want no failures", lines)
	}
}

// The plan's invalid-input case: RO-06 executes under read_only_txn today;
// a baseline claiming it was refused makes that cell a regression.
func TestBaselineDiff_RegressionNamesDesignAndCase(t *testing.T) {
	r := diffReport(pg17)
	b := mustBaseline(t, r)
	b.Designs["read_only_txn"]["RO-06"] = ClassPrivilege
	d := mustDiff(t, r, b)
	if len(d.Regressions) != 1 || len(d.Improvements) != 0 || len(d.Changes) != 0 {
		t.Fatalf("diff = %+v, want exactly one regression", d)
	}
	lines := d.Failures()
	if len(lines) != 1 || !strings.Contains(lines[0], "regression") ||
		!strings.Contains(lines[0], "read_only_txn RO-06") ||
		!strings.Contains(lines[0], "baseline privilege_error, observed executed") {
		t.Fatalf("failures = %q, want one regression naming read_only_txn RO-06", lines)
	}
}

func TestBaselineDiff_ImprovementAsksToRegenerate(t *testing.T) {
	r := diffReport(pg17)
	b := mustBaseline(t, r)
	r.ReadOnly[0].Attempts[0] = att("read_only_txn", ClassReadOnly, true)
	d := mustDiff(t, r, b)
	if len(d.Improvements) != 1 || len(d.Regressions) != 0 {
		t.Fatalf("diff = %+v, want one improvement", d)
	}
	got := joined(d.Failures())
	for _, want := range []string{"read_only_txn RO-06", "improved", "regenerate baseline",
		RegenerateCommand} {
		if !strings.Contains(got, want) {
			t.Errorf("failures %q lack %q", got, want)
		}
	}
}

func TestBaselineDiff_RankedTransitions(t *testing.T) {
	cases := []struct {
		name     string
		baseline RefusalClass
		observed Attempt
		want     string
	}{
		{"held to broken checksums", ClassPrivilege,
			att("privilege_role", ClassPrivilege, false), "regression"},
		{"held to not measured", ClassPrivilege,
			att("privilege_role", ClassOtherError, true), "regression"},
		{"not measured to executed", ClassOtherError,
			att("privilege_role", ClassExecuted, true), "regression"},
		{"unknown observed class", ClassOtherError,
			att("privilege_role", RefusalClass(""), true), "regression"},
		{"executed to not measured", ClassExecuted,
			att("privilege_role", ClassOtherError, true), "improved"},
		{"credited class swapped", ClassReadOnly,
			att("privilege_role", ClassPrivilege, true), "changed"},
		{"same class, same rank", ClassPrivilege,
			att("privilege_role", ClassPrivilege, true), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := diffReport(pg17)
			b := mustBaseline(t, r)
			b.Designs["privilege_role"]["RO-14"] = tc.baseline
			r.ReadOnly[1].Attempts[1] = tc.observed
			got := joined(mustDiff(t, r, b).Failures())
			if tc.want == "" {
				if got != "" {
					t.Fatalf("failures %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "privilege_role RO-14") {
				t.Fatalf("failures %q, want a %s naming privilege_role RO-14", got, tc.want)
			}
		})
	}
}

func TestBaselineDiff_MissingRowFails(t *testing.T) {
	r := diffReport(pg17)
	b := mustBaseline(t, r)
	delete(b.Designs["explain_guard"], "RO-14")
	got := mustDiff(t, r, b).Failures()
	if len(got) != 1 || !strings.Contains(got[0], "no baseline row for explain_guard RO-14") {
		t.Fatalf("failures = %q, want one missing row for explain_guard RO-14", got)
	}
}

func TestBaselineDiff_EmptyBaselineFailsEveryCell(t *testing.T) {
	for _, raw := range []string{"", "  \n", "{}", `{"designs": {}}`} {
		b, err := ParseBaseline([]byte(raw))
		if err != nil {
			t.Fatalf("ParseBaseline(%q): %v", raw, err)
		}
		got := mustDiff(t, diffReport(pg17), b).Failures()
		if len(got) != 6 {
			t.Fatalf("baseline %q: %d failures %q, want one per cell (6)", raw, len(got), got)
		}
		for _, line := range got {
			if !strings.Contains(line, "no baseline row for ") {
				t.Errorf("baseline %q: failure %q, want no baseline row", raw, line)
			}
		}
	}
}

func TestBaselineDiff_StaleRowFails(t *testing.T) {
	r := diffReport(pg17)
	b := mustBaseline(t, r)
	b.Designs["read_only_txn"]["RO-99"] = ClassReadOnly
	got := mustDiff(t, r, b).Failures()
	if len(got) != 1 || !strings.Contains(got[0], "read_only_txn RO-99") ||
		!strings.Contains(got[0], "not measured") {
		t.Fatalf("failures = %q, want one stale row for read_only_txn RO-99", got)
	}
}

// A per-major difference is accepted only on its major.
func TestBaselineDiff_PGOverrideAppliesToItsMajorOnly(t *testing.T) {
	b := mustBaseline(t, diffReport(pg17))
	b.PGOverrides = []PGOverride{{Major: 14, Design: "read_only_txn", Case: "RO-14",
		Observed: ClassExecuted, Reason: "lo_import ignores transaction_read_only before 16"}}
	for version, wantRegression := range map[string]bool{
		"PostgreSQL 14.23 (Debian 14.23-1.pgdg12+1)": false,
		pg17:                      true,
		"PostgreSQL 18beta1 on x": true,
	} {
		r := diffReport(version)
		r.ReadOnly[1].Attempts[0] = att("read_only_txn", ClassExecuted, true)
		d := mustDiff(t, r, b)
		want := 0
		if wantRegression {
			want = 1
		}
		if len(d.Regressions) != want || len(d.Failures()) != want {
			t.Errorf("%s: failures %q, want %d regression(s)", version, d.Failures(), want)
		}
	}
}

// Without the parser (CGO_ENABLED=0) the parser designs refuse everything,
// so their cells are listed as not compared instead of failing or passing.
func TestBaselineDiff_ParserDesignsNotComparedWithoutParser(t *testing.T) {
	r := diffReport(pg17)
	b := mustBaseline(t, r)
	r.SQLParser = false
	r.ReadOnly[0].Attempts[2] = att("explain_guard", ClassExecuted, true)
	delete(b.Designs, "explain_guard")
	d := mustDiff(t, r, b)
	if got := d.Failures(); len(got) != 0 {
		t.Fatalf("failures %q, want none (explain_guard not compared)", got)
	}
	if strings.Join(d.NotCompared, ",") != "explain_guard RO-06,explain_guard RO-14" {
		t.Fatalf("not compared = %v, want both explain_guard cells", d.NotCompared)
	}
	r.SQLParser = true
	if got := mustDiff(t, r, b).Failures(); len(got) != 2 {
		t.Fatalf("with the parser: failures %q, want both explain_guard cells", got)
	}
}

func TestBaselineDiff_UnreadableServerVersion(t *testing.T) {
	r := diffReport("")
	if _, err := DiffBaseline(r, mustBaseline(t, diffReport(pg17))); err == nil ||
		!strings.Contains(err.Error(), "PostgreSQL major") {
		t.Fatalf("DiffBaseline with no server version: %v, want a major error", err)
	}
}

func TestServerMajor(t *testing.T) {
	for in, want := range map[string]int{
		"PostgreSQL 14.23 (Debian 14.23-1.pgdg12+1) on x86_64": 14,
		"PostgreSQL 18beta1 on x86_64":                         18,
		" PostgreSQL 9.6.24":                                   9,
	} {
		if got, err := ServerMajor(in); err != nil || got != want {
			t.Errorf("ServerMajor(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "EnterpriseDB 15.2", "PostgreSQL x"} {
		if got, err := ServerMajor(in); err == nil {
			t.Errorf("ServerMajor(%q) = %d, want an error", in, got)
		}
	}
}

// override renders one pg_overrides entry for TestParseBaseline_Rejects.
func override(major int, id, observed, reason string) string {
	return fmt.Sprintf(`{"major": %d, "design": "read_only_txn", "case": %q, `+
		`"observed": %q, "reason": %q}`, major, id, observed, reason)
}

func TestParseBaseline_Rejects(t *testing.T) {
	withOverrides := func(entries ...string) string {
		return `{"designs": {"read_only_txn": {"RO-14": "read_only_error"}}, ` +
			`"pg_overrides": [` + strings.Join(entries, ", ") + `]}`
	}
	noReason := `{"major": 14, "design": "read_only_txn", "case": "RO-14", ` +
		`"observed": "executed"}`
	ok := override(14, "RO-14", "executed", "r")
	cases := []struct{ name, raw, want string }{
		{"malformed", `{"designs": `, "parse JSON"},
		{"unknown field", `{"design": {}}`, "unknown field"},
		{"unknown class", `{"designs": {"read_only_txn": {"RO-14": "held"}}}`, `"held"`},
		{"no reason", withOverrides(noReason), "reason is required"},
		{"blank reason", withOverrides(override(14, "RO-14", "executed", " ")),
			"reason is required"},
		{"zero major", withOverrides(override(0, "RO-14", "executed", "r")), "major"},
		{"reference major", withOverrides(override(17, "RO-14", "executed", "r")),
			"reference major"},
		{"trailing data", `{"designs": {}} {"designs": {}}`, "trailing data"},
		{"bad class", withOverrides(override(14, "RO-14", "nope", "r")), `"nope"`},
		{"no such cell", withOverrides(override(14, "RO-99", "executed", "r")),
			"does not have"},
		{"duplicate", withOverrides(ok, ok), "duplicate"},
	}
	for _, tc := range cases {
		if _, err := ParseBaseline([]byte(tc.raw)); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %s", tc.name, err, tc.want)
		}
	}
	if _, err := ParseBaseline([]byte(withOverrides(ok))); err != nil {
		t.Errorf("a valid override: %v", err)
	}
}

func TestLoadBaseline_ErrorsNameThePath(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(bad, []byte(`{"designs": [}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadBaseline(bad)
	if err == nil || !strings.Contains(err.Error(), bad) ||
		!strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("malformed baseline: %v, want the path and the JSON error", err)
	}
	missing := filepath.Join(dir, "nope.json")
	if _, err := LoadBaseline(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing baseline: %v, want the path", err)
	}
}

func TestBaselineFromReport_RefusesUnfitReports(t *testing.T) {
	noParser := diffReport(pg17)
	noParser.SQLParser = false
	if _, err := BaselineFromReport(noParser, nil); err == nil ||
		!strings.Contains(err.Error(), "parser") {
		t.Errorf("report without the parser: %v, want a parser error", err)
	}
	if _, err := BaselineFromReport(diffReport("PostgreSQL 14.23"), nil); err == nil ||
		!strings.Contains(err.Error(), "pg_overrides") {
		t.Errorf("PG14 report: %v, want it pointed at pg_overrides", err)
	}
	if _, err := BaselineFromReport(Report{ServerVersion: pg17, SQLParser: true}, nil); err == nil {
		t.Error("a report with no read-only cells must not make a baseline")
	}
}

func TestBaselineFromReport_KeepsValidOverrides(t *testing.T) {
	keep := []PGOverride{{Major: 15, Design: "read_only_txn", Case: "RO-14",
		Observed: ClassExecuted, Reason: "lo_import before 16"}}
	b, err := BaselineFromReport(diffReport(pg17), keep)
	if err != nil {
		t.Fatalf("BaselineFromReport: %v", err)
	}
	if len(b.PGOverrides) != 1 || b.PGOverrides[0] != keep[0] {
		t.Fatalf("overrides = %+v, want %+v kept", b.PGOverrides, keep)
	}
	gone := []PGOverride{{Major: 15, Design: "read_only_txn", Case: "RO-99",
		Observed: ClassExecuted, Reason: "r"}}
	if _, err := BaselineFromReport(diffReport(pg17), gone); err == nil ||
		!strings.Contains(err.Error(), "RO-99") {
		t.Fatalf("override for a vanished case: %v, want an error naming RO-99", err)
	}
}

// The committed baseline covers every design and every corpus case, so a
// new case or design cannot reach CI without a baseline row.
func TestCommittedBaselineCoversTheCorpus(t *testing.T) {
	b, err := LoadBaseline(BaselineFile)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := LoadCases(false)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	designs := Designs(readOnlyRole)
	defer closeDesigns(designs)
	if len(b.Designs) != len(designs) {
		t.Fatalf("baseline has %d designs, want %d", len(b.Designs), len(designs))
	}
	for _, d := range designs {
		if len(b.Designs[d.Name()]) != len(cases) {
			t.Errorf("baseline %s has %d cells, want %d", d.Name(), len(b.Designs[d.Name()]),
				len(cases))
		}
		for _, c := range cases {
			if _, ok := b.Designs[d.Name()][c.ID]; !ok {
				t.Errorf("baseline has no row for %s %s", d.Name(), c.ID)
			}
		}
	}
	for _, c := range []string{"sc-create", "sc-insert", "sc-update"} {
		if got := b.Designs["privilege_role"][c]; !got.Credited() {
			t.Errorf("baseline privilege_role %s = %s, want a credited refusal", c, got)
		}
	}
}

// The report says when the parser designs are not authoritative.
func TestMarkdownNamesAMissingParser(t *testing.T) {
	const note = "SQL parser: not in this build"
	r := diffReport(pg17)
	if strings.Contains(r.Markdown(), note) {
		t.Fatal("a cgo report must not carry the missing-parser note")
	}
	r.SQLParser = false
	if !strings.Contains(r.Markdown(), note) {
		t.Fatal("a report without the parser must say so")
	}
}

// CI always builds with cgo: a parser-less run there would publish
// explain_guard and agent_query as holding every case while the baseline
// skips their cells, so CI requires the parser.
func TestParserRequired(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{{"true", true}, {"", false}, {"false", false}}
	for _, tc := range cases {
		getenv := func(k string) string {
			if k == "GITHUB_ACTIONS" {
				return tc.value
			}
			return "true"
		}
		if got := ParserRequired(getenv); got != tc.want {
			t.Errorf("GITHUB_ACTIONS=%q: ParserRequired = %v, want %v", tc.value, got, tc.want)
		}
	}
}

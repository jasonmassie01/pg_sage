package sre

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The contract's checks, run on altered copies of the doc: each must fail
// for the reason it exists, or TestDocsContract_DocMatchesTheCode proves
// nothing.

func realSREDoc(t *testing.T) string {
	t.Helper()
	doc, err := readSREDoc(sreDocPath)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func requireProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Fatalf("problems %q do not include %q", problems, want)
}

func TestDocsContract_StaleClaimsFail(t *testing.T) {
	doc := realSREDoc(t)
	for _, claim := range []string{"R1 has no actions.", "R1 has no action the model could " +
		"trigger.", "One investigation makes at most 2 model calls.",
		"no `pg_signal_backend` (R1 never signals a backend)"} {
		requireProblem(t, docProblems(doc+"\n"+claim+"\n"), "stale claim")
	}
}

func TestDocsContract_MissingFileFailsNotSkips(t *testing.T) {
	_, err := readSREDoc("../../../docs/no-such-sre-doc.md")
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "no-such-sre-doc") {
		t.Fatalf("err = %v, want a not-exist error naming the file", err)
	}
}

// A CRLF checkout of the same page must pass: the section and row checks
// would otherwise miss "### Action privileges\r".
func TestDocsContract_CRLFCheckoutPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crlf.md")
	crlf := strings.ReplaceAll(realSREDoc(t), "\n", "\r\n")
	if err := os.WriteFile(path, []byte(crlf), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readSREDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if problems := docProblems(doc); len(problems) > 0 {
		t.Fatalf("a CRLF checkout of the doc fails the contract:\n%s",
			strings.Join(problems, "\n"))
	}
}

func TestDocsContract_EmptyDocFailsEveryCheck(t *testing.T) {
	problems := docProblems("")
	for _, want := range []string{`plan "narrow" has no 8-column row`,
		`plan "broad" has no 8-column row`, `no "### Investigation privileges" section`,
		`no "### Action privileges" section`, "missing limit statement",
		"do not name pg_signal_backend"} {
		requireProblem(t, problems, want)
	}
}

func TestDocsContract_WrongPlanNumberFails(t *testing.T) {
	doc := realSREDoc(t)
	row := strings.Join(tableRow(doc, PlanBroad), " | ")
	if row == "" {
		t.Fatal("the doc has no broad plan row")
	}
	bad := strings.Replace(doc, "| 64,000 |", "| 65,000 |", 1)
	requireProblem(t, docProblems(bad), `plan "broad" column 6 is "65,000", want "64,000"`)
	noExplain := strings.Replace(doc, "`explain_statement` for `plan_regression`", "", 1)
	requireProblem(t, docProblems(noExplain),
		`plan "narrow" does not list "explain_statement" for "plan_regression"`)
}

func TestDocsContract_ChangedCeilingFails(t *testing.T) {
	doc := realSREDoc(t)
	bad := strings.ReplaceAll(doc, "2,048 reasoning tokens per call", "2,000 reasoning")
	requireProblem(t, docProblems(bad), "2,048 reasoning tokens per call")
}

// Boundary: the first and the last read of the catalog each need a row,
// and so do the reads outside it.
func TestDocsContract_PrivilegeRowsAtTheEdges(t *testing.T) {
	doc := realSREDoc(t)
	sec := section(doc, "### Investigation privileges")
	if sec == "" {
		t.Fatal("the doc has no investigation privileges section")
	}
	ids := probes.Catalog().IDs()
	for _, id := range []string{string(ids[0]), string(ids[len(ids)-1]),
		string(ExplainProbeID), ToolHistory} {
		renamed := strings.ReplaceAll(sec, "`"+id+"`", "`renamed_"+id+"`")
		bad := strings.Replace(doc, sec, renamed, 1)
		requireProblem(t, docProblems(bad),
			`"### Investigation privileges" has no row for "`+id+`"`)
	}
}

func TestDocsContract_ActionRowsAreRequired(t *testing.T) {
	doc := realSREDoc(t)
	bad := strings.Replace(doc, "| `cancel_backend`", "| `cancel`", 1)
	requireProblem(t, docProblems(bad), `"### Action privileges" has no row for "cancel_backend"`)
}

func TestDocsContract_Thousands(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000",
		32000: "32,000", 1234567: "1,234,567"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDocsContract_SectionStopsAtTheNextHeading(t *testing.T) {
	doc := "x\n### A\nin a\n#### deeper\nstill a\n### B\nin b\n## C\n"
	if got := section(doc, "### A"); !strings.Contains(got, "still a") ||
		strings.Contains(got, "in b") {
		t.Fatalf("section A = %q", got)
	}
	if got := section(doc, "### B"); got != "in b" {
		t.Fatalf("section B = %q, want %q", got, "in b")
	}
	if got := section(doc, "### Missing"); got != "" {
		t.Fatalf("missing section = %q", got)
	}
}

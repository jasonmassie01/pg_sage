package selfmonitor

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// No concurrent access tests: the check reads source files only.

// qualifiedRefsBaseline listed, per production file, the unqualified
// extension references left before the search_path pin (security-01c).
// security-01c-q drove it to its header: it must stay without entries.
const qualifiedRefsBaseline = "testdata/unqualified_extension_refs.txt"

func refsIn(t *testing.T, src string) []string {
	t.Helper()
	refs, err := fileRefs(token.NewFileSet(), "probe.go", src)
	if err != nil {
		t.Fatalf("fileRefs: %v", err)
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.String())
	}
	return out
}

// Error propagation: an unqualified extension object in a probe is
// reported by file and line, each one of a raw string on its own line.
func TestQualifiedRefsReportsFileAndLine(t *testing.T) {
	src := "package p\n\nconst probe = `SELECT calls\n  FROM pg_stat_statements\n" +
		"  JOIN pg_catalog.pg_roles r ON true`\n\n" +
		"var hypo = \"SELECT * FROM hypopg_create_index($1)\"\n"
	want := []string{"probe.go:4: pg_stat_statements", "probe.go:7: hypopg_create_index"}
	if got := refsIn(t, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %q, want %q", got, want)
	}
}

func TestQualifiedRefsFlagsOIDLookupLiterals(t *testing.T) {
	src := "package p\n" +
		"const a = \"SELECT to_regclass('pg_stat_statements') IS NOT NULL\"\n" +
		"const b = \"SELECT 'hypopg_reset'::regproc\"\n" +
		"const c = \"SELECT to_regprocedure('pgstattuple(regclass)')\"\n"
	want := []string{"probe.go:2: pg_stat_statements", "probe.go:3: hypopg_reset",
		"probe.go:4: pgstattuple"}
	if got := refsIn(t, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %q, want %q", got, want)
	}
}

// Qualified names, setting names, extension names, other literals,
// comments and texts that are not SQL are not references.
func TestQualifiedRefsAcceptsWhatResolvesUnderThePin(t *testing.T) {
	src := "package p\n" +
		"const a = `SELECT * FROM public.pg_stat_statements s\n" +
		"  JOIN \"x\".hypopg_list_indexes ON true`\n" +
		"const b = \"SELECT current_setting('pg_stat_statements.max')\"\n" +
		"const c = \"SELECT 1 FROM pg_catalog.pg_extension\" +\n" +
		"  \" WHERE extname = 'pg_stat_statements'\"\n" +
		"const d = \"CREATE EXTENSION IF NOT EXISTS hypopg; SELECT 1\"\n" +
		"const e = \"SELECT 1 -- pg_stat_statements\"\n" +
		"const f = \"pg_stat_statements is not installed\"\n" +
		"const g = \"SELECT to_regclass('ext.pg_stat_statements')\"\n" +
		"const h = \"SELECT pg_stat_statements.calls FROM ext.pg_stat_statements\"\n"
	if got := refsIn(t, src); len(got) != 0 {
		t.Fatalf("refs = %q, want none", got)
	}
}

// A + chain is SQL when any of its literals is: the FROM line of a
// statement built in pieces is still checked.
func TestQualifiedRefsChecksConcatenatedStatements(t *testing.T) {
	src := "package p\nfunc q(cols string) string {\n" +
		"\treturn \"SELECT \" + cols +\n\t\t\" FROM pg_stat_statements_info\"\n}\n"
	want := []string{"probe.go:4: pg_stat_statements_info"}
	if got := refsIn(t, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %q, want %q", got, want)
	}
}

// A name passed to a call inside the chain (qualified at run time) is an
// argument, not statement text; a statement inside such a call is read.
func TestQualifiedRefsSkipsCallArgumentsInChains(t *testing.T) {
	src := "package p\nfunc q(s session) string {\n" +
		"\treturn \"SELECT \" + s.function(\"hypopg_reset\") + \"()\" +\n" +
		"\t\tf(\"SELECT * FROM pg_stat_statements\")\n}\n"
	want := []string{"probe.go:4: pg_stat_statements"}
	if got := refsIn(t, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %q, want %q", got, want)
	}
}

// A literal holding a backslash (a regular expression) ends at its quote,
// so the statement after it is still read.
func TestQualifiedRefsReadsPastBackslashLiterals(t *testing.T) {
	src := "package p\nconst q = `SELECT (regexp_match(h, 'Set\\(w \"(\\d+)MB\"\\)'))[1]\n" +
		"FROM sage.query_hints h\nJOIN pg_stat_statements s USING (queryid)`\n"
	want := []string{"probe.go:4: pg_stat_statements"}
	if got := refsIn(t, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %q, want %q", got, want)
	}
}

// Invalid input: a file that does not parse is an error, not a pass.
func TestQualifiedRefsRefusesUnparseableSource(t *testing.T) {
	if _, err := fileRefs(token.NewFileSet(), "bad.go", "package p\nconst = "); err == nil {
		t.Fatal("fileRefs accepted a file that does not parse")
	}
}

// Nil/empty: no source text, no references.
func TestQualifiedRefsEmptySQL(t *testing.T) {
	if at, names := unqualifiedRefs(sqlTokens("")); len(at) != 0 || len(names) != 0 {
		t.Fatalf("unqualifiedRefs(\"\") = %v %q, want none", at, names)
	}
}

// Prose marked //qualifiedrefs:prose (a model prompt, a tool description)
// names extensions in words and is not checked.
func TestQualifiedRefsSkipsMarkedProse(t *testing.T) {
	src := "package p\n" +
		"//qualifiedrefs:prose tool description for the model\n" +
		"var d = \"Plan-only EXPLAIN of one statement named by its \" +\n" +
		"\t\"pg_stat_statements queryid\"\n" +
		"var e = \"Safe EXPLAIN of a pg_stat_statements queryid\" " +
		"//qualifiedrefs:prose model text\n"
	if got := refsIn(t, src); len(got) != 0 {
		t.Fatalf("refs = %q, want none for marked prose", got)
	}
}

// A marker without a reason, or on a literal that opens as a statement, is
// still checked: the marker cannot excuse SQL.
func TestQualifiedRefsProseMarkerCannotHideSQL(t *testing.T) {
	src := "package p\n" +
		"//qualifiedrefs:prose not really prose\n" +
		"const q = `SELECT calls\n  FROM pg_stat_statements`\n" +
		"//qualifiedrefs:prose\n" +
		"var d = \"Plan-only EXPLAIN of a pg_stat_statements queryid\"\n" +
		"//qualifiedrefs:prose model text\n" +
		"var w = \"WITH s AS (SELECT 1 FROM pgstattuple_approx($1)) SELECT 1\"\n"
	want := []string{"probe.go:4: pg_stat_statements", "probe.go:6: pg_stat_statements",
		"probe.go:8: pgstattuple_approx"}
	if got := refsIn(t, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %q, want %q", got, want)
	}
}

// The strict check fails a tree holding one bare reference, by file and
// line.
func TestStrictQualifiedRefsCheckFailsFixture(t *testing.T) {
	refs, err := treeRefs(filepath.Join("testdata", "qualifiedrefs"), "internal")
	if err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	err = strictRefsProblem(refs)
	if err == nil {
		t.Fatal("strict check passed a fixture with a bare FROM pg_stat_statements")
	}
	want := "internal/probe/probe.go:7: pg_stat_statements"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("strict check error %q does not name %q", err, want)
	}
	if strings.Contains(err.Error(), "probe.go:12") {
		t.Fatalf("strict check reported the marked prose: %q", err)
	}
}

// Nil/empty: no references, no problem.
func TestStrictQualifiedRefsCheckEmpty(t *testing.T) {
	if err := strictRefsProblem(nil); err != nil {
		t.Fatalf("strictRefsProblem(nil) = %v, want nil", err)
	}
}

// The ratchet, now strict: no production file names an extension object
// without its schema, and the former baseline holds no entries.
func TestProductionSQLQualifiesExtensionObjects(t *testing.T) {
	if baseline := readQualifiedBaseline(t); len(baseline) != 0 {
		t.Fatalf("%s must hold only its header, has %v", qualifiedRefsBaseline, baseline)
	}
	if err := strictRefsProblem(productionRefs(t)); err != nil {
		t.Fatal(err)
	}
}

// strictRefsProblem lists every reference, or nil when there is none.
func strictRefsProblem(refs []unqualifiedRef) error {
	if len(refs) == 0 {
		return nil
	}
	lines := make([]string, 0, len(refs))
	for _, r := range refs {
		lines = append(lines, r.String())
	}
	sort.Strings(lines)
	return fmt.Errorf("extension objects must be schema-qualified (internal/extschema) "+
		"under the pinned search_path:\n  %s", strings.Join(lines, "\n  "))
}

func readQualifiedBaseline(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(qualifiedRefsBaseline)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	out := map[string]int{}
	for i, line := range strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		file, count, ok := strings.Cut(line, " ")
		n, err := strconv.Atoi(count)
		if !ok || err != nil || n <= 0 {
			t.Fatalf("%s:%d: want \"<file> <count>\", got %q", qualifiedRefsBaseline, i+1, line)
		}
		out[file] = n
	}
	return out
}

package selfmonitor

import (
	"fmt"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// No concurrent access tests: the check reads source files only.

// qualifiedRefsBaseline counts, per production file, the unqualified
// extension references left before the search_path pin can land
// (security-01c). A file may only lose entries; set
// SAGE_UPDATE_QUALIFIED_REFS=1 to rewrite it after qualifying a file.
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

// The ratchet: no production file gains an unqualified extension
// reference, and a file that lost some must lower its baseline.
func TestProductionSQLQualifiesExtensionObjects(t *testing.T) {
	refs := productionRefs(t)
	counts := map[string]int{}
	byFile := map[string][]string{}
	for _, r := range refs {
		counts[r.file]++
		byFile[r.file] = append(byFile[r.file], r.String())
	}
	if os.Getenv("SAGE_UPDATE_QUALIFIED_REFS") == "1" {
		writeQualifiedBaseline(t, counts)
		return
	}
	baseline := readQualifiedBaseline(t)
	var problems []string
	for file, n := range counts {
		if n > baseline[file] {
			problems = append(problems, fmt.Sprintf("%d unqualified (baseline %d):\n  %s",
				n, baseline[file], strings.Join(byFile[file], "\n  ")))
		}
	}
	for file, n := range baseline {
		if counts[file] < n {
			problems = append(problems, fmt.Sprintf("%s: %d unqualified, baseline %d: "+
				"lower the baseline (SAGE_UPDATE_QUALIFIED_REFS=1)", file, counts[file], n))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("extension objects must be schema-qualified under the pinned "+
			"search_path:\n%s", strings.Join(problems, "\n"))
	}
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

func writeQualifiedBaseline(t *testing.T, counts map[string]int) {
	t.Helper()
	files := make([]string, 0, len(counts))
	for f := range counts {
		files = append(files, f)
	}
	sort.Strings(files)
	var b strings.Builder
	b.WriteString("# Unqualified extension references per production file " +
		"(security-01c); only ever lower.\n")
	for _, f := range files {
		fmt.Fprintf(&b, "%s %d\n", f, counts[f])
	}
	if err := os.WriteFile(qualifiedRefsBaseline, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
}

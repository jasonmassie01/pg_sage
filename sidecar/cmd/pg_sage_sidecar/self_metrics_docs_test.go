package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// footprint-10: a DBA must be able to find every pg_sage_self_* family
// pg_sage exports, and the architecture's PostgreSQL range must be the
// one CI tests. These tests read the metric writers' source, the docs and
// the CI workflow. No concurrent access tests: the helpers are pure
// functions over strings.

const (
	archDocPath     = "../../../docs/architecture.md"
	findingsDocPath = "../../../docs/findings.md"
	ciWorkflowPath  = "../../../.github/workflows/ci.yml"
)

var selfMetricName = regexp.MustCompile(`pg_sage_self_[a-z0-9_]*[a-z0-9]`)

// selfMetricNames lists, sorted and once each, the pg_sage_self_* names
// the given sources spell.
func selfMetricNames(sources ...string) []string {
	seen := map[string]bool{}
	for _, src := range sources {
		for _, name := range selfMetricName.FindAllString(src, -1) {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// undocumentedSelfMetrics is every name no doc spells as a whole word: a
// name that only prefixes a documented one (pg_sage_self_budget inside
// pg_sage_self_budget_exceeded) is not documented by it.
func undocumentedSelfMetrics(names []string, docs ...string) []string {
	var out []string
	for _, name := range names {
		word := regexp.MustCompile(`(^|[^a-z0-9_])` + regexp.QuoteMeta(name) + `($|[^a-z0-9_])`)
		found := false
		for _, doc := range docs {
			if word.MatchString(doc) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, name)
		}
	}
	return out
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// emittedSelfMetricNames reads every non-test self_*_metrics.go writer.
func emittedSelfMetricNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("self_*_metrics.go")
	if err != nil {
		t.Fatalf("glob self_*_metrics.go: %v", err)
	}
	var sources []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			sources = append(sources, readRepoFile(t, f))
		}
	}
	if len(sources) == 0 {
		t.Fatal("no self_*_metrics.go writer found: the docs check would pass vacuously")
	}
	return selfMetricNames(sources...)
}

func TestSelfMetricsDocs_EveryEmittedFamilyIsDocumented(t *testing.T) {
	names := emittedSelfMetricNames(t)
	arch, findings := readRepoFile(t, archDocPath), readRepoFile(t, findingsDocPath)
	if missing := undocumentedSelfMetrics(names, arch, findings); len(missing) != 0 {
		t.Fatalf("pg_sage_self_* families exported but documented in neither "+
			"docs/architecture.md nor docs/findings.md: %s", strings.Join(missing, ", "))
	}
}

// The writers really emit the families the docs describe: an extraction
// that found nothing (or lost a known family) would make the docs check
// pass on no input.
func TestSelfMetricsDocs_WritersSpellTheKnownFamilies(t *testing.T) {
	names := emittedSelfMetricNames(t)
	for _, want := range []string{"pg_sage_self_db_time_ms_per_cycle",
		"pg_sage_self_schema_bytes", "pg_sage_self_budget",
		"pg_sage_self_component_db_time_ms_per_hour", "pg_sage_self_loop_runs_total"} {
		if !containsString(names, want) {
			t.Errorf("%s not found among the writers' names %v", want, names)
		}
	}
	if len(names) < 14 {
		t.Errorf("only %d pg_sage_self_* names extracted (%v), want at least 14",
			len(names), names)
	}
}

func TestSelfMetricsDocs_NamesAreExtractedOnceAndWhole(t *testing.T) {
	src := `"pg_sage_self_a_total" x pg_sage_self_a_total, "pg_sage_self_" + k ` +
		`pg_sage_self_b_ms{database}`
	got := selfMetricNames(src)
	want := []string{"pg_sage_self_a_total", "pg_sage_self_b_ms"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("names %v, want %v", got, want)
	}
	if got := selfMetricNames(); len(got) != 0 {
		t.Fatalf("no source gave %v, want none", got)
	}
	if got := selfMetricNames(""); len(got) != 0 {
		t.Fatalf("empty source gave %v, want none", got)
	}
}

// A family added without docs is reported by name; a prefix of a
// documented family is not documented by it.
func TestSelfMetricsDocs_AnUndocumentedFamilyIsNamed(t *testing.T) {
	docs := "Exports `pg_sage_self_budget_exceeded{resource}` and pg_sage_self_x_ms."
	names := []string{"pg_sage_self_budget", "pg_sage_self_budget_exceeded",
		"pg_sage_self_new_family_total", "pg_sage_self_x_ms"}
	got := undocumentedSelfMetrics(names, docs)
	want := []string{"pg_sage_self_budget", "pg_sage_self_new_family_total"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("undocumented %v, want %v", got, want)
	}
	if got := undocumentedSelfMetrics(names); len(got) != len(names) {
		t.Fatalf("with no docs %v undocumented, want all of %v", got, names)
	}
	if got := undocumentedSelfMetrics(nil, docs); len(got) != 0 {
		t.Fatalf("no names gave %v undocumented, want none", got)
	}
}

var docPGRange = regexp.MustCompile(`PostgreSQL (\d+)-(\d+)`)

// stated PG range of a doc: the first "PostgreSQL A-B".
func docPostgresRange(doc string) (lo, hi int, ok bool) {
	m := docPGRange.FindStringSubmatch(doc)
	if m == nil {
		return 0, 0, false
	}
	lo, err1 := strconv.Atoi(m[1])
	hi, err2 := strconv.Atoi(m[2])
	return lo, hi, err1 == nil && err2 == nil && lo <= hi
}

var (
	ciMatrixPG = regexp.MustCompile(`(?m)^\s*pg:\s*\[([^\]]*)\]`)
	ciImagePG  = regexp.MustCompile(`pgvector:[0-9.]+-pg(\d+)`)
	ciNumber   = regexp.MustCompile(`\d+`)
)

// ciPostgresVersions lists, sorted and once each, the PostgreSQL majors
// the workflow tests: its pg: matrices and its CI image tags.
func ciPostgresVersions(yml string) []int {
	seen := map[int]bool{}
	for _, m := range ciMatrixPG.FindAllStringSubmatch(yml, -1) {
		for _, n := range ciNumber.FindAllString(m[1], -1) {
			v, _ := strconv.Atoi(n)
			seen[v] = true
		}
	}
	for _, m := range ciImagePG.FindAllStringSubmatch(yml, -1) {
		v, _ := strconv.Atoi(m[1])
		seen[v] = true
	}
	out := make([]int, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func TestArchitectureDocs_PostgresRangeIsTheCIMatrix(t *testing.T) {
	versions := ciPostgresVersions(readRepoFile(t, ciWorkflowPath))
	if len(versions) < 2 {
		t.Fatalf("CI PostgreSQL versions %v: the workflow's matrix was not read", versions)
	}
	lo, hi, ok := docPostgresRange(readRepoFile(t, archDocPath))
	if !ok {
		t.Fatal("docs/architecture.md states no \"PostgreSQL A-B\" range")
	}
	if lo != versions[0] || hi != versions[len(versions)-1] {
		t.Fatalf("docs/architecture.md says PostgreSQL %d-%d, CI tests %v", lo, hi, versions)
	}
}

func TestArchitectureDocs_RangeAndMatrixParsing(t *testing.T) {
	yml := "    image: ghcr.io/x/pg_sage-ci-pgvector:0.8.2-pg17\n" +
		"      matrix:\n        pg: [\"14\", \"15\", \"16\", \"18\"]\n" +
		"  other:\n    image: ghcr.io/x/pg_sage-ci-pgvector:0.8.2-pg17\n"
	got := ciPostgresVersions(yml)
	if len(got) != 5 || got[0] != 14 || got[4] != 18 || got[3] != 17 {
		t.Fatalf("versions %v, want [14 15 16 17 18]", got)
	}
	if got := ciPostgresVersions(""); len(got) != 0 {
		t.Fatalf("empty workflow gave %v", got)
	}
	cases := []struct {
		doc    string
		lo, hi int
		ok     bool
	}{
		{"connects to any PostgreSQL 14-18 over", 14, 18, true},
		{"PostgreSQL 14-17, later PostgreSQL 9-10", 14, 17, true},
		{"PostgreSQL 18-14", 0, 0, false},
		{"PostgreSQL 14 to 18", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		lo, hi, ok := docPostgresRange(c.doc)
		if ok != c.ok || (ok && (lo != c.lo || hi != c.hi)) {
			t.Errorf("%q: got %d-%d ok=%v, want %d-%d ok=%v", c.doc, lo, hi, ok,
				c.lo, c.hi, c.ok)
		}
	}
}

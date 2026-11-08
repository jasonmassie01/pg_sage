package mutationcensus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No database, concurrency or state-transition tests: the census is a
// static scan of source files and a text allowlist; it opens no connection
// and holds no shared state.

func sidecarRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("sidecar root %s has no go.mod: %v", root, err)
	}
	return root
}

func treeSinks(t *testing.T) []sink {
	t.Helper()
	found, err := scanRoots(sidecarRoot(t), []string{"internal", "cmd"})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func committedSinks(t *testing.T) []entry {
	t.Helper()
	f, err := os.Open("sinks.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := parseSinks(f)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func fixtureSinks(t *testing.T, name string) map[string]sink {
	t.Helper()
	found, err := scanRoots(filepath.Join("testdata", "census", name), []string{"internal"})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]sink{}
	for _, s := range found {
		out[s.key+"/"+s.rule] = s
	}
	return out
}

// CHECK-10 (tree half): every write sink of the shipped tree is listed in
// sinks.tsv with a valid class, and no row is stale.
func TestEveryWriteSinkIsClassified(t *testing.T) {
	problems := compareCensus(treeSinks(t), committedSinks(t))
	for _, p := range problems {
		t.Error(p)
	}
}

// CHECK-11: every gate: row names contracts registered in internal/executor.
func TestGateRowsNameRegisteredContracts(t *testing.T) {
	gates := 0
	for _, e := range committedSinks(t) {
		if !strings.HasPrefix(e.class, "gate:") {
			continue
		}
		gates++
		if err := checkClass(e.class); err != nil {
			t.Errorf("line %d %s: %v", e.line, e.key, err)
		}
	}
	if gates == 0 {
		t.Fatal("sinks.tsv has no gate: row; the census lost its gated sinks")
	}
}

func TestEmptyAllowlistReportsTheTreeSinks(t *testing.T) {
	found := treeSinks(t)
	problems := compareCensus(found, nil)
	if len(found) < 10 || len(problems) != len(found) {
		t.Fatalf("empty allowlist: %d sinks, %d problems; want >= 10 sinks, each "+
			"reported (a scan matching nothing would pass every check)", len(found),
			len(problems))
	}
	rules := map[string]int{}
	for _, s := range found {
		rules[s.rule]++
	}
	if rules["A"] == 0 || rules["B"] == 0 {
		t.Fatalf("rules found = %v, want both A and B on the real tree", rules)
	}
}

func TestRemovingMigrationApplyRowFailsAndNamesIt(t *testing.T) {
	const key = "internal/migration/runtime/postgres.go#PostgresApplier.Apply"
	var kept []entry
	removed := false
	for _, e := range committedSinks(t) {
		if e.key == key {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		t.Fatalf("sinks.tsv has no %s row", key)
	}
	problems := compareCensus(treeSinks(t), kept)
	if len(problems) != 1 || !strings.Contains(problems[0], key) ||
		!strings.Contains(problems[0], "rule B") ||
		!strings.Contains(problems[0], "executor.ExecConcurrently") {
		t.Fatalf("problems = %q, want exactly one naming %s under rule B", problems, key)
	}
}

func TestStaleRowAndUnknownClassFail(t *testing.T) {
	entries := append(committedSinks(t),
		entry{key: "internal/fix/gone.go#Gone", class: "sage_schema", note: "x", line: 900},
		entry{key: "internal/migration/runtime/postgres.go#Other", class: "trusted",
			note: "x", line: 901})
	problems := strings.Join(compareCensus(treeSinks(t), entries), "\n")
	for _, want := range []string{"stale row sinks.tsv line 900: internal/fix/gone.go#Gone",
		`line 901 (internal/migration/runtime/postgres.go#Other): unknown class "trusted"`} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems lack %q:\n%s", want, problems)
		}
	}
}

func TestUnclassifiedFixtureIsReported(t *testing.T) {
	found := fixtureSinks(t, "unclassified")
	s, ok := found["internal/fix/purge.go#Purge/A"]
	if len(found) != 1 || !ok || s.detail != "DELETE FROM public.t" {
		t.Fatalf("fixture sinks = %v, want only Purge (rule A, DELETE FROM public.t)", found)
	}
	problems := compareCensus([]sink{s}, nil)
	if len(problems) != 1 || !strings.Contains(problems[0],
		"unclassified sink internal/fix/purge.go#Purge") {
		t.Fatalf("problems = %q", problems)
	}
}

func TestBoundaryFixture(t *testing.T) {
	found := fixtureSinks(t, "boundary")
	want := map[string]string{
		"DropChild":     "DROP TABLE ?_p1",
		"FormatVacuum":  "VACUUM (ANALYZE) ?",
		"RangeMixed":    "TRUNCATE public.b",
		"CopyUser":      "CopyFrom",
		"Batched":       "DELETE FROM public.q WHERE id = $1",
		"QuotedHypo":    `SELECT * FROM ?."hypopg_create_index"($1)`,
		"Hypo":          "SELECT * FROM hypopg_create_index($1)",
		"WriteFromSage": "INSERT INTO public.t SELECT * FROM sage.snap",
		"CTEDelete":     "DELETE FROM public.t RETURNING id) SELECT count(*) FROM gone",
		"Wrapped":       "TRUNCATE public.t",
		"Built":         "ALTER ROLE ? NOLOGIN",
		"RequestCtx":    "DELETE FROM public.r",
		"ShortCtx":      "DROP TABLE public.c",
		"MultiDrop":     "DROP TABLE IF EXISTS sage.a, public.b",
	}
	for fn, detail := range want {
		s, ok := found["internal/fix/boundary.go#"+fn+"/A"]
		if !ok || !strings.HasPrefix(detail, s.detail) {
			t.Errorf("%s: sink = %+v (found %v), want rule A %q", fn, s, ok, detail)
		}
	}
	if len(found) != len(want) {
		t.Errorf("fixture sinks = %v, want exactly %d (Columns, Comments, Explain, "+
			"SageOnly, SageBuilt, CopySage, ParamText, ReadIntoSage and BindAfter "+
			"are not sinks)", found, len(want))
	}
}

func TestRuleBFixture(t *testing.T) {
	found := fixtureSinks(t, "ruleb")
	apply, ok := found["internal/fix/apply.go#Applier.Apply/B"]
	if !ok || apply.detail != "executor.ExecInTransaction" {
		t.Errorf("Applier.Apply = %+v, %v; want rule B executor.ExecInTransaction", apply, ok)
	}
	pick, ok := found["internal/fix/apply.go#Pick/B"]
	if !ok || pick.detail != "executor.ExecConcurrently" {
		t.Errorf("Pick = %+v, %v; want rule B executor.ExecConcurrently", pick, ok)
	}
	if len(found) != 2 {
		t.Errorf("fixture sinks = %v, want exactly 2 (internal/executor is the gate)", found)
	}
}

func TestScanErrorsNameTheirCause(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "fix")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bad := []byte("package fix\n\nfunc Broken( {\n")
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := scanRoots(root, []string{"internal"})
	if err == nil || !strings.Contains(err.Error(), "parse internal/fix/bad.go") {
		t.Fatalf("broken source: err = %v, want it to name internal/fix/bad.go", err)
	}
	_, err = scanRoots(filepath.Join("testdata", "census"), []string{"no_such_dir"})
	if err == nil || !strings.Contains(err.Error(), "scan no_such_dir") {
		t.Fatalf("missing root: err = %v, want it to name no_such_dir", err)
	}
}

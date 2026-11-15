package testdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ci-23 guard: the tests that measure state every other package moves on
// the shared server (the xmin horizon an emergency freeze needs, the
// pg_stat_statements rows a collection reads) hold the exclusive server
// (testdb.Exclusive, directly or through a package helper) and never take
// the shared server's fixture helper as well.
//
// No concurrent-access tests: the guard parses files and keeps no shared
// state. No state transitions: it is a pure scan.

// exclusiveMeasurer names one test that must hold the exclusive server,
// and the shared-server helpers it must not call itself.
type exclusiveMeasurer struct {
	dir, test string
	shared    []string
}

// exclusiveMeasurers are the measuring tests ci-13 and ci-23 moved.
var exclusiveMeasurers = []exclusiveMeasurer{
	{"internal/autonomy", "TestFreezeEmergencyProfileLifecycleOnRealPostgres",
		[]string{"requireAutonomyDB"}},
	{"internal/collector", "TestReadQueryCounters_OwnIDsLeftOutOfMeanAndIO",
		[]string{"textTestDB", "testPool"}},
	{"internal/collector", "TestCollect_TenSteadyCyclesReadTextsAtMostOnce",
		[]string{"textTestDB", "testPool"}},
	{"internal/collector", "TestCollectQueries_MatchesLegacySingleRead",
		[]string{"textTestDB", "testPool"}},
	{"internal/collector", "TestPreflightEvidenceResetAfterRegrowth",
		[]string{"testPool"}},
}

// measurerProblems reports each measurer that is missing, does not reach
// testdb.Exclusive, or calls one of its shared-server helpers directly.
func measurerProblems(idx *resetLintIndex, measurers []exclusiveMeasurer) []string {
	var out []string
	for _, m := range measurers {
		f := idx.function(m.dir, m.test)
		switch {
		case f == nil:
			out = append(out, m.dir+"."+m.test+": not found")
		case !f.exclusive:
			out = append(out, m.dir+"."+m.test+": does not hold the exclusive server")
		default:
			for _, name := range idx.directCalls(f) {
				for _, shared := range m.shared {
					if name == shared {
						out = append(out, m.dir+"."+m.test+": also calls the shared-server "+
							"helper "+shared)
					}
				}
			}
		}
	}
	return out
}

// function returns the top-level (non-method) function name in dir.
func (idx *resetLintIndex) function(dir, name string) *lintFunc {
	for _, f := range idx.funcs {
		if f.dir == dir && f.name == name && !f.method {
			return f
		}
	}
	return nil
}

// directCalls lists the bare callee names f calls itself.
func (idx *resetLintIndex) directCalls(f *lintFunc) []string {
	var out []string
	for _, c := range idx.calls {
		if c.caller == f && c.ident != "" {
			out = append(out, c.ident)
		}
	}
	return out
}

func TestExclusiveMeasurersHoldTheExclusiveServer(t *testing.T) {
	root, modulePath := sidecarModule(t)
	idx, err := indexResetLint(root, modulePath, moduleLintFiles)
	if err != nil {
		t.Fatalf("index the module's tests: %v", err)
	}
	if problems := measurerProblems(idx, exclusiveMeasurers); len(problems) != 0 {
		t.Fatalf("measuring tests on the shared server:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

const measurerFixture = `package a

import "example.com/resetlint/internal/testdb"

func held(t T) string { return testdb.Exclusive(t, "x") }
func shared(t T) string { return "" }

func TestHeld(t T)    { _ = held(t) }
func TestDirect(t T)  { _ = open(testdb.Exclusive(t, "x")) }
func TestShared(t T)  { _ = shared(t) }
func TestBoth(t T)    { _ = held(t); _ = shared(t) }
func (T) TestMethod() { _ = held(nil) }
`

// Happy path, invalid input and nil: the guard accepts a test that holds
// the server through a helper or directly, and reports one that does not,
// one that also takes the shared helper, a method of that name (not a
// test) and a missing test, each by name.
func TestMeasurerProblemsReportsEachWrongTest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a_test.go"), []byte(measurerFixture),
		0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	idx, err := indexResetLint(filepath.Dir(dir), resetLintFixture,
		func(string) bool { return true })
	if err != nil {
		t.Fatalf("index fixture: %v", err)
	}
	only := []string{"shared"}
	got := measurerProblems(idx, []exclusiveMeasurer{
		{"a", "TestHeld", only}, {"a", "TestDirect", only}, {"a", "TestShared", only},
		{"a", "TestBoth", only}, {"a", "TestMethod", only}, {"a", "TestGone", only},
		{"b", "TestHeld", only},
	})
	want := []string{
		"a.TestShared: does not hold the exclusive server",
		"a.TestBoth: also calls the shared-server helper shared",
		"a.TestMethod: not found",
		"a.TestGone: not found",
		"b.TestHeld: not found",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if empty := measurerProblems(idx, nil); len(empty) != 0 {
		t.Fatalf("no measurers gave problems %q, want none", empty)
	}
}

package assertlint

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// No concurrent access tests: Scan keeps all of its state in one linter
// value per call and shares nothing between calls.

// TestModuleTestsCanFail is the gate: every Test function in the module can
// fail, and only internal/testdb reads the test DSN. Existing offenders are
// listed in allowlist.txt, which may only shrink.
func TestModuleTestsCanFail(t *testing.T) {
	findings, err := Scan(moduleRoot(t), "github.com/pg-sage/sidecar")
	if err != nil {
		t.Fatalf("scan module: %v", err)
	}
	f, err := os.Open("allowlist.txt")
	if err != nil {
		t.Fatalf("open allowlist: %v", err)
	}
	defer f.Close()
	allow, err := ParseAllowlist(f)
	if err != nil {
		t.Fatalf("parse allowlist.txt: %v", err)
	}
	unlisted, stale := Compare(findings, allow)
	for _, u := range unlisted {
		t.Errorf("%s: a %s finding; give the test an assertion (or read the DSN through "+
			"internal/testdb), do not add it to allowlist.txt", u, u.Rule)
	}
	for _, s := range stale {
		t.Errorf("stale allowlist.txt entry %q: it no longer matches; delete the line", s)
	}
	t.Logf("%d findings, %d allowlisted", len(findings), len(allow))
}

func TestScanFlagsHollowTests(t *testing.T) {
	root := filepath.Join("testdata", "basic")
	findings, err := Scan(root, "example.com/fix")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	src := filepath.Join(root, "pkg", "basic_test.go")
	var want []Finding
	for _, name := range []string{"TestNoCall", "TestEmpty", "TestErrString",
		"TestSubtestHollow", "TestQuietHelper", "TestExternalQuiet",
		"TestGoroutineNoWait", "TestProductionMethodClash", "TestLocalShadowsHelper",
		"TestFieldNamedLikeHelper"} {
		want = append(want, Finding{Rule: RuleNoAssert, Path: "pkg/basic_test.go",
			Line: lineOf(t, src, "func "+name+"("), Func: name})
	}
	slices.SortFunc(want, func(a, b Finding) int { return a.Line - b.Line })
	if !slices.Equal(findings, want) {
		t.Fatalf("findings:\n%s\nwant:\n%s", list(findings), list(want))
	}
}

func TestScanAcceptsEveryFailingForm(t *testing.T) {
	findings, err := Scan(filepath.Join("testdata", "basic"), "example.com/fix")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	flagged := map[string]bool{}
	for _, f := range findings {
		flagged[f.Func] = true
	}
	for _, name := range []string{"TestFatal", "TestRequire", "TestSubtestFails",
		"TestSubtestHelperValue", "TestFailingHelperChain", "TestExternalFails",
		"TestFieldReceiver", "TestRaceWithWait", "TestMain", "Testlower"} {
		if flagged[name] {
			t.Errorf("%s was flagged; it can fail (or is not a test)", name)
		}
	}
}

func TestScanFlagsDSNReadsOutsideTestdb(t *testing.T) {
	root := filepath.Join("testdata", "getenv")
	findings, err := Scan(root, "example.com/fix")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var got []string
	for _, f := range findings {
		if f.Rule == RuleGetenv {
			got = append(got, f.String())
		}
	}
	store := filepath.Join(root, "internal", "store", "store_test.go")
	x := filepath.Join(root, "internal", "testdbx", "x_test.go")
	queue := filepath.Join(root, "internal", "queue", "queue_test.go")
	want := []string{
		Finding{RuleGetenv, "internal/queue/queue_test.go",
			lineOf(t, queue, "os.Getenv(tdb.EnvName)"), "openPool"}.String(),
		Finding{RuleGetenv, "internal/store/store_test.go",
			lineOf(t, store, "var pkgDSN"), "<package>"}.String(),
		Finding{RuleGetenv, "internal/store/store_test.go",
			lineOf(t, store, "return os.Getenv"), "dsn"}.String(),
		Finding{RuleGetenv, "internal/store/store_test.go",
			lineOf(t, store, "os.LookupEnv"), "TestStore"}.String(),
		Finding{RuleGetenv, "internal/testdbx/x_test.go",
			lineOf(t, x, "os.Getenv"), "TestX"}.String(),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("getenv findings:\n%s\nwant:\n%s", strings.Join(got, "\n"),
			strings.Join(want, "\n"))
	}
}

func TestScanPackageWithoutTestsIsClean(t *testing.T) {
	findings, err := Scan(filepath.Join("testdata", "notests"), "example.com/fix")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings in a package with no tests:\n%s", list(findings))
	}
}

func TestScanErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := Scan(missing, "m"); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Errorf("scan of a missing root: err = %v, want one naming the path", err)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad_test.go")
	if err := os.WriteFile(bad, []byte("package x\nfunc TestX(t *testing.T) {"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Scan(dir, "m")
	if err == nil || !strings.Contains(err.Error(), "bad_test.go") {
		t.Errorf("scan of a broken file: err = %v, want one naming bad_test.go", err)
	}
}

func TestScanEmptyRoot(t *testing.T) {
	findings, err := Scan(t.TempDir(), "m")
	if err != nil || len(findings) != 0 {
		t.Fatalf("empty root: findings=%v err=%v, want none", findings, err)
	}
}

func TestCompareReportsStaleAndUnlisted(t *testing.T) {
	findings, err := Scan(filepath.Join("testdata", "basic"), "example.com/fix")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	allow := []string{
		"noassert pkg/basic_test.go TestEmpty",
		// TestFatal asserts: an entry for it is stale.
		"noassert pkg/basic_test.go TestFatal",
	}
	unlisted, stale := Compare(findings, allow)
	if !slices.Equal(stale, []string{"noassert pkg/basic_test.go TestFatal"}) {
		t.Errorf("stale = %q, want only the TestFatal entry", stale)
	}
	if len(unlisted) != len(findings)-1 {
		t.Errorf("unlisted = %d, want %d (all but TestEmpty)", len(unlisted), len(findings)-1)
	}
	for _, u := range unlisted {
		if u.Func == "TestEmpty" {
			t.Errorf("allowlisted TestEmpty reported as unlisted")
		}
	}
	if u, s := Compare(nil, nil); u != nil || s != nil {
		t.Errorf("Compare(nil, nil) = %v, %v; want nothing", u, s)
	}
}

func TestParseAllowlist(t *testing.T) {
	keys, err := ParseAllowlist(strings.NewReader(
		"# comment\n\nnoassert a_test.go TestA\n  noassert b_test.go TestB  \n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"noassert a_test.go TestA", "noassert b_test.go TestB"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %q, want %q", keys, want)
	}
	for _, tc := range []struct{ in, want string }{
		{"noassert a_test.go", "line 1"},
		{"noassert a_test.go TestA extra", "line 1"},
		{"\nbogus a_test.go TestA", "line 2: unknown rule \"bogus\""},
		{"noassert a TestA\nnoassert a TestA", "line 2: duplicate of line 1"},
		// Every direct DSN read is converted (ci-11): a new one is fixed, never listed.
		{"getenv b_test.go helper", "line 1: a getenv finding cannot be allowlisted"},
	} {
		_, err := ParseAllowlist(strings.NewReader(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseAllowlist(%q) err = %v, want it to contain %q", tc.in, err, tc.want)
		}
	}
}

func TestFindingKeyOmitsLine(t *testing.T) {
	a := Finding{RuleNoAssert, "p/x_test.go", 10, "TestX"}
	b := a
	b.Line = 99
	if a.Key() != b.Key() || a.Key() != "noassert p/x_test.go TestX" {
		t.Fatalf("keys %q and %q, want both %q", a.Key(), b.Key(), "noassert p/x_test.go TestX")
	}
	if got := a.String(); got != "p/x_test.go:10: noassert TestX" {
		t.Fatalf("String() = %q", got)
	}
}

func lineOf(t *testing.T, path, needle string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.Contains(sc.Text(), needle) {
			return n
		}
	}
	t.Fatalf("%q not found in %s", needle, path)
	return 0
}

func list(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  " + f.String() + "\n")
	}
	return b.String()
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

package testdb

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ci-13 guard: no test resets or disturbs the shared server's cluster-wide
// state. A zero-argument pg_stat_statements_reset(), pg_stat_reset_shared(
// or an executed CHECKPOINT runs only where testdb.Exclusive holds the
// exclusive server.
//
// No concurrent-access tests: the lint reads files and keeps no shared
// state. No state transitions: it is a pure scan.

// resetLintFixture is the module path the fixture directory scans as.
const resetLintFixture = "example.com/resetlint"

func sidecarModule(t *testing.T) (root, modulePath string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	first, _, _ := strings.Cut(string(body), "\n")
	modulePath = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(first), "module"))
	if modulePath == "" {
		t.Fatalf("go.mod has no module line: %q", first)
	}
	return root, modulePath
}

// moduleLintFiles is what the guard covers: every test file, and the
// shared test helpers under internal/testsupport (not _test.go files).
func moduleLintFiles(rel string) bool {
	return strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "internal/testsupport/")
}

func resetLintReport(findings []resetFinding) string {
	var b strings.Builder
	b.WriteString("cluster-wide statistics resets or checkpoints outside testdb.Exclusive:\n")
	for _, f := range findings {
		b.WriteString("  " + f.String() + "\n")
	}
	return b.String()
}

func TestResetLint(t *testing.T) {
	root, modulePath := sidecarModule(t)
	findings, err := scanResetLint(root, modulePath, moduleLintFiles)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(findings) > 0 {
		t.Fatal(resetLintReport(findings))
	}
}

// wantedLines returns "file:line" for every line of the fixture directory
// marked with a trailing "// want".
func wantedLines(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		for line := 1; scanner.Scan(); line++ {
			if strings.HasSuffix(strings.TrimSpace(scanner.Text()), "// want") {
				out = append(out, filepath.ToSlash(rel)+":"+strconv.Itoa(line))
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sort.Strings(out)
	return out
}

// Boundary and error propagation: the fixture's flagged lines are reported
// exactly (the zero-argument reset in every spelling, pg_stat_reset_shared,
// an executed CHECKPOINT, a helper a shared-server test reaches), and its
// allowed lines are not (pg_stat_reset(), the three-argument reset,
// CHECKPOINT as data or prose, anything an Exclusive test vouches for).
func TestResetLintFixtureReportsEachOffendingLine(t *testing.T) {
	root := filepath.Join("testdata", "resetlint")
	findings, err := scanResetLint(root, resetLintFixture, func(string) bool { return true })
	if err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.file+":"+strconv.Itoa(f.line))
		if f.what == "" {
			t.Errorf("%s:%d has no reason", f.file, f.line)
		}
	}
	want := wantedLines(t, root)
	if len(want) < 10 {
		t.Fatalf("fixture marks %d lines, want at least 10", len(want))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("reported lines:\n%s\nwant:\n%s", strings.Join(got, "\n"),
			strings.Join(want, "\n"))
	}
	report := resetLintReport(findings)
	for _, w := range want {
		if !strings.Contains(report, "  "+w+": ") {
			t.Errorf("failure report lacks %s:\n%s", w, report)
		}
	}
}

// Nil/empty: an empty tree has no findings; a missing one is an error.
func TestResetLintEmptyAndMissingTrees(t *testing.T) {
	findings, err := scanResetLint(t.TempDir(), resetLintFixture,
		func(string) bool { return true })
	if err != nil || len(findings) != 0 {
		t.Fatalf("empty tree: %v, %v; want no findings and no error", findings, err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := scanResetLint(missing, resetLintFixture,
		func(string) bool { return true }); err == nil {
		t.Fatal("a missing tree scanned without error")
	}
}

// Invalid input: a file that does not parse fails the scan with its name.
func TestResetLintUnparseableFileFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken_test.go"),
		[]byte("package broken\nfunc {"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := scanResetLint(dir, resetLintFixture, func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "broken_test.go") {
		t.Fatalf("scan error = %v, want one naming broken_test.go", err)
	}
}

func TestModuleLintFilesCoversTestsAndTestsupport(t *testing.T) {
	for rel, want := range map[string]bool{
		"internal/verify/write_rate_db_test.go":   true,
		"internal/testsupport/perfgate/settle.go": true,
		"e2e/pipeline_binary_test.go":             true,
		"internal/collector/collector.go":         false,
		"sre-bench/scenarios_ckpt.go":             false,
	} {
		if got := moduleLintFiles(rel); got != want {
			t.Errorf("moduleLintFiles(%q) = %v, want %v", rel, got, want)
		}
	}
}

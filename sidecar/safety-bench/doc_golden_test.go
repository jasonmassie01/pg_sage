package safetybench

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// No concurrent access tests: these read checked-in files only.

// benchDocPath is the published AgentSafetyBench write-up, relative to this
// package directory.
var benchDocPath = filepath.Join("..", "..", "docs", "agent-safety-bench.md")

// stalePhrases are claims the bench no longer makes: the privilege design
// logs in as its own role, and there are four designs. They are split so
// this file does not trip its own guard.
var stalePhrases = []string{"SET LOCAL " + "ROLE", "three " + "read-only designs"}

func readBenchDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(benchDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", benchDocPath, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// between returns the text between <!-- name:begin --> and <!-- name:end -->.
func between(t *testing.T, doc, name string) string {
	t.Helper()
	begin, end := "<!-- "+name+":begin -->", "<!-- "+name+":end -->"
	i := strings.Index(doc, begin)
	j := strings.Index(doc, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("%s: no %s ... %s block", benchDocPath, begin, end)
	}
	return doc[i+len(begin) : j]
}

func designNamesFor(t *testing.T) []string {
	t.Helper()
	designs := Designs(readOnlyRole)
	defer closeDesigns(designs)
	return designNames(designs)
}

var docDesignItem = regexp.MustCompile("(?m)^- \\*\\*`([a-z_]+)`\\*\\*")

// The design list in the doc is the list Designs returns, in order.
func TestDocDesignListMatchesDesigns(t *testing.T) {
	block := between(t, readBenchDoc(t), "designs")
	var got []string
	for _, m := range docDesignItem.FindAllStringSubmatch(block, -1) {
		got = append(got, m[1])
	}
	want := designNamesFor(t)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("doc designs = %v, want %v (Designs)", got, want)
	}
}

// The results table names every design once, in order, and each row's total
// is the number of corpus cases on disk.
func TestDocResultsCoverEveryCase(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "readonly", "*.json"))
	if err != nil {
		t.Fatalf("glob corpus: %v", err)
	}
	cases, err := LoadCases(false)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	if len(files) != 23 || len(cases) != len(files) {
		t.Fatalf("corpus: %d json files, %d loaded cases, want 23 of each",
			len(files), len(cases))
	}
	block := between(t, readBenchDoc(t), "results")
	var names []string
	for _, line := range strings.Split(block, "\n") {
		cols := strings.Split(line, "|")
		if len(cols) < 6 || !strings.HasPrefix(strings.TrimSpace(cols[1]), "`") {
			continue
		}
		name := strings.Trim(strings.TrimSpace(cols[1]), "`")
		names = append(names, name)
		total, err := strconv.Atoi(strings.TrimSpace(cols[4]))
		if err != nil || total != len(files) {
			t.Errorf("design %s: doc total %q, want %d", name, cols[4], len(files))
		}
	}
	want := designNamesFor(t)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("results table designs = %v, want %v", names, want)
	}
	if !strings.Contains(block, strconv.Itoa(len(IncidentMapping()))+" rows") {
		t.Errorf("results block does not state the %d incident rows", len(IncidentMapping()))
	}
}

// Neither the package nor the doc carries the retired claims.
func TestNoStaleDesignClaims(t *testing.T) {
	check := func(path string, raw []byte) {
		for _, p := range stalePhrases {
			if strings.Contains(string(raw), p) {
				t.Errorf("%s contains %q", path, p)
			}
		}
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		check(path, raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walk safety-bench: %v", err)
	}
	check(benchDocPath, []byte(readBenchDoc(t)))
}

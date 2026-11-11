package analyzer_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/executor"
)

// No concurrent access tests: the binding is a pure function of a finding.

// relationRules are the sources whose statement changes a relation; the
// others (signals, slot drops, role settings) name none.
var relationRules = map[string]bool{
	"rules_autovacuum_tuning.go": true, "rules_fk_index.go": true, "rules_index.go": true,
	"rules_stale_statistics.go": true, "rules_unused_index.go": true, "rules_vacuum.go": true,
	"rules_stale_statistics_parent.go": true, "rules_index_bloat.go": true,
	"optimizer_mapping.go": true,
}

// executor-08 (CHECK-03): every analyzer source that writes SQL yields a
// statement whose target is the object its finding names, so target
// binding parks no catalog finding.
func TestCatalogStatementTargetParity(t *testing.T) {
	catalog := analyzer.CatalogSQLFindings(t)
	for _, file := range sqlWritingSources(t) {
		findings := catalog[file]
		if len(findings) == 0 {
			t.Errorf("%s writes RecommendedSQL but CatalogSQLFindings has no finding "+
				"for it: add a fixture", file)
			continue
		}
		for _, f := range findings {
			checkParity(t, file, f)
		}
	}
}

func checkParity(t *testing.T, file string, f analyzer.Finding) {
	t.Helper()
	if f.RecommendedSQL == "" {
		t.Errorf("%s: fixture finding %q has no SQL", file, f.Title)
		return
	}
	binding, err := executor.BindFindingTarget(f)
	if err != nil {
		t.Errorf("%s: %q on %q: %v", file, f.RecommendedSQL, f.ObjectIdentifier, err)
		return
	}
	if relationRules[file] && binding.Statement == "" {
		t.Errorf("%s: %q names no relation; want the finding's %q", file,
			f.RecommendedSQL, f.ObjectIdentifier)
	}
	if !relationRules[file] && binding.Statement != "" {
		t.Errorf("%s: %q unexpectedly names relation %q", file, f.RecommendedSQL,
			binding.Statement)
	}
	if relationRules[file] && binding.Named == "" {
		t.Errorf("%s: finding %q names no object to bind", file, f.Title)
	}
}

var recommendedSQLWrite = regexp.MustCompile(`RecommendedSQL\s*(:|=[^=])`)

// sqlWritingSources lists the analyzer sources that set RecommendedSQL on
// a finding they build (the rules and the optimizer mapping).
func sqlWritingSources(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	var files []string
	for _, path := range paths {
		matched, _ := filepath.Match("*_test.go", path)
		isSource := path == "optimizer_mapping.go" || strings.HasPrefix(path, "rules_")
		if matched || !isSource {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if recommendedSQLWrite.Match(body) {
			files = append(files, path)
		}
	}
	sort.Strings(files)
	if len(files) < len(relationRules) {
		t.Fatalf("found %d SQL-writing sources %v, want at least %d", len(files), files,
			len(relationRules))
	}
	return files
}

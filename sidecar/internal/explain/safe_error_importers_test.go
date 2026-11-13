package explain

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// security-10: every non-test file that imports this package either sends
// its errors through SafeError before they can reach an agent or a model,
// or is listed here with the reason it does not need to.
var safeErrorAllowlist = map[string]string{
	"internal/agentguard/broker/broker.go": "the broker withholds data exceptions " +
		"itself (dbFailure, sanitize); its operator log keeps the full text",
	"internal/agentguard/broker/exec.go": "the broker withholds data exceptions " +
		"itself (dbFailure, sanitize); its operator log keeps the full text",
	"internal/api/handlers_v09.go": "the dashboard route sends non-validation " +
		"errors through internalError, which never returns server text",
	"internal/rca/tier2_sql.go": "ProveRead errors reach only the operator log; " +
		"the incident chain note is fixed text, so no server text reaches a prompt",
}

const explainImportPath = "github.com/pg-sage/sidecar/internal/explain"

// No concurrent access tests: the walk reads source files only.

func TestSafeError_EveryImporterWithholdsOrIsAllowlisted(t *testing.T) {
	importers := explainImporters(t, filepath.Join("..", ".."))
	if len(importers) == 0 {
		t.Fatal("found no importer of internal/explain; the walk is broken")
	}
	seen := map[string]bool{}
	for _, rel := range importers {
		seen[rel] = true
		if reason, ok := safeErrorAllowlist[rel]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s: allowlisted without a reason", rel)
			}
			continue
		}
		src, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(src), "explain.SafeError(") {
			t.Errorf("%s imports internal/explain but never calls explain.SafeError; "+
				"withhold server text that can quote values, or allowlist it with a reason",
				rel)
		}
	}
	for rel := range safeErrorAllowlist {
		if !seen[rel] {
			t.Errorf("allowlist entry %s no longer imports internal/explain; remove it", rel)
		}
	}
}

// explainImporters lists the module's non-test Go files (any build tag)
// that import this package, as slash paths relative to root.
func explainImporters(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "web", "node_modules", "testdata", "vendor", "dist", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/explain/") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(src), explainImportPath) {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == explainImportPath {
				out = append(out, rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

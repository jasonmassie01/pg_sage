package verify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// No concurrent access, database or invalid-input tests: this is a
// static check of the package sources.

// The query store's window rules (anchor and epoch, R10) live in one place,
// internal/querystore. verify and the SLO latency proxy call its readers,
// so no SQL string in either package may name sage.query_store.
func TestQueryStoreSQLHasOneSource(t *testing.T) {
	var files []string
	for _, dir := range []string{".", filepath.Join("..", "sre", "slo")} {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range matches {
			if !strings.HasSuffix(f, "_test.go") {
				files = append(files, f)
			}
		}
	}
	if len(files) < 10 {
		t.Fatalf("found only %d source files; the scan is looking in the wrong place", len(files))
	}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "sage.query_store") {
				t.Errorf("%s: SQL naming sage.query_store; use the querystore readers",
					fset.Position(lit.Pos()))
			}
			return true
		})
	}
}

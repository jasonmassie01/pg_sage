package schemaguard

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dead-code guard (migration-09): every InvariantKind constant has a
// non-test producer, a composite literal outside tests that sets Kind to it.
// A kind nothing detects only feeds planner branches nobody exercises (the
// removed redundant_index kind was wired to the FK-index change class).
func TestEveryInvariantKindHasANonTestProducer(t *testing.T) {
	kinds := invariantKindConstants(t, "types.go")
	if len(kinds) < 4 {
		t.Fatalf("found %d InvariantKind constants in types.go: %v", len(kinds), kinds)
	}
	sidecar := filepath.Join("..", "..")
	produced := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		collectProducedKinds(t, filepath.Join(sidecar, dir), produced)
	}
	for _, kind := range kinds {
		if !produced[kind] {
			t.Errorf("%s has no non-test producer (no Invariant{Kind: schemaguard.%s} "+
				"outside tests): delete it or add its detector", kind, kind)
		}
	}
}

// invariantKindConstants lists the constants declared with type
// InvariantKind in one file of this package.
func invariantKindConstants(t *testing.T, file string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var names []string
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			if ident, ok := value.Type.(*ast.Ident); ok && ident.Name == "InvariantKind" {
				for _, name := range value.Names {
					names = append(names, name.Name)
				}
			}
		}
	}
	return names
}

// collectProducedKinds marks every schemaguard.Invariant* value used as a
// Kind field in a composite literal of a non-test Go file under root.
func collectProducedKinds(t *testing.T, root string, produced map[string]bool) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		return markProducedKinds(path, produced)
	})
	if err != nil {
		t.Fatalf("scan %s for invariant producers: %v", root, err)
	}
}

func markProducedKinds(path string, produced map[string]bool) error {
	source, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Contains(source, []byte("schemaguard.Invariant")) {
		return nil
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
	if err != nil {
		return err
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		field, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := field.Key.(*ast.Ident)
		value, isSelector := field.Value.(*ast.SelectorExpr)
		if !ok || key.Name != "Kind" || !isSelector {
			return true
		}
		if pkg, ok := value.X.(*ast.Ident); ok && pkg.Name == "schemaguard" &&
			strings.HasPrefix(value.Sel.Name, "Invariant") {
			produced[value.Sel.Name] = true
		}
		return true
	})
	return nil
}

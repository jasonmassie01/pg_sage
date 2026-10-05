package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// productionFunction returns the declaration of a function in this package's
// non-test sources, wherever the file split put it. Methods are named
// "Receiver.method"; plain names match only functions without a receiver.
// The name must match exactly one declaration.
func productionFunction(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	matches := productionDeclarations(t, name)
	if len(matches) != 1 {
		files := make([]string, 0, len(matches))
		for _, match := range matches {
			files = append(files, match.file)
		}
		t.Fatalf("function %s: want 1 production declaration, found %d %v",
			name, len(matches), files)
	}
	return matches[0].fn
}

type productionDeclaration struct {
	file string
	fn   *ast.FuncDecl
}

// productionDeclarations returns every non-test declaration named name.
func productionDeclarations(t *testing.T, name string) []productionDeclaration {
	t.Helper()
	var matches []productionDeclaration
	for _, path := range productionSources(t) {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range parsed.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if ok && qualifiedFuncName(fn) == name {
				matches = append(matches, productionDeclaration{
					file: filepath.Base(path), fn: fn,
				})
			}
		}
	}
	return matches
}

// productionSources lists the package's .go files, excluding tests.
func productionSources(t *testing.T) []string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source directory")
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(current), "*.go"))
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	sources := paths[:0]
	for _, path := range paths {
		if !strings.HasSuffix(path, "_test.go") {
			sources = append(sources, path)
		}
	}
	if len(sources) == 0 {
		t.Fatalf("no production sources in %s", filepath.Dir(current))
	}
	return sources
}

// qualifiedFuncName is "Receiver.method" for methods and the bare name for
// functions.
func qualifiedFuncName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	if ident, ok := receiver.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}
	return "?." + fn.Name.Name
}

// The lookup must not depend on file names, and a bare name must never
// resolve to a method: three types in this package declare start.
func TestProductionFunctionResolvesAcrossFiles(t *testing.T) {
	if got := productionDeclarations(t, "start"); len(got) != 0 {
		t.Fatalf("bare start matched %d methods", len(got))
	}
	if got := productionDeclarations(t, "noSuchProductionFunction"); len(got) != 0 {
		t.Fatalf("unknown name matched %d declarations", len(got))
	}
	tests := []struct {
		name     string
		receiver bool
		params   int
	}{
		{name: "initStandalone", params: 0},
		{name: "startAPIServer", params: 1},
		{name: "databaseRuntime.start", receiver: true, params: 1},
		{name: "sreSignals.start", receiver: true, params: 2},
		{name: "fleetBootstrap.start", receiver: true, params: 1},
		{name: "dbBudget.CanSpend", receiver: true, params: 1},
	}
	for _, tt := range tests {
		fn := productionFunction(t, tt.name)
		if got := qualifiedFuncName(fn); got != tt.name {
			t.Errorf("%s: resolved %s", tt.name, got)
		}
		if (fn.Recv != nil) != tt.receiver {
			t.Errorf("%s: receiver = %v, want %v", tt.name, fn.Recv != nil, tt.receiver)
		}
		if params := fn.Type.Params.NumFields(); params != tt.params {
			t.Errorf("%s: %d params, want %d", tt.name, params, tt.params)
		}
	}
}

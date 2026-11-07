package main

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// alerting-02: the signature alone does not stop a caller from passing ""
// and silently dropping the database name from every fleet alert, so the
// one call site must hand over the runtime's own spec name.
func TestInstanceAlertManagerCallerPassesSpecName(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "database_runtime_monitor.go", nil, 0)
	if err != nil {
		t.Fatalf("parse database_runtime_monitor.go: %v", err)
	}
	var args [][]string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "newInstanceAlertManager" {
			args = append(args, renderArgs(t, fset, call.Args))
		}
		return true
	})
	if len(args) != 1 {
		t.Fatalf("found %d newInstanceAlertManager calls, want 1", len(args))
	}
	want := []string{"rt.spec.Pool", "rt.spec.Name"}
	if strings.Join(args[0], ",") != strings.Join(want, ",") {
		t.Fatalf("newInstanceAlertManager args = %v, want %v", args[0], want)
	}
}

func renderArgs(t *testing.T, fset *token.FileSet, exprs []ast.Expr) []string {
	t.Helper()
	out := make([]string, 0, len(exprs))
	for _, e := range exprs {
		var b strings.Builder
		if err := printer.Fprint(&b, fset, e); err != nil {
			t.Fatalf("render call argument: %v", err)
		}
		out = append(out, b.String())
	}
	return out
}

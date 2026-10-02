package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// executorDecls parses the package's production files.
func executorDecls(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string]*ast.FuncDecl{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				decls[fn.Name.Name] = fn
			}
		}
	}
	return decls
}

func callsMethod(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if selector, isSel := callFunExpr(call).(*ast.SelectorExpr); ok && isSel &&
			selector.Sel.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func callFunExpr(call *ast.CallExpr) ast.Expr {
	if call == nil {
		return nil
	}
	return call.Fun
}

// Every execution entry point hands its change to Apply (G4-I07).
func TestEveryExecutionEntryPointCallsApply(t *testing.T) {
	decls := executorDecls(t)
	for _, entry := range []string{
		"processFinding", "ExecuteManual", "SubmitCustodianProposal",
		"SubmitVerifiedIndexProposal", "ExecuteRetention",
	} {
		fn := decls[entry]
		if fn == nil {
			t.Fatalf("entry point %s not found", entry)
		}
		if !callsMethod(fn, "Apply") {
			t.Errorf("%s does not route through Apply", entry)
		}
	}
	// RunCycle acts on durable recommendations (C07): each candidate goes
	// through processFinding (the gate) or, for an operator approval,
	// ExecuteManual; both are Apply entry points above.
	for _, edge := range [][2]string{
		{"RunCycle", "processCandidate"},
		{"processCandidate", "processFinding"},
		{"processCandidate", "runOperatorApproval"},
		{"runOperatorApproval", "ExecuteManual"},
	} {
		if !callsMethod(decls[edge[0]], edge[1]) {
			t.Errorf("%s does not call %s", edge[0], edge[1])
		}
	}
}

// The finding body runs only as an intent's Execute step, never directly.
// (The test-only executeFinding helper also goes through Apply.)
func TestFindingBodyRunsOnlyInsideAnIntent(t *testing.T) {
	calls := 0
	for name, fn := range executorDecls(t) {
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			pair, ok := node.(*ast.KeyValueExpr)
			if key, isKey := keyIdent(pair); ok && isKey && key == "Execute" {
				calls += countMethodCalls(pair.Value, "runAuthorizedFinding")
				return false
			}
			if call, isCall := node.(*ast.CallExpr); isCall {
				if selector, isSel := call.Fun.(*ast.SelectorExpr); isSel &&
					selector.Sel.Name == "runAuthorizedFinding" {
					t.Errorf("%s calls runAuthorizedFinding outside an intent's Execute", name)
				}
			}
			return true
		})
	}
	if calls != 2 {
		t.Fatalf("runAuthorizedFinding intents = %d, want the cycle and "+
			"verified-index intents", calls)
	}
}

func keyIdent(pair *ast.KeyValueExpr) (string, bool) {
	if pair == nil {
		return "", false
	}
	ident, ok := pair.Key.(*ast.Ident)
	if !ok {
		return "", false
	}
	return ident.Name, true
}

func countMethodCalls(node ast.Node, name string) int {
	count := 0
	ast.Inspect(node, func(child ast.Node) bool {
		call, ok := child.(*ast.CallExpr)
		if selector, isSel := callFunExpr(call).(*ast.SelectorExpr); ok && isSel &&
			selector.Sel.Name == name {
			count++
		}
		return true
	})
	return count
}

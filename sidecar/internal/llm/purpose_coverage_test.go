package llm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// llm-04: the usage ledger attributes spend by purpose, so every model call
// outside package llm must label its context: the enclosing function calls
// llm.WithPurpose or llm.WithDefaultPurpose. Manager.ChatForPurpose labels
// the call with its own purpose argument. An unlabelled call would be
// recorded as "unlabeled".
//
// No concurrent-access or database tests: this is a static walk of the
// source tree.

func TestPurposeCoverageEveryModelCallIsLabelled(t *testing.T) {
	_, calls := loadGuardTree(t, sidecarRoot)
	outside, missing := 0, map[string]bool{}
	for _, c := range calls {
		if c.pkg.isLLM {
			continue
		}
		outside++
		if !labelsPurpose(c.fn, c.name) {
			missing[c.file+":"+funcName(c.fn)] = true
		}
	}
	if outside < len(knownCallSites) {
		t.Fatalf("found %d model calls outside package llm, want >= %d: the walk "+
			"would pass vacuously", outside, len(knownCallSites))
	}
	if len(missing) > 0 {
		var sites []string
		for site := range missing {
			sites = append(sites, site)
		}
		t.Fatalf("%d model call sites without llm.WithPurpose in their function:\n%s",
			len(sites), strings.Join(sites, "\n"))
	}
}

// labelsPurpose reports whether fn labels the purpose of a call to method.
func labelsPurpose(fn *ast.FuncDecl, method string) bool {
	if method == "ChatForPurpose" {
		return true
	}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "llm" &&
			(sel.Sel.Name == "WithPurpose" || sel.Sel.Name == "WithDefaultPurpose") {
			found = true
		}
		return !found
	})
	return found
}

// The rule itself: a fixture function with an unlabelled Chat call fails,
// a labelled one passes.
func TestPurposeCoverageFixtures(t *testing.T) {
	src := `package p
func bad(ctx context.Context) { c.Chat(ctx, s, u, 1) }
func good(ctx context.Context) { c.Chat(llm.WithPurpose(ctx, "x"), s, u, 1) }
func local(ctx context.Context) { ctx = llm.WithDefaultPurpose(ctx, "x"); c.ChatWithTools(ctx) }
func other(ctx context.Context) { c.Chat(foo.WithPurpose(ctx, "x"), s, u, 1) }
func routed(ctx context.Context) { m.ChatForPurpose(ctx, "advisor", s, u, 1) }
`
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"bad": false, "good": true, "local": true, "other": false,
		"routed": true}
	for _, decl := range file.Decls {
		fn := decl.(*ast.FuncDecl)
		method := "Chat"
		if fn.Name.Name == "routed" {
			method = "ChatForPurpose"
		}
		if got := labelsPurpose(fn, method); got != want[fn.Name.Name] {
			t.Errorf("%s: labelsPurpose = %v, want %v", fn.Name.Name, got, want[fn.Name.Name])
		}
	}
}

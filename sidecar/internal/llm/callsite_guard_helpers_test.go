package llm

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// guardPromptArg is the argument index that carries the prompt (the system
// prompt, or the message list for ChatWithTools) of each model call.
var guardPromptArg = map[string]int{
	"Chat": 1, "ChatResult": 1, "ChatWithTools": 1, "ChatForPurpose": 2,
}

const (
	transportDirective   = "//llm:transport"
	noDBContentDirective = "//llm:no-db-content"
)

// guardPkg is one directory's non-test files: its top-level funcs, consts
// and vars by name, and its methods by "Type.Method" and ".Method".
type guardPkg struct {
	dir   string
	isLLM bool
	decls map[string][]ast.Node
}

type guardCall struct {
	pkg  *guardPkg
	file string
	fn   *ast.FuncDecl
	call *ast.CallExpr
	name string
}

// guardResult is the verdict for one call site (file:func): how its prompt
// is fenced, or why it is not.
type guardResult struct {
	site, verdict, problem string
}

// guardSites judges every model call in the non-test files under root's
// internal/ and cmd/ trees, sorted by site.
func guardSites(t *testing.T, root string) []guardResult {
	t.Helper()
	pkgs, calls := loadGuardTree(t, root)
	bySite := map[string]guardResult{}
	for _, c := range calls {
		r := judgeCall(pkgs, c)
		if prev, ok := bySite[r.site]; ok && prev.problem != "" {
			continue // the first problem in a function wins
		}
		bySite[r.site] = r
	}
	out := make([]guardResult, 0, len(bySite))
	for _, r := range bySite {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].site < out[j].site })
	return out
}

func loadGuardTree(t *testing.T, root string) (map[string]*guardPkg, []guardCall) {
	t.Helper()
	pkgs := map[string]*guardPkg{}
	var calls []guardCall
	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == "testdata":
				return filepath.SkipDir
			case d.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go"):
				return nil
			}
			calls = append(calls, loadGuardFile(t, root, path, pkgs)...)
			return nil
		})
		if err != nil && !isNotExist(err) {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return pkgs, calls
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

func loadGuardFile(
	t *testing.T, root, path string, pkgs map[string]*guardPkg,
) []guardCall {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil,
		parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("relative path of %s: %v", path, err)
	}
	rel = filepath.ToSlash(rel)
	dir := filepath.ToSlash(filepath.Dir(rel))
	pkg := pkgs[dir]
	if pkg == nil {
		pkg = &guardPkg{dir: dir, isLLM: file.Name.Name == "llm",
			decls: map[string][]ast.Node{}}
		pkgs[dir] = pkg
	}
	pkg.index(file)
	var calls []guardCall
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if _, model := guardPromptArg[sel.Sel.Name]; model {
					calls = append(calls, guardCall{pkg: pkg, file: rel, fn: fn,
						call: call, name: sel.Sel.Name})
				}
			}
			return true
		})
	}
	return calls
}

func (p *guardPkg) index(file *ast.File) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := funcName(d)
			p.decls[name] = append(p.decls[name], d)
			if d.Recv != nil {
				p.decls["."+d.Name.Name] = append(p.decls["."+d.Name.Name], d)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, n := range vs.Names {
						p.decls[n.Name] = append(p.decls[n.Name], vs)
					}
				}
			}
		}
	}
}

// judgeCall decides one call: an annotation is checked first (a wrong
// claim is a defect even when the prompt happens to be fenced), then the
// llm package's own forwarding methods, then the enclosing function, then
// what the prompt argument names.
func judgeCall(pkgs map[string]*guardPkg, c guardCall) guardResult {
	r := guardResult{site: c.file + ":" + funcName(c.fn)}
	kind, rest := guardDirective(c.fn)
	switch {
	case kind == noDBContentDirective && rest == "":
		r.problem = noDBContentDirective + " needs a reason"
	case kind == noDBContentDirective:
		r.verdict = "no database content: " + rest
	case kind == transportDirective:
		r.verdict, r.problem = judgeTransport(pkgs, rest)
	case c.pkg.isLLM && c.fn.Recv != nil && guardIsModelMethod(c.fn.Name.Name):
		r.verdict = "forwarder: the llm API itself; each caller is judged"
	case c.pkg.directRule(c.fn.Body):
		r.verdict = "fenced in the enclosing function"
	case c.pkg.promptRefsRule(c):
		r.verdict = "fenced by what the prompt argument names"
	default:
		r.problem = "prompt does not reference llm.UntrustedDataRule: fence it, or " +
			"annotate the function " + transportDirective + " <dir.builder>... or " +
			noDBContentDirective + " <reason>"
	}
	return r
}

func guardIsModelMethod(name string) bool {
	_, ok := guardPromptArg[name]
	return ok
}

func guardDirective(fn *ast.FuncDecl) (string, string) {
	if fn.Doc == nil {
		return "", ""
	}
	for _, c := range fn.Doc.List {
		for _, d := range []string{transportDirective, noDBContentDirective} {
			if c.Text == d || strings.HasPrefix(c.Text, d+" ") {
				return d, strings.TrimSpace(strings.TrimPrefix(c.Text, d))
			}
		}
	}
	return "", ""
}

// judgeTransport checks that every builder a transport names exists (as
// dir.Name or dir.Type.Method) and references the rule.
func judgeTransport(pkgs map[string]*guardPkg, rest string) (string, string) {
	builders := strings.Fields(rest)
	if len(builders) == 0 {
		return "", transportDirective + " must name the prompt builder(s) it sends"
	}
	for _, b := range builders {
		slash := strings.LastIndex(b, "/")
		dot := strings.Index(b[slash+1:], ".")
		if dot < 0 {
			return "", "transport builder " + b + " is not dir.Name"
		}
		dir, name := b[:slash+1+dot], b[slash+2+dot:]
		pkg := pkgs[dir]
		if pkg == nil || len(pkg.decls[name]) == 0 {
			return "", "transport builder " + b + " does not exist"
		}
		if !pkg.declsRefRule(pkg.decls[name]) {
			return "", "transport builder " + b + " does not reference llm.UntrustedDataRule"
		}
	}
	return "transport: prompt built by " + strings.Join(builders, ", "), ""
}

func isRuleSelector(n ast.Node) bool {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "UntrustedDataRule" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "llm"
}

// directRule reports whether n itself names the rule.
func (p *guardPkg) directRule(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		found = found || isRuleOrLLMIdent(p, x)
		return !found
	})
	return found
}

// promptRefsRule reports whether the prompt argument, or the right-hand
// side of a local assignment to a name it uses, names a package-level
// const, var or func that references the rule (followed transitively).
func (p *guardPkg) promptRefsRule(c guardCall) bool {
	idx := guardPromptArg[c.name]
	if idx >= len(c.call.Args) {
		return false
	}
	arg := c.call.Args[idx]
	nodes := []ast.Node{arg}
	locals := map[string]bool{}
	ast.Inspect(arg, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			locals[id.Name] = true
		}
		return true
	})
	ast.Inspect(c.fn.Body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && locals[id.Name] {
					nodes = append(nodes, as)
					break
				}
			}
		}
		return true
	})
	return p.declsRefRule(nodes)
}

func (p *guardPkg) declsRefRule(nodes []ast.Node) bool {
	seen := map[ast.Node]bool{}
	for _, n := range nodes {
		if p.refsRule(n, seen) {
			return true
		}
	}
	return false
}

// refsRule reports whether n names the rule, directly or through a
// package-level declaration of p that does.
func (p *guardPkg) refsRule(n ast.Node, seen map[ast.Node]bool) bool {
	if seen[n] {
		return false
	}
	seen[n] = true
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if found || isRuleOrLLMIdent(p, x) {
			found = true
			return false
		}
		var refs []ast.Node
		switch v := x.(type) {
		case *ast.SelectorExpr:
			refs = p.decls["."+v.Sel.Name]
		case *ast.Ident:
			refs = p.decls[v.Name]
		}
		for _, d := range refs {
			if p.refsRule(d, seen) {
				found = true
			}
		}
		return !found
	})
	return found
}

// isRuleOrLLMIdent reports whether n is llm.UntrustedDataRule (or, inside
// package llm, the bare UntrustedDataRule).
func isRuleOrLLMIdent(p *guardPkg, n ast.Node) bool {
	if isRuleSelector(n) {
		return true
	}
	id, ok := n.(*ast.Ident)
	return ok && p.isLLM && id.Name == "UntrustedDataRule"
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if idx, ok := typ.(*ast.IndexExpr); ok {
		typ = idx.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

package assertlint

import (
	"go/ast"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// failMethods are the testing.TB methods that mark a test failed.
var failMethods = map[string]bool{
	"Error": true, "Errorf": true, "Fatal": true, "Fatalf": true,
	"Fail": true, "FailNow": true,
}

// tFields are struct field names that conventionally hold a testing.TB,
// as in h.t.Fatalf(...).
var tFields = map[string]bool{"t": true, "tb": true, "T": true}

// assertPkgs are the testsupport packages whose every call can fail.
var assertPkgs = map[string]bool{"require": true, "assert": true, "check": true}

// group is one Go package: the functions it declares and which can fail.
type group struct {
	l       *linter
	funcs   map[string]*ast.FuncDecl
	methods map[string][]*ast.FuncDecl
	fileOf  map[*ast.FuncDecl]*parsedFile
	failing map[string]bool // "f:<name>" for functions, "m:<name>" for methods
}

func newGroup(l *linter, files []*parsedFile) *group {
	g := &group{l: l, funcs: map[string]*ast.FuncDecl{},
		methods: map[string][]*ast.FuncDecl{},
		fileOf:  map[*ast.FuncDecl]*parsedFile{}, failing: map[string]bool{}}
	for _, f := range files {
		for _, decl := range f.ast.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			g.fileOf[fd] = f
			if fd.Recv == nil {
				g.funcs[fd.Name.Name] = fd
			} else {
				g.methods[fd.Name.Name] = append(g.methods[fd.Name.Name], fd)
			}
		}
	}
	g.solve()
	return g
}

// solve marks failing functions until nothing changes, so a helper that
// fails only through another helper is found whatever the order.
func (g *group) solve() {
	for changed := true; changed; {
		changed = false
		for name, fd := range g.funcs {
			if !g.failing["f:"+name] && g.bodyFails(fd) {
				g.failing["f:"+name], changed = true, true
			}
		}
		for name, fds := range g.methods {
			for _, fd := range fds {
				if !g.failing["m:"+name] && g.bodyFails(fd) {
					g.failing["m:"+name], changed = true, true
				}
			}
		}
	}
}

func (g *group) bodyFails(fd *ast.FuncDecl) bool {
	f := g.fileOf[fd]
	tnames := testingNames(fd, f)
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			found = found || g.callFails(f, x, tnames)
		case *ast.Ident:
			// A failing helper passed as a value, as in t.Run(name, check).
			found = found || g.failing["f:"+x.Name]
		}
		return !found
	})
	return found
}

func (g *group) callFails(f *parsedFile, call *ast.CallExpr, tnames map[string]bool) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch x := sel.X.(type) {
	case *ast.Ident:
		if tnames[x.Name] {
			return failMethods[sel.Sel.Name]
		}
		if path, isPkg := f.imports[x.Name]; isPkg {
			return g.l.pkgFails(path, sel.Sel.Name)
		}
	case *ast.SelectorExpr:
		if tFields[x.Sel.Name] && failMethods[sel.Sel.Name] {
			return true
		}
	}
	return g.failing["m:"+sel.Sel.Name]
}

// pkgFails reports whether calling path.name can fail the test: any call
// into a testsupport assertion package, or a module function that fails.
func (l *linter) pkgFails(path, name string) bool {
	base := path[strings.LastIndex(path, "/")+1:]
	if assertPkgs[base] && strings.Contains(path, "/testsupport/") {
		return true
	}
	if path != l.module && !strings.HasPrefix(path, l.module+"/") {
		return false
	}
	return l.externalGroup(path).failing["f:"+name]
}

// externalGroup parses the non-test files of a module package once. A
// package that cannot be read has no helpers; Scan has already parsed
// every directory it reports on, so this only affects helper lookups.
func (l *linter) externalGroup(path string) *group {
	if g, ok := l.external[path]; ok {
		return g
	}
	empty := &group{failing: map[string]bool{}}
	l.external[path] = empty
	rel := strings.TrimPrefix(strings.TrimPrefix(path, l.module), "/")
	files, err := l.parseDir(filepath.Join(l.root, filepath.FromSlash(rel)), false)
	if err != nil {
		return empty
	}
	g := newGroup(l, files)
	l.external[path] = g
	return g
}

// testingNames collects the parameter names of fd and of its closures whose
// type is *testing.T, *testing.B, *testing.F or testing.TB.
func testingNames(fd *ast.FuncDecl, f *parsedFile) map[string]bool {
	pkg := ""
	for name, path := range f.imports {
		if path == "testing" {
			pkg = name
		}
	}
	names := map[string]bool{}
	ast.Inspect(fd, func(n ast.Node) bool {
		ft, ok := n.(*ast.FuncType)
		if !ok || ft.Params == nil || pkg == "" {
			return true
		}
		for _, p := range ft.Params.List {
			if isTestingType(p.Type, pkg) {
				for _, id := range p.Names {
					names[id.Name] = true
				}
			}
		}
		return true
	})
	return names
}

func isTestingType(expr ast.Expr, pkg string) bool {
	if s, ok := expr.(*ast.StarExpr); ok {
		sel, ok := s.X.(*ast.SelectorExpr)
		return ok && isIdent(sel.X, pkg) &&
			(sel.Sel.Name == "T" || sel.Sel.Name == "B" || sel.Sel.Name == "F")
	}
	sel, ok := expr.(*ast.SelectorExpr)
	return ok && isIdent(sel.X, pkg) && sel.Sel.Name == "TB"
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

// hollowTests reports the Test functions of f that cannot fail.
func (g *group) hollowTests(f *parsedFile) []Finding {
	var out []Finding
	for _, decl := range f.ast.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || !isTestFunc(fd, f) || g.failing["f:"+fd.Name.Name] || raceTest(fd, f) {
			continue
		}
		out = append(out, Finding{Rule: RuleNoAssert, Path: f.rel,
			Line: g.l.fset.Position(fd.Pos()).Line, Func: fd.Name.Name})
	}
	return out
}

// isTestFunc matches what go test runs: TestXxx(t *testing.T).
func isTestFunc(fd *ast.FuncDecl, f *parsedFile) bool {
	name := fd.Name.Name
	if fd.Recv != nil || fd.Body == nil || !strings.HasPrefix(name, "Test") {
		return false
	}
	if rest := name[len("Test"):]; rest != "" {
		r, _ := utf8.DecodeRuneInString(rest)
		if unicode.IsLower(r) {
			return false
		}
	}
	params := fd.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	s, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := s.X.(*ast.SelectorExpr)
	return ok && f.imports[identName(sel.X)] == "testing" && sel.Sel.Name == "T"
}

func identName(expr ast.Expr) string {
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// raceTest reports a body that starts goroutines and waits on a
// sync.WaitGroup: under -race a data race fails it without an assertion.
func raceTest(fd *ast.FuncDecl, f *parsedFile) bool {
	var spawns, waits, waitGroup bool
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GoStmt:
			spawns = true
		case *ast.SelectorExpr:
			if f.imports[identName(x.X)] == "sync" && x.Sel.Name == "WaitGroup" {
				waitGroup = true
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && len(x.Args) == 0 &&
				sel.Sel.Name == "Wait" {
				waits = true
			}
		}
		return true
	})
	return spawns && waits && waitGroup
}

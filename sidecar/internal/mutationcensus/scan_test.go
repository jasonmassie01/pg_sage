package mutationcensus

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const executorImport = "github.com/pg-sage/sidecar/internal/executor"

// sqlMethods are the database-driver calls whose SQL argument Rule A reads.
var sqlMethods = map[string]bool{"Exec": true, "Query": true, "QueryRow": true,
	"CopyFrom": true, "SendBatch": true, "Queue": true}

// executorSinks are the raw executor entry points Rule B reports.
var executorSinks = map[string]bool{"ExecConcurrently": true,
	"ExecInTransaction": true, "ExecStatistics": true}

// sink is one function that writes: key is path#Func, detail says why.
type sink struct {
	key, rule, detail string
}

// scanRoots scans each directory under root (relative, slash-separated)
// and returns the sinks sorted by key, one per key and rule.
func scanRoots(root string, dirs []string) ([]sink, error) {
	var found []sink
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string,
			d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if excludedDir(rel) {
				return filepath.SkipDir
			}
			pkg, err := scanPackage(path, rel)
			found = append(found, pkg...)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", dir, err)
		}
	}
	return dedupe(found), nil
}

// excludedDir names the not-shipped trees: test helpers and fixtures.
func excludedDir(rel string) bool {
	return rel == "internal/testsupport" || rel == "internal/testdb" ||
		strings.HasPrefix(rel, "internal/testsupport/") ||
		strings.HasPrefix(rel, "internal/testdb/")
}

func dedupe(found []sink) []sink {
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].key != found[j].key {
			return found[i].key < found[j].key
		}
		return found[i].rule < found[j].rule
	})
	var out []sink
	for _, s := range found {
		n := len(out)
		if n > 0 && out[n-1].key == s.key && out[n-1].rule == s.rule {
			continue
		}
		out = append(out, s)
	}
	return out
}

// scanPackage parses the non-test Go files of one directory together, so
// package-level SQL constants resolve across files.
func scanPackage(dir, rel string) ([]sink, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil,
			parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s/%s: %w", rel, name, err)
		}
		files = append(files, f)
		names = append(names, rel+"/"+name)
	}
	pkg := pkgScope{values: packageValues(files), wrappers: sqlWrappers(files),
		funcs: packageFuncs(files)}
	var found []sink
	for i, f := range files {
		found = append(found, scanFile(f, names[i], pkg)...)
	}
	return found, nil
}

// packageValues maps every package-level const and var name to its value
// expressions.
func packageValues(files []*ast.File) map[string][]ast.Expr {
	values := map[string][]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok {
				addValueSpecs(values, gd)
			}
		}
	}
	return values
}

func addValueSpecs(values map[string][]ast.Expr, gd *ast.GenDecl) {
	if gd.Tok != token.CONST && gd.Tok != token.VAR {
		return
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok || len(vs.Values) != len(vs.Names) {
			continue
		}
		for i, name := range vs.Names {
			values[name.Name] = append(values[name.Name], vs.Values[i])
		}
	}
}

// pkgScope is what one package's files share: its SQL constants and its
// SQL-forwarding wrappers.
type pkgScope struct {
	values   map[string][]ast.Expr
	wrappers map[string]int
	funcs    map[string][]*ast.FuncDecl
}

func packageFuncs(files []*ast.File) map[string][]*ast.FuncDecl {
	funcs := map[string][]*ast.FuncDecl{}
	for _, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				funcs[fn.Name.Name] = append(funcs[fn.Name.Name], fn)
			}
		}
	}
	return funcs
}

// scanFile reports the sinks of one file, keyed by enclosing function.
func scanFile(f *ast.File, rel string, pkg pkgScope) []sink {
	execName := importName(f, executorImport)
	ruleB := execName != "" && !strings.HasPrefix(rel, "internal/executor/")
	var found []sink
	for _, decl := range f.Decls {
		name, body := declScope(decl)
		if body == nil {
			continue
		}
		res := &resolver{pkg: pkg.values, local: localValues(body), funcs: pkg.funcs}
		key := rel + "#" + name
		ast.Inspect(body, func(n ast.Node) bool {
			if ruleB {
				if s, ok := executorRef(n, execName, key); ok {
					found = append(found, s)
				}
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if s, ok := sqlCallSink(call, res, pkg.wrappers, key); ok {
					found = append(found, s)
				}
			}
			return true
		})
	}
	return found
}

// importName is the local name a file uses for an import path, or "".
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return p[strings.LastIndex(p, "/")+1:]
	}
	return ""
}

// declScope names a declaration that holds code and returns that code:
// a function or method, or a package-level var initialised by a func.
func declScope(decl ast.Decl) (string, ast.Node) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Body == nil {
			return "", nil
		}
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return d.Name.Name, d.Body
		}
		return recvType(d.Recv.List[0].Type) + "." + d.Name.Name, d.Body
	case *ast.GenDecl:
		if d.Tok != token.VAR || len(d.Specs) == 0 {
			return "", nil
		}
		vs, ok := d.Specs[0].(*ast.ValueSpec)
		if !ok || len(vs.Names) == 0 {
			return "", nil
		}
		return "var:" + vs.Names[0].Name, d
	}
	return "", nil
}

func recvType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return recvType(t.X)
	case *ast.IndexExpr:
		return recvType(t.X)
	case *ast.IndexListExpr:
		return recvType(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

// executorRef reports Rule B: any reference to a raw executor entry point.
func executorRef(n ast.Node, execName, key string) (sink, bool) {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok || !executorSinks[sel.Sel.Name] {
		return sink{}, false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != execName {
		return sink{}, false
	}
	return sink{key: key, rule: "B", detail: "executor." + sel.Sel.Name}, true
}

package testdb

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The reset lint (ci-13) finds SQL that resets or disturbs the whole
// cluster: a zero-argument pg_stat_statements_reset(), a
// pg_stat_reset_shared( call or an executed CHECKPOINT. Each must run in
// a test that holds the exclusive server (testdb.Exclusive, called
// directly or through a fixture helper), or in helpers only such tests
// call. Table data, map values,
// arguments of non-SQL calls and comments are not SQL that runs.

var (
	lintZeroArgReset = regexp.MustCompile(`(?i)pg_stat_statements_reset\s*\(\s*\)`)
	// (0, 0, 0): every role, every database, every query; the same reset.
	lintAllZeroReset = regexp.MustCompile(`(?i)pg_stat_statements_reset\s*\(` +
		`(\s*0\s*(::\s*\w+\s*)?,){2}\s*0\s*(::\s*\w+\s*)?[,)]`)
	lintResetShared = regexp.MustCompile(`(?i)pg_stat_reset_shared\s*\(`)
)

// resetLintSkipDirs are never walked: fixtures, vendored or built trees.
var resetLintSkipDirs = map[string]bool{
	"testdata": true, "node_modules": true, "vendor": true, "web": true, "dist": true,
}

type resetFinding struct {
	file string
	line int
	what string
}

func (f resetFinding) String() string { return fmt.Sprintf("%s:%d: %s", f.file, f.line, f.what) }

// lintFunc is one top-level function or method of a scanned file.
type lintFunc struct {
	dir, name string
	method    bool
	exclusive bool // calls testdb.Exclusive, itself or through a helper
	findings  []resetFinding
}

// lintCall is one call site, made inside caller (nil at package level).
type lintCall struct {
	caller   *lintFunc
	dir      string
	ident    string // bare callee name, or
	alias    string // the X of a selector callee, with
	sel      string // its Sel
	isImport bool   // whether alias names an import
	pkgPath  string // the import path alias names
}

type resetLintIndex struct {
	modulePath string
	funcs      []*lintFunc
	calls      []lintCall
	loose      []resetFinding // findings outside any function
	byKey      map[string][]lintCall
}

// scanResetLint walks root (whose import path is modulePath), parses
// every file include accepts, and returns the findings no Exclusive
// caller vouches for, sorted.
func scanResetLint(root, modulePath string, include func(rel string) bool,
) ([]resetFinding, error) {
	idx, err := indexResetLint(root, modulePath, include)
	if err != nil {
		return nil, err
	}
	return idx.unvouched(), nil
}

// indexResetLint parses every file under root that include accepts and
// marks each function that reaches testdb.Exclusive.
func indexResetLint(root, modulePath string, include func(rel string) bool,
) (*resetLintIndex, error) {
	idx := &resetLintIndex{modulePath: modulePath}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (resetLintSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".go") || !include(rel) {
			return nil
		}
		file, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("reset lint: parse %s: %w", rel, err)
		}
		idx.addFile(fset, rel, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	idx.spreadExclusive()
	return idx, nil
}

// imports maps each import's local name to its path.
func lintImports(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		out[name] = p
	}
	return out
}

func (idx *resetLintIndex) addFile(fset *token.FileSet, rel string, file *ast.File) {
	dir := path.Dir(rel)
	imports := lintImports(file)
	inTestdb := file.Name.Name == "testdb"
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			idx.loose = append(idx.loose, lintLiterals(fset, rel, decl)...)
			continue
		}
		lf := &lintFunc{dir: dir, name: fn.Name.Name, method: fn.Recv != nil}
		idx.funcs = append(idx.funcs, lf)
		lf.findings = lintLiterals(fset, rel, fn)
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			c := lintCall{caller: lf, dir: dir}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				c.ident = fun.Name
				lf.exclusive = lf.exclusive || (inTestdb && fun.Name == "Exclusive")
			case *ast.SelectorExpr:
				c.sel = fun.Sel.Name
				if x, ok := fun.X.(*ast.Ident); ok {
					c.alias = x.Name
					c.pkgPath, c.isImport = imports[x.Name]
				}
				lf.exclusive = lf.exclusive || (c.isImport && c.sel == "Exclusive" &&
					strings.HasSuffix(c.pkgPath, "/internal/testdb"))
			}
			idx.calls = append(idx.calls, c)
			return true
		})
	}
}

// funcKeys are the names a call can reach f by: a bare call in f's
// directory, a method call (x.name, x not an import) in f's directory, or
// a package-qualified call from anywhere that imports f's package.
func (idx *resetLintIndex) funcKeys(f *lintFunc) []string {
	if f.method {
		return []string{"m|" + f.dir + "|" + f.name}
	}
	pkgPath := idx.modulePath
	if f.dir != "." {
		pkgPath += "/" + f.dir
	}
	return []string{"f|" + f.dir + "|" + f.name, "p|" + pkgPath + "|" + f.name}
}

// callKey is the name call c reaches its callee by (funcKeys).
func callKey(c lintCall) string {
	switch {
	case c.ident != "":
		return "f|" + c.dir + "|" + c.ident
	case c.isImport:
		return "p|" + c.pkgPath + "|" + c.sel
	}
	return "m|" + c.dir + "|" + c.sel
}

// callers returns the call sites that can reach f.
func (idx *resetLintIndex) callers(f *lintFunc) []lintCall {
	var out []lintCall
	for _, key := range idx.funcKeys(f) {
		out = append(out, idx.byKey[key]...)
	}
	return out
}

// spreadExclusive indexes the calls by callee name and marks every
// function that reaches testdb.Exclusive through the helpers it calls (a
// fixture helper that takes the server).
func (idx *resetLintIndex) spreadExclusive() {
	idx.byKey = map[string][]lintCall{}
	for _, c := range idx.calls {
		idx.byKey[callKey(c)] = append(idx.byKey[callKey(c)], c)
	}
	var queue []*lintFunc
	for _, f := range idx.funcs {
		if f.exclusive {
			queue = append(queue, f)
		}
	}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		for _, c := range idx.callers(f) {
			if c.caller != nil && !c.caller.exclusive {
				c.caller.exclusive = true
				queue = append(queue, c.caller)
			}
		}
	}
}

// vouched reports whether every way into f holds the exclusive server: f
// calls Exclusive, or f has callers and each of them is vouched for. A
// cycle or an unknown caller vouches for nothing (fail closed).
func (idx *resetLintIndex) vouched(f *lintFunc, visiting map[*lintFunc]bool) bool {
	if f.exclusive {
		return true
	}
	if visiting[f] {
		return false
	}
	visiting[f] = true
	defer delete(visiting, f)
	calls := idx.callers(f)
	if len(calls) == 0 {
		return false
	}
	for _, c := range calls {
		if c.caller == nil || !idx.vouched(c.caller, visiting) {
			return false
		}
	}
	return true
}

func (idx *resetLintIndex) unvouched() []resetFinding {
	out := append([]resetFinding(nil), idx.loose...)
	for _, f := range idx.funcs {
		if len(f.findings) > 0 && !idx.vouched(f, map[*lintFunc]bool{}) {
			out = append(out, f.findings...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

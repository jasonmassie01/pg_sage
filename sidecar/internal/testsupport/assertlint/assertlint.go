// Package assertlint finds hollow tests (ci-11): a Test function that can
// never fail, and a test helper outside internal/testdb that reads the test
// DSN itself and so bypasses testdb's live check. It reads source with go/ast
// only; nothing is compiled or type-checked.
//
// A test can fail when its body (closures included) calls t.Error*, t.Fatal*
// or t.Fail*, calls into a testsupport require, assert or check package, or
// uses a helper of the same package, or of another package of the module,
// that itself can fail. A test that starts goroutines and waits on a
// sync.WaitGroup is exempt: it exists for the race detector, which fails it.
package assertlint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Rule names a kind of finding; it is the first field of an allowlist line.
type Rule string

const (
	// RuleNoAssert flags a Test function with no failing call.
	RuleNoAssert Rule = "noassert"
	// RuleGetenv flags a test-file read of the test DSN outside testdb.
	RuleGetenv Rule = "getenv"
)

// DSNEnv is the test DSN only internal/testdb may read (testdb.EnvName).
const DSNEnv = "SAGE_TEST_DATABASE_URL"

// testdbDir is the one place allowed to read DSNEnv, relative to the root.
const testdbDir = "internal/testdb/"

// Finding is one hollow test or direct DSN read.
type Finding struct {
	Rule Rule
	Path string // slash-separated, relative to the scanned root
	Line int
	Func string
}

// Key identifies the finding in the allowlist; it omits the line so that
// edits elsewhere in the file do not invalidate an entry.
func (f Finding) Key() string {
	return string(f.Rule) + " " + f.Path + " " + f.Func
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s %s", f.Path, f.Line, f.Rule, f.Func)
}

// Scan reports the findings of every _test.go file under root. module is
// the module path of root, used to follow helpers into sibling packages.
func Scan(root, module string) ([]Finding, error) {
	l := &linter{root: root, module: module, external: map[string]*group{}}
	dirs, err := testDirs(root)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, dir := range dirs {
		found, err := l.scanDir(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

// testDirs lists the directories under root holding a _test.go file. Like
// the go tool's ./..., it skips testdata, vendor, node_modules, dot or
// underscore directories and nested modules.
func testDirs(root string) ([]string, error) {
	seen := map[string]bool{}
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if d.IsDir() {
			if path != root && (skipDir(d.Name()) || isModuleRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		dir := filepath.Dir(path)
		if strings.HasSuffix(path, "_test.go") && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
		return nil
	})
	return dirs, err
}

func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func isModuleRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

type linter struct {
	root     string
	module   string
	fset     *token.FileSet
	external map[string]*group // import path -> non-test files of that package
}

// scanDir checks each package (name) of one directory: package x and its
// external test package x_test are separate groups.
func (l *linter) scanDir(dir string) ([]Finding, error) {
	files, err := l.parseDir(dir, true)
	if err != nil {
		return nil, err
	}
	byPkg := map[string][]*parsedFile{}
	for _, f := range files {
		byPkg[f.ast.Name.Name] = append(byPkg[f.ast.Name.Name], f)
	}
	var out []Finding
	for _, pf := range byPkg {
		g := newGroup(l, pf)
		for _, f := range pf {
			if f.test {
				out = append(out, g.hollowTests(f)...)
				out = append(out, l.getenvReads(f)...)
			}
		}
	}
	return out, nil
}

type parsedFile struct {
	ast     *ast.File
	rel     string
	test    bool
	imports map[string]string // local name -> import path
}

// parseDir parses the .go files of dir, all of them when withTests is set
// and only the non-test files otherwise. Build constraints are ignored, so
// tagged suites are checked too.
func (l *linter) parseDir(dir string, withTests bool) ([]*parsedFile, error) {
	if l.fset == nil {
		l.fset = token.NewFileSet()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var out []*parsedFile
	for _, e := range entries {
		name := e.Name()
		isTest := strings.HasSuffix(name, "_test.go")
		if e.IsDir() || !strings.HasSuffix(name, ".go") || (isTest && !withTests) {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(l.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		rel, err := filepath.Rel(l.root, path)
		if err != nil {
			return nil, fmt.Errorf("relative path of %s: %w", path, err)
		}
		out = append(out, &parsedFile{ast: f, rel: filepath.ToSlash(rel),
			test: isTest, imports: importNames(f)})
	}
	return out, nil
}

func importNames(f *ast.File) map[string]string {
	names := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = path
	}
	return names
}

// getenvReads flags os.Getenv or os.LookupEnv of DSNEnv, spelled as the
// literal or as testdb.EnvName, outside testdb.
func (l *linter) getenvReads(f *parsedFile) []Finding {
	if strings.HasPrefix(f.rel, testdbDir) {
		return nil
	}
	var out []Finding
	for _, decl := range f.ast.Decls {
		name := "<package>"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			name = funcKey(fd)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && readsDSN(f, call) {
				out = append(out, Finding{Rule: RuleGetenv, Path: f.rel,
					Line: l.fset.Position(call.Pos()).Line, Func: name})
			}
			return true
		})
	}
	return out
}

func readsDSN(f *parsedFile, call *ast.CallExpr) bool {
	pkg, fn, ok := pkgCall(call)
	if !ok || f.imports[pkg] != "os" || (fn != "Getenv" && fn != "LookupEnv") ||
		len(call.Args) != 1 {
		return false
	}
	switch arg := call.Args[0].(type) {
	case *ast.BasicLit:
		return arg.Value == `"`+DSNEnv+`"`
	case *ast.SelectorExpr:
		return arg.Sel.Name == "EnvName" &&
			strings.HasSuffix(f.imports[identName(arg.X)], "/internal/testdb")
	}
	return false
}

// pkgCall splits a call of the form x.F(...) into x and F.
func pkgCall(call *ast.CallExpr) (string, string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	return id.Name, sel.Sel.Name, true
}

// funcKey names a function, or a method as Type.Method.
func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

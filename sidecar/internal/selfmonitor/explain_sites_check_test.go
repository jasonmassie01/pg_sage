package selfmonitor

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// security-01c-n: on a session pinned to pg_catalog, pg_temp an
// application's statement resolves its tables only on the application's
// own search_path, which apppath.Pin sets for the planning transaction.
// This test-only check finds every production function holding a string
// literal that opens with EXPLAIN and requires an entry for it in
// testdata/explain_sites.txt; a "pin" entry's function must call
// apppath.Pin itself. It reads Go syntax trees only, so it runs without cgo.

// explainSite is one function (or package-level declaration) holding an
// EXPLAIN literal: key is "<file> <name>", file relative to the module.
type explainSite struct {
	file, name string
	line       int
}

func (s explainSite) key() string { return s.file + " " + s.name }

// funcFacts is what the check needs of one function: whether it calls
// apppath.Pin and whether it holds a literal naming search_path.
type funcFacts struct {
	callsPin, setsPath bool
}

// explainTree is a scanned tree: its sites, and the facts of every
// function by "<package dir> <name>".
type explainTree struct {
	sites []explainSite
	funcs map[string]funcFacts
}

// opensWithExplain reports a literal whose text opens with the EXPLAIN
// keyword (alone, or before a space or an option list).
func opensWithExplain(value string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(value), "EXPLAIN")
	return ok && (rest == "" || strings.ContainsAny(rest[:1], " (\t\n"))
}

// declName names a function as Type.Method or Func.
func declName(x *ast.FuncDecl) string {
	if x.Recv == nil || len(x.Recv.List) == 0 {
		return x.Name.Name
	}
	return recvType(x.Recv.List[0].Type) + "." + x.Name.Name
}

func recvType(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recvType(x.X)
	case *ast.IndexExpr:
		return recvType(x.X)
	case *ast.Ident:
		return x.Name
	}
	return "?"
}

// scanExplainFile adds one file's sites and function facts to tree.
func scanExplainFile(fset *token.FileSet, file string, src []byte, tree *explainTree) error {
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	dir := path.Dir(file)
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			for _, spec := range d.(*ast.GenDecl).Specs {
				tree.addSite(fset, file, specName(spec), spec)
			}
			continue
		}
		tree.funcs[dir+" "+declName(fn)] = tree.addSite(fset, file, declName(fn), fn)
	}
	return nil
}

// addSite records n as a site when it holds an EXPLAIN literal and
// returns its facts.
func (tree *explainTree) addSite(fset *token.FileSet, file, name string, n ast.Node,
) funcFacts {
	facts, site := declFacts(fset, n)
	if site > 0 {
		tree.sites = append(tree.sites, explainSite{file: file, name: name, line: site})
	}
	return facts
}

// specName names a package-level spec by its first name.
func specName(spec ast.Spec) string {
	switch x := spec.(type) {
	case *ast.ValueSpec:
		return x.Names[0].Name
	case *ast.TypeSpec:
		return x.Name.Name
	}
	return "?"
}

// declFacts walks one declaration: its function facts and the line of
// its first EXPLAIN literal (0 for none).
func declFacts(fset *token.FileSet, d ast.Node) (funcFacts, int) {
	var facts funcFacts
	site := 0
	ast.Inspect(d, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			value := literalValue(x)
			if x.Kind == token.STRING && site == 0 && opensWithExplain(value) {
				site = fset.Position(x.Pos()).Line
			}
			facts.setsPath = facts.setsPath || strings.Contains(value, "search_path")
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "apppath" && x.Sel.Name == "Pin" {
				facts.callsPin = true
			}
		}
		return true
	})
	return facts, site
}

// scanExplainTree scans every non-test Go file under root's dirs.
func scanExplainTree(root string, dirs ...string) (explainTree, error) {
	tree := explainTree{funcs: map[string]funcFacts{}}
	fset := token.NewFileSet()
	for _, dir := range dirs {
		walk := func(p string, d os.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && skipDir(d.Name()):
				return filepath.SkipDir
			case d.IsDir(), !strings.HasSuffix(p, ".go"), strings.HasSuffix(p, "_test.go"):
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return scanExplainFile(fset, filepath.ToSlash(rel), src, &tree)
		}
		if err := filepath.WalkDir(filepath.Join(root, dir), walk); err != nil {
			return tree, fmt.Errorf("scan %s: %w", dir, err)
		}
	}
	return tree, nil
}

// siteEntry is one line of the ratchet: "<file> <name> <mode> [<func>]
// [-- <reason>]". Modes:
//   - pin: the function calls apppath.Pin.
//   - via <func>: <func>, in the same package, calls apppath.Pin on the
//     transaction the site plans in.
//   - own <func>: <func>, in the same package, sets the session's own
//     search_path (the agent broker's grant path).
//   - qualified: pg_sage's own statement over schema-qualified names.
//   - text: the literal is never run as a statement (prose, a script
//     for the operator, a pattern).
//
// Every mode but pin needs a reason.
type siteEntry struct {
	file, name, mode, fn, reason string
	line                         int
}

func (e siteEntry) key() string { return e.file + " " + e.name }

// parseSiteEntries reads the ratchet; # starts a comment line.
func parseSiteEntries(text string) ([]siteEntry, []string) {
	var out []siteEntry
	var problems []string
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		head, reason, _ := strings.Cut(line, " -- ")
		f := strings.Fields(head)
		e := siteEntry{line: n, reason: strings.TrimSpace(reason)}
		if len(f) >= 3 {
			e.file, e.name, e.mode = f[0], f[1], f[2]
		}
		if len(f) == 4 {
			e.fn = f[3]
		}
		if p := entryShapeProblem(e, len(f)); p != "" {
			problems = append(problems, fmt.Sprintf("line %d: %s: %q", n, p, line))
			continue
		}
		out = append(out, e)
	}
	return out, problems
}

func entryShapeProblem(e siteEntry, fields int) string {
	switch {
	case e.mode == "pin" && fields == 3:
		return ""
	case (e.mode == "via" || e.mode == "own") && fields == 4 && e.reason != "":
		return ""
	case (e.mode == "qualified" || e.mode == "text") && fields == 3 && e.reason != "":
		return ""
	}
	return "want <file> <name> pin | via|own <func> -- reason | qualified|text -- reason"
}

// checkExplainSites returns every site without an entry, every entry
// without a site, and every entry whose claim the syntax trees disprove.
func checkExplainSites(tree explainTree, entries []siteEntry) []string {
	var problems []string
	listed := map[string]siteEntry{}
	for _, e := range entries {
		listed[e.key()] = e
	}
	found := map[string]bool{}
	for _, s := range tree.sites {
		found[s.key()] = true
		e, ok := listed[s.key()]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s:%d: %s holds an EXPLAIN and is not "+
				"in explain_sites.txt", s.file, s.line, s.name))
			continue
		}
		if p := entryClaimProblem(tree, e); p != "" {
			problems = append(problems, fmt.Sprintf("%s:%d: %s: %s", s.file, s.line, s.name, p))
		}
	}
	for _, e := range entries {
		if !found[e.key()] {
			problems = append(problems, fmt.Sprintf("explain_sites.txt line %d: %s holds no "+
				"EXPLAIN (stale entry)", e.line, e.key()))
		}
	}
	sort.Strings(problems)
	return problems
}

func entryClaimProblem(tree explainTree, e siteEntry) string {
	dir := path.Dir(e.file)
	switch e.mode {
	case "pin":
		if !tree.funcs[dir+" "+e.name].callsPin {
			return "does not call apppath.Pin"
		}
	case "via":
		if !tree.funcs[dir+" "+e.fn].callsPin {
			return e.fn + " does not call apppath.Pin"
		}
	case "own":
		if !tree.funcs[dir+" "+e.fn].setsPath {
			return e.fn + " sets no search_path"
		}
	}
	return ""
}

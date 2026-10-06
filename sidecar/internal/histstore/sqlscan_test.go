package histstore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// historySQL matches a statement that reads or writes a history table.
var historySQL = regexp.MustCompile(
	`(?i)\b(from|join|into|update|truncate)\s+(only\s+)?sage\.(snapshots|query_store)\b`)

// unscopedAllowed are the files whose history SQL is deliberately not
// bound to a store, and why. Everything else that names sage.snapshots or
// sage.query_store in SQL must carry a {db:...} or {dbcol} marker, so it
// fails to parse if it is ever sent without a store binding it: a reader
// can never silently read an empty (or another database's) history.
var unscopedAllowed = map[string]string{
	"internal/schema/":      "DDL and migrations of the tables themselves",
	"internal/partition/":   "partition layout; acts on the pool it is given",
	"internal/retention/":   "acts on the pool it is given: the store cleaner's is the store, a per-database cleaner skips history in meta mode",
	"internal/testsupport/": "test harnesses (perf gate, fixtures)",
	"sre-bench/":            "benchmark harness, always monitored mode",
	"release-bench/":        "benchmark harness, always monitored mode",
}

func allowed(rel string) bool {
	for prefix := range unscopedAllowed {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// TestHistorySQLIsAlwaysScoped scans every non-test Go file of the sidecar.
func TestHistorySQLIsAlwaysScoped(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if allowed(rel) {
			return nil
		}
		scanned++
		offenders = append(offenders, unscopedIn(t, path, rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files: the walk did not reach the sidecar", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("history SQL without a store marker ({db:alias} or {dbcol}); send it "+
			"through histstore.Resolve(pool) with markers:\n%s",
			strings.Join(offenders, "\n"))
	}
}

// unscopedIn returns the history statements of one file that carry no
// marker. A statement is a maximal chain of string concatenations, so a
// marker in another literal of the same expression counts.
func unscopedIn(t *testing.T, path, rel string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	var out []string
	seen := map[ast.Node]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if !ok || seen[n] {
			return true
		}
		text, parts := literalText(expr)
		if len(parts) == 0 {
			return true
		}
		for _, p := range parts {
			seen[p] = true
		}
		if historySQL.MatchString(text) && !strings.Contains(text, "{db:") &&
			!strings.Contains(text, "{dbcol}") {
			out = append(out, rel+":"+strconv.Itoa(fset.Position(n.Pos()).Line))
		}
		return true
	})
	return out
}

// literalText concatenates the string literals of a + chain.
func literalText(e ast.Expr) (string, []ast.Node) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", nil
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", nil
		}
		return s, []ast.Node{v}
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", nil
		}
		l, lp := literalText(v.X)
		r, rp := literalText(v.Y)
		if len(lp)+len(rp) == 0 {
			return "", nil
		}
		return l + r, append(append([]ast.Node{v}, lp...), rp...)
	case *ast.ParenExpr:
		return literalText(v.X)
	}
	return "", nil
}

// TestHistorySQLScanCatchesAnUnscopedStatement proves the scan is not
// vacuous: an unmarked statement is reported, a marked one is not.
func TestHistorySQLScanCatchesAnUnscopedStatement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reader.go")
	src := "package x\n" +
		"const bad = `SELECT 1 FROM sage.snapshots WHERE category = $1`\n" +
		"var good = `SELECT 1 FROM sage.query_store q ` + \"WHERE {db:q}\"\n" +
		"var ins = `INSERT INTO sage.query_store (queryid{dbcol}) VALUES ($1{dbval})`\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := unscopedIn(t, path, "reader.go")
	if len(got) != 1 || got[0] != "reader.go:2" {
		t.Fatalf("want exactly the unmarked statement on line 2, got %v", got)
	}
}

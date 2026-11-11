package selfmonitor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// footprint-08: the packages that send most of pg_sage's statements name
// themselves in the tag (/* pg_sage collector */), so its cost can be
// attributed. A bare /* pg_sage */ string left in their source would show
// as "unattributed" in the footprint.
var componentTaggedPackages = []string{
	"collector", "analyzer", "executor", "facts", "tuner", "optimizer",
}

// stringLiterals returns every string literal of the package's non-test
// Go files under dir (recursively), with its position.
func stringLiterals(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					out[fset.Position(lit.Pos()).String()] = s
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", dir, err)
	}
	return out
}

func TestComponentTags_NoBareTagInComponentPackages(t *testing.T) {
	for _, pkg := range componentTaggedPackages {
		lits := stringLiterals(t, filepath.Join("..", pkg))
		if len(lits) == 0 {
			t.Fatalf("%s: no string literals found; is the path right?", pkg)
		}
		named := 0
		for pos, s := range lits {
			if strings.Contains(s, StatementTag) {
				t.Errorf("%s: bare %s tag; name the component (/* pg_sage %s */)", pos,
					StatementTag, pkg)
			}
			if strings.Contains(s, "/* "+ApplicationName+" "+pkg) {
				named++
			}
		}
		if named == 0 {
			t.Errorf("%s: no statement carries /* %s %s */", pkg, ApplicationName, pkg)
		}
	}
}

// The scan itself must see a bare tag where there is one, or the test
// above would pass on a broken scanner.
func TestComponentTags_ScannerFindsBareTag(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\nconst q = `" + StatementTag + " SELECT 1`\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"),
		[]byte("package x\n\nconst skipped = \"/* pg_sage */\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lits := stringLiterals(t, dir)
	if len(lits) != 1 {
		t.Fatalf("literals = %v, want only x.go's", lits)
	}
	for _, s := range lits {
		if !strings.Contains(s, StatementTag) {
			t.Fatalf("literal %q lost the tag", s)
		}
	}
}

// Tagged statements of a named component are still pg_sage's: the
// exclusion predicates match any /* pg_sage prefix.
func TestComponentTags_NamedTagIsStillTagged(t *testing.T) {
	for _, pkg := range componentTaggedPackages {
		sql := "SELECT /* " + ApplicationName + " " + pkg + " */ 1"
		if !IsTagged(sql) {
			t.Errorf("IsTagged(%q) = false", sql)
		}
		if got := placeTag("/* " + ApplicationName + " " + pkg + " */ SELECT 1"); got !=
			"SELECT /* "+ApplicationName+" "+pkg+" */ 1" {
			t.Errorf("placeTag moved %s's tag to %q", pkg, got)
		}
	}
}

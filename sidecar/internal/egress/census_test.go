package egress

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// forbiddenLiterals are types whose literal construction bypasses the
// egress floor: a client, transport or dialer built here dials without
// egress.Control.
var forbiddenLiterals = map[string]bool{
	"net/http.Client":    true,
	"net/http.Transport": true,
	"net.Dialer":         true,
}

// forbiddenSelectors are package-level clients and helpers that dial
// with the default (unchecked) dialer.
var forbiddenSelectors = map[string]bool{
	"net/http.DefaultClient":    true,
	"net/http.DefaultTransport": true,
	"net/http.Get":              true,
	"net/http.Head":             true,
	"net/http.Post":             true,
	"net/http.PostForm":         true,
	"net.Dial":                  true,
	"net.DialTimeout":           true,
	"crypto/tls.Dial":           true,
	"crypto/tls.DialWithDialer": true,
}

func TestEgressCensusNoBareClients(t *testing.T) {
	var offenders []string
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		found, err := censusTree(root)
		if err != nil {
			t.Fatalf("census walk %s: %v", root, err)
		}
		offenders = append(offenders, found...)
	}
	sort.Strings(offenders)
	t.Logf("egress census: %d offenders", len(offenders))
	for _, o := range offenders {
		t.Errorf("outbound client built outside internal/egress: %s", o)
	}
}

func TestEgressCensusDetectsOffenders(t *testing.T) {
	src := `package x
import (
	h "net/http"
	"net"
	"crypto/tls"
)
var a = &h.Client{}
var b = h.Transport{}
var c = net.Dialer{}
var d = h.DefaultClient
func e() { _, _ = h.Get("x"); _, _ = net.Dial("tcp", "x"); _, _ = tls.Dial("tcp", "x", nil) }
func f() *h.Client { return new(h.Client) }
var ok h.Client
`
	got, err := censusSource("x.go", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("census found %d offenders, want 8: %v", len(got), got)
	}
}

func TestEgressCensusIgnoresShadowedNames(t *testing.T) {
	src := `package x
type Client struct{}
var a = &Client{}
func g(http struct{ DefaultClient int }) int { return 0 }
`
	got, err := censusSource("y.go", src)
	if err != nil || len(got) != 0 {
		t.Fatalf("census = %v, %v; want no offenders", got, err)
	}
}

func censusTree(root string) ([]string, error) {
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "egress" || name == "testdata" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		found, perr := censusFile(path)
		offenders = append(offenders, found...)
		return perr
	})
	return offenders, err
}

func censusFile(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	return censusAST(fset, file), nil
}

func censusSource(name, src string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	return censusAST(fset, file), nil
}

func censusAST(fset *token.FileSet, file *ast.File) []string {
	imports := importNames(file)
	var offenders []string
	report := func(pos token.Pos, what string) {
		offenders = append(offenders, fmt.Sprintf("%s: %s", fset.Position(pos), what))
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if name := qualified(node.Type, imports); forbiddenLiterals[name] {
				report(node.Pos(), name+" literal")
			}
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "new" && len(node.Args) == 1 {
				if name := qualified(node.Args[0], imports); forbiddenLiterals[name] {
					report(node.Pos(), "new("+name+")")
				}
			}
		case *ast.SelectorExpr:
			if name := qualified(node, imports); forbiddenSelectors[name] {
				report(node.Pos(), name)
			}
		}
		return true
	})
	return offenders
}

// importNames maps each file-local package name to its import path.
func importNames(file *ast.File) map[string]string {
	names := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		local := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			local = spec.Name.Name
		}
		names[local] = path
	}
	return names
}

func qualified(expr ast.Expr, imports map[string]string) string {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	path, ok := imports[pkg.Name]
	if !ok {
		return ""
	}
	return path + "." + sel.Sel.Name
}

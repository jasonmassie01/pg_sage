package selfcost

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// The batch census keeps pg_sage's batched statements its own where
// selfmonitor.TagWriter does not see them: pgx sends a batch in
// QueryExecModeExec (default_query_exec_mode=exec, common behind
// PgBouncer) through pgconn.ExecBatch, which writes to the connection
// directly. Every SQL text a non-test Queue call can receive (a constant
// string, or an unexported function's parameter, resolved through its
// callers) is stripped as PostgreSQL 18's pg_stat_statements strips it and
// must stay pg_sage's own (selfmonitor.IsStatementText), keeping any
// pg_sage tag and the component it names. SQL it cannot resolve must be on
// batchCensusAllow with a reason. No concurrent-access or state-transition
// tests: the census is a pure function of parsed sources.

// batchAllow is a Queue call (file, enclosing function) whose SQL the
// census cannot resolve, with why that SQL is recognizably pg_sage's.
type batchAllow struct{ file, fn, reason string }

// batchCensusAllow is empty: every production Queue call resolves.
var batchCensusAllow = []batchAllow{}

// batchText is one SQL text a Queue call can receive; at is where it
// enters the batch (the Queue argument, or a forwarder caller's argument).
type batchText struct{ text, at string }

// batchUnresolved is a Queue call whose SQL the census cannot read.
type batchUnresolved struct{ file, fn, why string }

type batchScan struct {
	texts      []batchText
	unresolved []batchUnresolved
}

// batchPkg is one package's files, type-checked without their imports:
// enough for a constant's value and a parameter's identity.
type batchPkg struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
}

// batchCall is a call and the function it sits in (nil: package level).
type batchCall struct {
	call *ast.CallExpr
	fn   *ast.FuncDecl
}

// pg18Stored is the text PostgreSQL 18's pg_stat_statements keeps for a
// one-statement query: it starts at the first token, so the white space
// and comments in front of it are gone (block comments nest, as the
// server reads them). ok is false for an unterminated block comment.
func pg18Stored(sql string) (string, bool) {
	for {
		s := strings.TrimLeft(sql, " \t\n\r\f\v")
		switch {
		case strings.HasPrefix(s, "--"):
			nl := strings.IndexAny(s, "\n\r")
			if nl < 0 {
				return "", true
			}
			sql = s[nl+1:]
		case strings.HasPrefix(s, "/*"):
			depth, i := 0, 0
			for ; i+1 < len(s) && (i == 0 || depth > 0); i++ {
				switch s[i : i+2] {
				case "/*":
					depth, i = depth+1, i+1
				case "*/":
					depth, i = depth-1, i+1
				}
			}
			if depth > 0 {
				return "", false
			}
			sql = s[i:]
		default:
			return s, true
		}
	}
}

// batchTextProblem names what the untagged batch path loses from text on
// PostgreSQL 18; "" when nothing.
func batchTextProblem(text string) string {
	stored, ok := pg18Stored(text)
	switch {
	case !ok:
		return "unterminated comment in front of the first token"
	case !selfmonitor.IsStatementText(stored):
		return "not recognizable as pg_sage's own once PostgreSQL 18 drops its leading " +
			"comments: it names no sage object and has no pg_sage tag after its first keyword"
	case selfmonitor.IsTagged(text) && !selfmonitor.IsTagged(stored):
		return "its pg_sage tag sits in front of the first keyword, where PostgreSQL 18 drops it"
	case Component(stored) != Component(text):
		return fmt.Sprintf("component %q is stored as %q: its tag sits in front of the "+
			"first keyword", Component(text), Component(stored))
	}
	return ""
}

// batchAllowProblems reports unresolved calls that are not on allow, and
// entries that are stale (no unresolved call there) or give no reason.
func batchAllowProblems(unresolved []batchUnresolved, allow []batchAllow) []string {
	used := make([]bool, len(allow))
	var out []string
	for _, u := range unresolved {
		i := slices.IndexFunc(allow, func(a batchAllow) bool {
			return a.file == u.file && a.fn == u.fn
		})
		if i < 0 {
			out = append(out, fmt.Sprintf("%s %s: %s; resolve it or list it on "+
				"batchCensusAllow with a reason", u.file, u.fn, u.why))
			continue
		}
		used[i] = true
	}
	for i, a := range allow {
		if !used[i] || strings.TrimSpace(a.reason) == "" {
			out = append(out, fmt.Sprintf("allow-list entry %s %s: stale (its SQL resolves "+
				"or the call is gone) or without a reason", a.file, a.fn))
		}
	}
	return out
}

// scanBatchSources parses srcs (module-relative path to source) and scans
// each package among them for Queue calls.
func scanBatchSources(srcs map[string][]byte) (*batchScan, error) {
	fset := token.NewFileSet()
	pkgs := map[string]*batchPkg{}
	for _, rel := range slices.Sorted(maps.Keys(srcs)) {
		f, err := parser.ParseFile(fset, rel, srcs[rel], parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		key := path.Dir(rel) + " " + f.Name.Name
		if pkgs[key] == nil {
			pkgs[key] = &batchPkg{fset: fset}
		}
		pkgs[key].files = append(pkgs[key].files, f)
	}
	out := &batchScan{}
	for _, key := range slices.Sorted(maps.Keys(pkgs)) {
		pkgs[key].scan(key, out)
	}
	return out, nil
}

// scan records the SQL of every Queue call in p (named key).
func (p *batchPkg) scan(key string, out *batchScan) {
	for _, f := range p.files {
		for _, d := range f.Decls {
			fn, _ := d.(*ast.FuncDecl)
			ast.Inspect(d, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Queue" {
					return true
				}
				if p.info == nil {
					p.info = &types.Info{Types: map[ast.Expr]types.TypeAndValue{},
						Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
					// No Importer: every import fails, and only the package is needed.
					conf := types.Config{Error: func(error) {}}
					_, _ = conf.Check(key, p.fset, p.files, p.info) // errors: the imports
				}
				texts, err := p.texts(call.Args[0], fn, 0)
				if err != nil {
					out.unresolved = append(out.unresolved, batchUnresolved{
						p.fset.Position(call.Pos()).Filename, batchFuncName(fn), err.Error()})
				}
				out.texts = append(out.texts, texts...)
				return true
			})
		}
	}
}

// texts returns the SQL texts e can hold in fn: a constant string, or the
// texts of a parameter's argument at every call of fn.
func (p *batchPkg) texts(e ast.Expr, fn *ast.FuncDecl, depth int) ([]batchText, error) {
	at := p.fset.Position(e.Pos()).String()
	if v := p.info.Types[e].Value; v != nil && v.Kind() == constant.String {
		return []batchText{{constant.StringVal(v), at}}, nil
	}
	param := p.param(fn, e)
	if param < 0 {
		return nil, fmt.Errorf("%s: %s is neither a constant string nor a parameter "+
			"its function leaves alone", at, types.ExprString(e))
	}
	calls, values := p.callsOf(fn)
	switch {
	case depth > 8:
		return nil, fmt.Errorf("%s: forwarded through too many parameters", at)
	case fn.Name.IsExported():
		return nil, fmt.Errorf("%s: parameter of exported %s, whose callers outside "+
			"its package are not scanned", at, batchFuncName(fn))
	case values > 0 || len(calls) == 0:
		return nil, fmt.Errorf("%s: %s is used as a value or never called: its "+
			"callers cannot be found", at, batchFuncName(fn))
	}
	var out []batchText
	for _, c := range calls {
		if param >= len(c.call.Args) {
			return nil, fmt.Errorf("%s: call with %d arguments", at, len(c.call.Args))
		}
		texts, err := p.texts(c.call.Args[param], c.fn, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, texts...)
	}
	return out, nil
}

// param is the position of e among fn's parameters, or -1 when e is not
// one or fn assigns to it (by = or by range) or takes its address.
func (p *batchPkg) param(fn *ast.FuncDecl, e ast.Expr) int {
	id, ok := e.(*ast.Ident)
	obj := p.info.Uses[id]
	if !ok || fn == nil || obj == nil {
		return -1
	}
	writes := func(l ast.Expr) bool {
		lid, ok := l.(*ast.Ident)
		return ok && (p.info.Uses[lid] == obj || p.info.Defs[lid] == obj)
	}
	written := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			written = written || slices.ContainsFunc(n.Lhs, writes)
		case *ast.RangeStmt:
			written = written || (n.Tok == token.ASSIGN && (writes(n.Key) || writes(n.Value)))
		case *ast.UnaryExpr:
			written = written || (n.Op == token.AND && writes(n.X))
		}
		return !written
	})
	i := 0 // parameters are all named or all unnamed: unnamed, obj is none of them
	for _, field := range fn.Type.Params.List {
		for _, name := range field.Names {
			if p.info.Defs[name] == obj && !written {
				return i
			}
			i++
		}
	}
	return -1
}

// callsOf returns the calls of fn in p (for a method, every call of a
// method of its name: an interface call included) and how many other
// references to its name there are.
func (p *batchPkg) callsOf(fn *ast.FuncDecl) ([]batchCall, int) {
	names := func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && fn.Recv != nil {
			return sel.Sel.Name == fn.Name.Name
		}
		id, ok := n.(*ast.Ident)
		return ok && fn.Recv == nil && id.Name == fn.Name.Name
	}
	var calls []batchCall
	values := 0
	for _, f := range p.files {
		for _, d := range f.Decls {
			caller, _ := d.(*ast.FuncDecl)
			callees := map[ast.Node]bool{}
			ast.Inspect(d, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					callees[c.Fun] = true
					if names(c.Fun) {
						calls = append(calls, batchCall{c, caller})
					}
				} else if names(n) && !callees[n] && (caller == nil || n != caller.Name) {
					values++
				}
				return true
			})
		}
	}
	return calls, values
}

// batchFuncName is Type.Method or Func ("" at package level).
func batchFuncName(fn *ast.FuncDecl) string {
	switch {
	case fn == nil:
		return ""
	case fn.Recv == nil:
		return fn.Name.Name
	}
	return strings.TrimPrefix(types.ExprString(fn.Recv.List[0].Type), "*") + "." + fn.Name.Name
}

// loadBatchSources reads every non-test Go file under root, skipping
// fixtures, hidden folders and nested modules.
func loadBatchSources(root string) (map[string][]byte, error) {
	srcs := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			_, modErr := os.Stat(filepath.Join(p, "go.mod"))
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") ||
				strings.HasPrefix(name, "_") || modErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		rel, relErr := filepath.Rel(root, p)
		srcs[filepath.ToSlash(rel)] = src
		return errors.Join(err, relErr)
	})
	return srcs, err
}

// The census over the module: every queued SQL text survives PostgreSQL
// 18's leading-comment drop as pg_sage's own, with its tag and component.
func TestBatchSQLCensus(t *testing.T) {
	srcs, err := loadBatchSources(filepath.Join("..", "..")) // the module root
	if err != nil {
		t.Fatal(err)
	}
	scan, err := scanBatchSources(srcs)
	if err != nil {
		t.Fatal(err)
	}
	// Not vacuous: SQL queued in each file with a Queue call when the census
	// was written (a wrong root finds none), and an earned read that reaches
	// reads.go's Queue through its parameter.
	for _, f := range []string{"internal/store/discovery_store.go", "internal/budget/store.go",
		"internal/budget/store_cancels.go", "internal/provideradvice/store.go",
		"internal/earned/approval_history.go"} {
		if !slices.ContainsFunc(scan.texts, func(x batchText) bool {
			return strings.HasPrefix(x.at, f+":")
		}) {
			t.Errorf("no queued SQL resolved in %s", f)
		}
	}
	for _, x := range scan.texts {
		if p := batchTextProblem(x.text); p != "" {
			t.Errorf("%s: %s:\n%s", x.at, p, x.text)
		}
	}
	for _, p := range batchAllowProblems(scan.unresolved, batchCensusAllow) {
		t.Error(p)
	}
}

// Boundary: the census strips what PostgreSQL 18 strips, and no more.
func TestBatchCensus_PG18Stored(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"/* pg_sage */ SELECT 1", "SELECT 1", true},
		{" \t\n-- note\r/* a /* nested */ b */\fSELECT 1 /* tail */", "SELECT 1 /* tail */", true},
		{"SELECT /* pg_sage */ 1", "SELECT /* pg_sage */ 1", true},
		{"-- only a comment", "", true},
		{"", "", true},
		{"/* open /* nested */ SELECT 1", "", false},
	}
	for _, c := range cases {
		if got, ok := pg18Stored(c.in); got != c.want || ok != c.ok {
			t.Errorf("pg18Stored(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// The detector flags a leading-only tag: on PostgreSQL 18 the lock is no
// longer pg_sage's, and a budget write loses its tag or its component.
func TestBatchCensus_FlagsLeadingOnlyTag(t *testing.T) {
	cases := []struct{ text, want string }{
		{"/* pg_sage */ SELECT pg_advisory_xact_lock($1)", "not recognizable"},
		{"/* pg_sage */ INSERT INTO sage.t VALUES (1)", "its pg_sage tag sits in front"},
		{"/* pg_sage budget */\nINSERT INTO sage.t VALUES (1)", "its pg_sage tag sits in front"},
		{"/* pg_sage budget */ INSERT /* pg_sage */ INTO sage.t VALUES (1)",
			`component "budget" is stored as "unattributed"`},
		{"/* pg_sage", "unterminated"},
		{"SELECT 1", "not recognizable"},
		{"SELECT /* pg_sage */ pg_advisory_xact_lock($1)", ""},
		{"INSERT /* pg_sage budget */\nINTO sage.t VALUES (1)", ""},
		{"UPDATE sage.t SET n = 1", ""},
	}
	for _, c := range cases {
		got := batchTextProblem(c.text)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("batchTextProblem(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

const batchFixture = `package fix

import "fmt"

const lead, cols = "/* pg_sage */ SELECT pg_advisory_xact_lock($1)", "a, b"

type q struct{ b interface{ Queue(string, ...any) } }

func (x *q) add(what, sql string) { x.b.Queue(sql) }
func (x *q) Add(sql string)       { x.b.Queue(sql) }
func (x *q) grow(sql string)      { sql += " LIMIT 1"; x.b.Queue(sql) }
func (x *q) loop(sql string, more []string) { for _, sql = range more { x.b.Queue(sql) } }
func (x *q) put(sql string)       { x.b.Queue(sql) }
var _ = (*q).put

func use(x *q, n int) {
	x.b.Queue(lead)
	x.add("w", "/* pg_sage budget */ INSERT INTO sage.t ("+cols+") VALUES (1, 2)")
	x.b.Queue(fmt.Sprintf("SELECT %d", n))
	x.grow("SELECT /* pg_sage */ 1")
	x.loop("SELECT /* pg_sage */ 1", []string{"SELECT 1"})
	x.put("SELECT /* pg_sage */ 1")
}

func shadow(x *q) { cols := "SELECT 1"; x.b.Queue(cols) }
`

// The scan flags a leading-only tag through a constant and through a
// forwarder's caller, and reports SQL it cannot resolve: an exported, a
// reassigned (by = or by range) or a value-used forwarder's parameter, a
// call result, and a local that shadows a constant.
func TestBatchCensus_ScanResolvesAndReports(t *testing.T) {
	scan, err := scanBatchSources(map[string][]byte{"internal/fix/fix.go": []byte(batchFixture)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string // add's text first: the scan follows source order
	for _, x := range scan.texts {
		got = append(got, x.at[:strings.LastIndex(x.at, ":")]+" "+batchTextProblem(x.text))
	}
	for _, u := range scan.unresolved {
		got = append(got, u.fn+" "+u.why)
	}
	want := []string{"internal/fix/fix.go:18 its pg_sage tag sits in front",
		"internal/fix/fix.go:17 not recognizable", "q.Add internal/fix/fix.go:10:",
		"q.grow internal/fix/fix.go:11:", "q.loop internal/fix/fix.go:12:",
		"q.put internal/fix/fix.go:13:", "use internal/fix/fix.go:19:",
		"shadow internal/fix/fix.go:25:"}
	why := []string{"", "", "exported", "sql is neither", "sql is neither", "used as a value",
		"Sprintf", "cols is neither"}
	for i := range want {
		if len(got) != len(want) || !strings.HasPrefix(got[i], want[i]) ||
			!strings.Contains(got[i], why[i]) {
			t.Fatalf("scan = %q, want #%d to start %q and name %q", got, i, want[i], why[i])
		}
	}
}

// Error propagation: an unresolved call off the list, an entry without a
// reason and a stale entry each fail the census, naming the call or entry.
func TestBatchCensus_AllowList(t *testing.T) {
	unresolved := []batchUnresolved{{"a.go", "F", "why"}, {"a.go", "G", "why"},
		{"c.go", "K", "fmt.Sprintf"}}
	allow := []batchAllow{{"a.go", "F", "reason"}, {"a.go", "G", " "}, {"b.go", "H", "reason"}}
	got := batchAllowProblems(unresolved, allow)
	want := []string{"c.go K: fmt.Sprintf", "allow-list entry a.go G:", "allow-list entry b.go H:"}
	if len(got) != len(want) {
		t.Fatalf("problems = %q, want %d", got, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("problem %d = %q, want prefix %q", i, got[i], want[i])
		}
	}
	clean := append(batchAllowProblems(unresolved[:1], allow[:1]), batchAllowProblems(nil, nil)...)
	if clean != nil {
		t.Errorf("an allowed call with a reason, and nothing = %q, want no problem", clean)
	}
}

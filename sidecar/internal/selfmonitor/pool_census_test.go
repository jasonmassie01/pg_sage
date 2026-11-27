package selfmonitor

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The pool census keeps every session pg_sage opens tagged. In source order it judges each
// pgx opener call: of a connection string untagged, of a config tagged if that config went
// to a censusMarker before, is a declared pool's Config() or was assigned either (or a
// Copy(); ConnString() drops the hooks). sagepool.New is a tagged opener; the rest are on
// poolCensusAllow. It compares names, not objects (a config's reassignment or shadowing :=
// goes unseen); only a pool a receiver or parameter declares (*pgxpool.Pool, *pgx.Conn) has a
// Config(). No concurrent or state-transition tests: the census is a pure function.

const (
	censusModule = "github.com/pg-sage/sidecar"
	censusPgx    = "github.com/jackc/pgx/v5"
	censusSM     = censusModule + "/internal/selfmonitor."
	censusBad    = "allow-list entry %s %s: stale (no untagged opener there) or without a reason"
	// censusUserDSN may be a transaction-pooled endpoint the pins would leak through.
	censusUserDSN = "user-chosen DSN: tagging it needs a tag-only mode"
)

var (
	censusOpeners = map[string]bool{censusPgx + ".Connect": true, censusPgx + "/pgxpool.New": true,
		censusPgx + ".ConnectConfig": true, censusPgx + ".ConnectWithOptions": true,
		censusPgx + "/pgxpool.NewWithConfig": true, censusPgx + "/pgconn.Connect": true,
		censusPgx + "/pgconn.ConnectConfig": true, censusPgx + "/pgconn.ConnectWithOptions": true}
	censusMarkers = map[string]bool{censusSM + "ConfigurePool": true,
		censusSM + "ConfigureConn": true, censusModule + "/internal/sagepool.Configure": true}
	censusPools     = map[string]bool{censusPgx + "/pgxpool.Pool": true, censusPgx + ".Conn": true}
	censusPath      = regexp.MustCompile(`^[\pL\pN_.]+$`) // c, s.cfg: never a call's result
	censusMajor     = regexp.MustCompile(`/v[0-9]+$`)
	censusModuleDef = regexp.MustCompile(`(?m)^module\s+"?` + regexp.QuoteMeta(censusModule) +
		`"?\s*$`)
)

// censusAllow is an opener left untagged; fn "*" covers all under dir path/.
type censusAllow struct{ path, fn, reason string }

// poolCensusAllow: safety-bench needs no entry, its opener copies a pool's Config().
var poolCensusAllow = []censusAllow{
	{"cmd/pg_sage_sidecar/metadb_member.go", "memberAction.open", "refusal pool: never dials"},
	{"cmd/pg_sage_sidecar/startup_retry.go", "openStartupPool",
		"copies base; every caller's tune calls ConfigurePool (self_visibility_test.go)"},
	{"cmd/pg_sage_sidecar/lint_migration_cmd.go", "connectLintDatabase", censusUserDSN},
	{"internal/vectorlab/command.go", "commandPool", censusUserDSN},
	{"internal/api/database_helpers.go", "testFromConnStringAtIP",
		"config from sageConnectionConfig, which calls selfmonitor.ConfigureConn"},
	{"internal/agentguard/broker/pool.go", "pools.poolFor",
		"config from poolConfig: pg_sage's pool Config() under the broker role"},
	{"internal/clusterlock/clusterlock.go", "connect", "DSN-based cluster lock session"},
	{"internal/clusterlock/clusterlock.go", "LockConfig",
		"copies its caller's ConnConfig: the executor passes its pool's Config()"},
	{"internal/agentguard/kill_replica.go", "Switch.openReplica", "kill switch on a replica"},
	{"internal/clone/identity.go", "GuardedConnect", "rehearsal clone, never monitored"},
	{"internal/gameday/local_verify.go", "connectTarget", "disposable game-day probe"},
	{"internal/sre/pooler/admin.go", "DialPgBouncer", "PgBouncer admin console"},
	{"sre-bench/", "*", "bench harness: disposable fault targets"},
	{"internal/testdb/", "*", "test infrastructure"},
	{"internal/testsupport/", "*", "test infrastructure"},
}

// censusSite is one opener reference in fn, Type.Method or Func.
type censusSite struct {
	file, fn, callee string
	line             int
	tagged           bool
}

// censusTree is a scan (problems: openers outside a function, dot imports), the file
// in it (imports: names to paths) and its declaration: fn, keys tagged, calls by Fun.
type censusTree struct {
	sites        []censusSite
	problems     []string
	fset         *token.FileSet
	rel, pkg, fn string
	imports      map[string]string
	tagged       map[string]bool
	calls        map[ast.Expr]*ast.CallExpr
}

// addFile scans one source file; rel is relative to the module root.
func (t *censusTree) addFile(fset *token.FileSet, rel string, src []byte) error {
	if ignored, err := censusBuildIgnored(src); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	} else if ignored {
		return nil
	}
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	t.fset, t.rel, t.pkg = fset, rel, path.Join(censusModule, path.Dir(rel))
	t.imports = map[string]string{}
	for _, spec := range f.Imports {
		p := strings.Trim(spec.Path.Value, "\"`")
		name := path.Base(censusMajor.ReplaceAllString(p, ""))
		if spec.Name != nil {
			name = spec.Name.Name
		}
		switch {
		case name == "." && (censusOpeners[p+".Connect"] || censusOpeners[p+".New"]):
			t.problems = append(t.problems,
				fmt.Sprintf("%s: dot import of %s hides its openers", rel, p))
		case name != "." && name != "_":
			t.imports[name] = p
		}
	}
	for _, d := range f.Decls {
		t.fn, t.tagged, t.calls = "", map[string]bool{}, map[ast.Expr]*ast.CallExpr{}
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && len(fd.Recv.List) > 0 {
			recv, _, _ := strings.Cut(types.ExprString(fd.Recv.List[0].Type), "[")
			t.fn = strings.Trim(recv, "*()") + "." + fd.Name.Name
		} else if ok {
			t.fn = fd.Name.Name
		}
		ast.Inspect(d, t.visit)
	}
	return nil
}

// ref is the pkg.Name e names, in the file's package or an import, or "".
func (t *censusTree) ref(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return t.pkg + "." + x.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && t.imports[id.Name] != "" {
			return t.imports[id.Name] + "." + x.Sel.Name
		}
	}
	return ""
}

// visit records each opener, learning in source order the keys tagged: a
// marker's arguments, a declared pool's Config(), a name assigned a tagged one.
func (t *censusTree) visit(n ast.Node) bool {
	switch x := n.(type) {
	case *ast.Field:
		star, _ := x.Type.(*ast.StarExpr)
		for _, id := range x.Names {
			t.tagged[id.Name+".Config()"] = star != nil && censusPools[t.ref(star.X)]
		}
	case *ast.CallExpr:
		t.calls[x.Fun] = x
		for _, a := range x.Args {
			t.tag(a, censusMarkers[t.ref(x.Fun)])
		}
	case *ast.AssignStmt:
		for i, lhs := range x.Lhs {
			t.tag(lhs, len(x.Lhs) == len(x.Rhs) && t.tagged[censusKey(x.Rhs[i], true)])
		}
	case *ast.SelectorExpr:
		t.site(x)
	}
	return true
}

// tag records e's key when ok and e is a path: a marker's c.Copy() tags no c.
func (t *censusTree) tag(e ast.Expr, ok bool) {
	if ok && censusPath.MatchString(censusKey(e, false)) {
		t.tagged[censusKey(e, false)] = true
	}
}

// censusKey is e past (), &, .ConnConfig and .Config (ConfigurePool tags the
// ConnConfig, ConfigureConn its Config) and, with copies, Copy().
func censusKey(e ast.Expr, copies bool) string {
	switch x := ast.Unparen(e).(type) {
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return censusKey(x.X, copies)
		}
	case *ast.SelectorExpr:
		if x.Sel.Name == "ConnConfig" || x.Sel.Name == "Config" {
			return censusKey(x.X, copies)
		}
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && copies && sel.Sel.Name == "Copy" {
			return censusKey(sel.X, copies)
		}
	}
	return types.ExprString(ast.Unparen(e))
}

// site records x when it names an opener: a call of a config opener (NewWithConfig,
// ConnectConfig) is tagged when its key is; one of a connection-string opener (New, Connect,
// ConnectWithOptions), whatever its argument, or any other use (f := pgxpool.New) is not.
func (t *censusTree) site(x *ast.SelectorExpr) {
	ref := t.ref(x)
	if !censusOpeners[ref] {
		return
	}
	i, line, call := strings.LastIndex(ref, "."), t.fset.Position(x.Pos()).Line, t.calls[x]
	callee := path.Base(censusMajor.ReplaceAllString(ref[:i], "")) + ref[i:]
	tagged := call != nil && len(call.Args) == 2 && strings.HasSuffix(ref, "Config") &&
		t.tagged[censusKey(call.Args[1], true)]
	if t.fn == "" {
		t.problems = append(t.problems,
			fmt.Sprintf("%s:%d: %s outside any function", t.rel, line, callee))
	} else {
		t.sites = append(t.sites, censusSite{t.rel, t.fn, callee, line, tagged})
	}
}

// censusBuildIgnored: no build includes the file, its constraint holding
// neither with every other tag set nor with none ("ignore" never is set).
func censusBuildIgnored(src []byte) (bool, error) {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break
		}
		if constraint.IsGoBuild(line) || constraint.IsPlusBuild(line) {
			expr, err := constraint.Parse(line)
			all := func(tag string) bool { return tag != "ignore" }
			return err == nil && !expr.Eval(all) && !expr.Eval(func(string) bool { return false }),
				err
		}
	}
	return false, nil
}

// scanCensusTree scans the non-test Go files ./... sees under root: no vendor, none of npm's.
func scanCensusTree(root string) (*censusTree, error) {
	tree, fset := &censusTree{}, token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch name := d.Name(); {
		case d.IsDir() && p != root && (name == "testdata" || name == "node_modules" ||
			name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")):
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go"):
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(p, root)), "/")
		return tree.addFile(fset, rel, src)
	})
	return tree, err
}

// censusRoot is two levels above pkgDir; its go.mod must declare censusModule.
func censusRoot(pkgDir string) (string, error) {
	root, err := filepath.Abs(filepath.Join(pkgDir, "..", ".."))
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err == nil && !censusModuleDef.Match(raw) {
		err = fmt.Errorf("%s: go.mod does not declare %s", root, censusModule)
	}
	return root, err
}

// checkCensus lists, sorted, each untagged opener allow misses and each bad entry.
func checkCensus(t *censusTree, allow []censusAllow) []string {
	problems := slices.Clone(t.problems)
	used := make([]bool, len(allow))
	for _, s := range t.sites {
		i := slices.IndexFunc(allow, func(a censusAllow) bool {
			return a.fn == "*" && strings.HasPrefix(s.file, a.path) ||
				a.path == s.file && a.fn == s.fn
		})
		switch {
		case s.tagged:
		case i < 0:
			problems = append(problems, fmt.Sprintf("%s:%d: %s opens %s untagged",
				s.file, s.line, s.fn, s.callee))
		default:
			used[i] = true
		}
	}
	for i, a := range allow {
		if !used[i] || a.reason == "" {
			problems = append(problems, fmt.Sprintf(censusBad, a.path, a.fn))
		}
	}
	slices.Sort(problems)
	return problems
}

func TestPoolCensus_Production(t *testing.T) {
	root, err := censusRoot(".")
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	tree, err := scanCensusTree(root)
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	if problems := checkCensus(tree, poolCensusAllow); len(problems) > 0 {
		t.Errorf("tag these or allow-list them with a reason:\n%s", strings.Join(problems, "\n"))
	}
	got := map[string]bool{} // the sites whose verdict this census pins
	for _, s := range tree.sites {
		got[s.file+" "+s.fn] = s.tagged
	}
	for key, want := range map[string]bool{
		"internal/sagepool/sagepool.go New":                          true,
		"cmd/create_admin/main.go openAdminPool":                     true,
		"cmd/pg_sage_sidecar/doctor_cmd.go openDoctorPool":           true,
		"cmd/pg_sage_sidecar/action_identity.go openActionPool":      true,
		"internal/executor/database_setting.go freshDatabaseSetting": true,
		"internal/fleet/observe_only_visit.go observeConnect":        true,
		"safety-bench/readonly_designs.go privRoleDesign.Attempt":    true,
		"cmd/pg_sage_sidecar/startup_retry.go openStartupPool":       false,
		"cmd/pg_sage_sidecar/metadb_member.go memberAction.open":     false,
	} {
		if tagged, ok := got[key]; !ok || tagged != want {
			t.Errorf("site %s: found %v, tagged %v; want tagged %v", key, ok, tagged, want)
		}
	}
}

// pcTree scans in-memory sources keyed by module-relative file name.
func pcTree(t *testing.T, files map[string]string) *censusTree {
	t.Helper()
	tree, fset := &censusTree{}, token.NewFileSet()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := tree.addFile(fset, name, []byte(files[name])); err != nil {
			t.Fatalf("scan %s: %v", name, err)
		}
	}
	return tree
}

const pcHead = "package demo\n\nimport (\n\t\"" + censusPgx + "\"\n\t\"" + censusPgx +
	"/pgxpool\"\n\tsm \"" + censusModule + "/internal/selfmonitor\"\n\t\"" + censusModule +
	"/internal/sagepool\"\n)\n"

// pcShapes: up to shared, demo.go's functions but tagged have one untagged opener on their
// first line; the ok ones and sagepool.go's Open a tagged one; lakebase.go is the old NewPool.
var pcShapes = map[string]string{"internal/demo/demo.go": pcHead + `
import (db "github.com/jackc/pgx/v5"; "github.com/jackc/pgx/v5/pgconn"
	pool "github.com/jackc/pgx/v5/pgxpool"; conf "github.com/pg-sage/sidecar/internal/config"
	dba "github.com/pg-sage/sidecar/internal/databricksauth")
var global = pgxpool.New
func plain(d string)                 { pgxpool.New(nil, d) }
func aliased(d string)               { db.Connect(nil, d) }
func session(c *pgconn.Config)       { pgconn.ConnectConfig(nil, c) }
func value(c *pgxpool.Config)        { sm.ConfigurePool(c); f := pgxpool.NewWithConfig; f(nil, c) }
func (s *Set[K, V]) method()         { pgxpool.New(nil, "") }
func (s *Set[K, V]) field()          { pgxpool.NewWithConfig(nil, s.pool.Config()) }
func rebuilt(p *pgxpool.Pool)        { pgxpool.New(nil, p.Config().ConnString()) }
func packageConfig()                 { pgx.ConnectConfig(nil, conf.Config()) }
func notPool(p *Pool) { sm.ConfigurePool(p.Config()); pgxpool.NewWithConfig(nil, p.Config()) }
func lakebaseOnly(c *pgxpool.Config) { dba.ConfigurePool(nil, c); pgxpool.NewWithConfig(nil, c) }
func helper(c *pgxpool.Config)       { pgxpool.NewWithConfig(nil, tagged(c)) }
func tagged(c *pgxpool.Config) *pgxpool.Config { sm.ConfigurePool(c); return c }
func sagepoolNew(d string)           { _, _ = sagepool.New(nil, d); pgxpool.New(nil, d) }
func closure(c *pgxpool.Config)      { go func() { sm.ConfigurePool(c) }(); pgxpool.New(nil, "") }
func late(c *pgxpool.Config)         { pgxpool.NewWithConfig(nil, c); sm.ConfigurePool(c) }
func copiedOnly(c *pgxpool.Config)   { sm.ConfigurePool(c.Copy()); pgxpool.NewWithConfig(nil, c) }
func shadowed(c *pgxpool.Config)     { sm.ConfigurePool(c); { c := "dsn"; pgxpool.New(nil, c) } }
func shared(c *pgxpool.Config, d string) { pgxpool.New(nil, d)
	sm.ConfigurePool(c); pgxpool.NewWithConfig(nil, c) }
func okAliased(c *pool.Config)    { sm.ConfigurePool(c); pool.NewWithConfig(nil, c) }
func okConn(c *pgx.ConnConfig)    { sm.ConfigureConn(c); pgx.ConnectConfig(nil, c) }
func okConfigure(c *pool.Config)  { sagepool.Configure(nil, c); pool.NewWithConfig(nil, c) }
func okViaConn(c *pool.Config)    { sm.ConfigureConn(c.ConnConfig); pool.NewWithConfig(nil, (c)) }
func okSession(c *pgx.ConnConfig) { sm.ConfigureConn(c); pgconn.ConnectConfig(nil, (&c.Config)) }
func okCopyOf(c *pool.Config)     { sm.ConfigurePool(c); pool.NewWithConfig(nil, c.Copy()) }
func okAssigned(c *pool.Config)   { sm.ConfigurePool(c); d := c.Copy(); pool.NewWithConfig(nil, d) }
func okFromPool(p *pool.Pool)     { pool.NewWithConfig(nil, p.Config()) }
func okFromConn(c *pgx.Conn)      { pgx.ConnectConfig(nil, c.Config()) }
func okCopied(p *pool.Pool)       { c := p.Config().ConnConfig.Copy(); pgx.ConnectConfig(nil, c) }
func okSagepool(d string)         { _, _ = sagepool.New(nil, d); open := sagepool.New; _ = open }
`, "internal/demo/dot.go": "package demo\n\nimport . \"" + censusPgx + "/pgxpool\"\n\n" +
	"func dot() { New(nil, \"\") }\n",
	"internal/sagepool/sagepool.go": "package sagepool\nimport \"" + censusPgx + "/pgxpool\"\n" +
		"func New(d string) { pgxpool.New(nil, d) }\nfunc Open(d string) { c, _ := " +
		"pgxpool.ParseConfig(d); Configure(nil, c); pgxpool.NewWithConfig(nil, c) }\n",
	"internal/databricksauth/lakebase.go": "package databricksauth\n\nimport \"" + censusPgx +
		"/pgxpool\"\n\nfunc NewPool(dsn string) { c, _ := pgxpool.ParseConfig(dsn); " +
		"_ = ConfigurePool(nil, c); pgxpool.NewWithConfig(nil, c) }\n"}

func TestPoolCensus_JudgesEachShape(t *testing.T) {
	at := func(file, mark string) string {
		src := pcShapes["internal/"+file]
		line := strings.Count(src[:strings.Index(src, mark)], "\n") + 1
		return fmt.Sprintf("internal/%s:%d: ", file, line)
	}
	cfg, dsn := "pgxpool.NewWithConfig", "pgxpool.New"
	want := []string{at("demo/demo.go", "var global") + dsn + " outside any function",
		at("sagepool/sagepool.go", "func New") + "New opens " + dsn + " untagged",
		at("databricksauth/lakebase.go", "func NewPool") + "NewPool opens " + cfg + " untagged",
		"internal/demo/dot.go: dot import of " + censusPgx + "/pgxpool hides its openers"}
	for fn, callee := range map[string]string{"plain": dsn, "aliased": "pgx.Connect",
		"session": "pgconn.ConnectConfig", "value": cfg, "Set.method": dsn, "Set.field": cfg,
		"rebuilt": dsn, "packageConfig": "pgx.ConnectConfig", "notPool": cfg, "late": cfg,
		"lakebaseOnly": cfg, "helper": cfg, "sagepoolNew": dsn, "closure": dsn, "shared": dsn,
		"copiedOnly": cfg, "shadowed": dsn} {
		mark := " " + fn[strings.LastIndex(fn, ".")+1:] + "("
		want = append(want, at("demo/demo.go", mark)+fn+" opens "+callee+" untagged")
	}
	slices.Sort(want)
	tree := pcTree(t, pcShapes)
	if got := checkCensus(tree, nil); len(tree.sites) != 31 || !slices.Equal(got, want) {
		t.Errorf("%d sites, problems\n%s\nwant 31 sites, problems\n%s",
			len(tree.sites), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPoolCensus_AllowListCoversOnlyWhatItNames(t *testing.T) {
	const file = "internal/demo/demo.go"
	src := func(fn string) map[string]string { return map[string]string{file: pcHead + fn} }
	untagged := src("\nfunc open() { pgxpool.New(nil, \"\") }\n")
	tagged := src("\nfunc open(p *pgxpool.Pool) { pgxpool.NewWithConfig(nil, p.Config()) }\n")
	exact, dir := censusAllow{file, "open", "r"}, censusAllow{"internal/demo/", "*", "r"}
	other, sibling := censusAllow{file, "other", "r"}, censusAllow{"internal/dem/", "*", "r"}
	bad := func(a censusAllow) string { return fmt.Sprintf(censusBad, a.path, a.fn) }
	flagged := fmt.Sprintf("%s:%d: open opens pgxpool.New untagged", file,
		strings.Count(pcHead, "\n")+2)
	for _, c := range []struct {
		files map[string]string
		allow []censusAllow
		want  []string
	}{
		{untagged, []censusAllow{exact}, nil},
		{untagged, []censusAllow{dir}, nil},
		{untagged, []censusAllow{{file, "open", ""}}, []string{bad(exact)}},
		{untagged, []censusAllow{other}, []string{bad(other), flagged}},
		{untagged, []censusAllow{sibling}, []string{bad(sibling), flagged}},
		{tagged, []censusAllow{exact, dir}, []string{bad(dir), bad(exact)}},
		{nil, []censusAllow{exact}, []string{bad(exact)}},
		{nil, nil, nil},
	} {
		if got := checkCensus(pcTree(t, c.files), c.allow); !slices.Equal(got, c.want) {
			t.Errorf("allow %v: problems %q, want %q", c.allow, got, c.want)
		}
	}
}

func TestPoolCensus_ScanSkipsWhatGoSkipsAndFailsLoud(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := errors.Join(os.MkdirAll(filepath.Dir(p), 0o755),
			os.WriteFile(p, []byte(content), 0o600)); err != nil {
			t.Fatal(err)
		}
	}
	opener := "package p\n\nimport \"" + censusPgx + "\"\n\nfunc open() { pgx.Connect(nil, nil) }\n"
	for rel, head := range map[string]string{"a/a.go": "", "a/a_test.go": "", "a/testdata/t.go": "",
		".git/g.go": "", "_old/o.go": "", "web/node_modules/m/m.go": "", "a/a.txt": "",
		"b/ignore.go": "//go:build ignore\n\n", "b/plus.go": "// +build ignore\n\n",
		"b/crlf.go": "//go:build ignore\r\n\r\n", "c/nocgo.go": "//go:build !cgo\n\n",
		"c/neither.go": "//go:build !unix && !windows\n\n", "vendor/v/v.go": ""} {
		write(rel, head+opener)
	}
	write("c/late.go", opener+"//go:build ignore\n")
	tree, err := scanCensusTree(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var got []string
	for _, s := range tree.sites {
		got = append(got, s.file+" "+s.fn)
	}
	if want := []string{"a/a.go open", "c/late.go open", "c/neither.go open",
		"c/nocgo.go open"}; !slices.Equal(got, want) {
		t.Errorf("sites %q, want %q", got, want)
	}
	err = tree.addFile(token.NewFileSet(), "bad.go", []byte("package p\nfunc {"))
	if err == nil || !strings.HasPrefix(err.Error(), "bad.go:2:") {
		t.Errorf("unparseable source: %v, want a parse error at bad.go:2", err)
	}
	write("d/bad.go", "//go:build (\n\npackage p\n")
	if _, err := scanCensusTree(root); err == nil || !strings.HasPrefix(err.Error(), "d/bad.go: ") {
		t.Errorf("malformed constraint: %v, want an error naming d/bad.go", err)
	}
	if _, err := scanCensusTree(filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing root: %v, want fs.ErrNotExist", err)
	}
	if _, err := censusRoot(filepath.Join(root, "a", "b")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("module root without go.mod: %v, want fs.ErrNotExist", err)
	}
}

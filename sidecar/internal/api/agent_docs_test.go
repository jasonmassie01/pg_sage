package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/decide"
)

// agentgov-08: docs/agent-governance.md documents every agent-governance
// route registered in the files below, and its profiles table lists exactly
// the profiles the gate ships (decide.DefaultProfiles).
//
// No concurrency or state-transition tests: these read static files only.
// No integration test: no SQL or database behaviour is involved.

const agentGovDoc = "../../../docs/agent-governance.md"

// agentRouteFiles register the routes the governance doc must cover.
var agentRouteFiles = []string{"agents_routes.go", "agent_kill_routes.go",
	"agent_activity_routes.go", "agent_grant_routes.go", "agent_env_routes.go",
	"mcp_token_routes.go", "agent_identity_routes.go"}

// minAgentRoutes is the route count of agentRouteFiles today; fewer means
// the scanner lost registrations, so a pass would prove nothing.
const minAgentRoutes = 25

// readGovernanceDoc reads the doc with LF line endings (checkouts may use
// CRLF); a missing or empty file is an error.
func readGovernanceDoc(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the agent governance doc %s: %w", path, err)
	}
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if strings.TrimSpace(doc) == "" {
		return "", fmt.Errorf("the agent governance doc %s is empty", path)
	}
	return doc, nil
}

// scanRoutes returns the pattern of every route registration (two-argument
// Handle or HandleFunc, or route) in one Go file. A pattern that is not a constant string is an error,
// so a registration can never slip past the check.
func scanRoutes(name string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	consts, err := fileConsts(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var routes []string
	var bad []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		arg, ok := routePatternArg(call)
		if !ok {
			return true
		}
		pattern, err := evalString(arg, consts, 0)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", fset.Position(call.Pos()), err))
			return true
		}
		routes = append(routes, pattern)
		return true
	})
	if len(bad) > 0 {
		return nil, fmt.Errorf("route patterns that are not constant: %s",
			strings.Join(bad, "; "))
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("no route, Handle or HandleFunc registrations found in %s", name)
	}
	return routes, nil
}

// routePatternArg is the pattern argument of a route registration: the
// first of mux.Handle / mux.HandleFunc, or the second of the gated
// route(mux, pattern, gate, handler) and of the database-scoped
// routeDB(mux, pattern, gate, resolver, handler) (route_registry.go).
func routePatternArg(call *ast.CallExpr) (ast.Expr, bool) {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		if len(call.Args) == 2 && (fn.Sel.Name == "Handle" || fn.Sel.Name == "HandleFunc") {
			return call.Args[0], true
		}
	case *ast.Ident:
		if (fn.Name == "route" && len(call.Args) == 4) ||
			(fn.Name == "routeDB" && len(call.Args) == 5) {
			return call.Args[1], true
		}
	}
	return nil, false
}

// fileConsts maps every constant declared in f (at any scope) to its
// expression. A name declared twice is an error: it could not be resolved.
func fileConsts(f *ast.File) (map[string]ast.Expr, error) {
	consts := map[string]ast.Expr{}
	var dup []string
	ast.Inspect(f, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if _, seen := consts[id.Name]; seen {
					dup = append(dup, id.Name)
				}
				consts[id.Name] = vs.Values[i]
			}
		}
		return true
	})
	if len(dup) > 0 {
		return nil, fmt.Errorf("constants declared more than once: %s", strings.Join(dup, ", "))
	}
	return consts, nil
}

// evalString evaluates a constant string expression: literals, + and
// constants of the same file.
func evalString(e ast.Expr, consts map[string]ast.Expr, depth int) (string, error) {
	if depth > 10 {
		return "", fmt.Errorf("constant chain too deep")
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			return strconv.Unquote(v.Value)
		}
	case *ast.ParenExpr:
		return evalString(v.X, consts, depth+1)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			l, err := evalString(v.X, consts, depth+1)
			if err != nil {
				return "", err
			}
			r, err := evalString(v.Y, consts, depth+1)
			return l + r, err
		}
	case *ast.Ident:
		if c, ok := consts[v.Name]; ok {
			return evalString(c, consts, depth+1)
		}
	}
	return "", fmt.Errorf("%T is not a constant string", e)
}

// scanAgentRoutes reads every route of agentRouteFiles.
func scanAgentRoutes(t *testing.T) []string {
	t.Helper()
	var all []string
	for _, name := range agentRouteFiles {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read route file %s: %v", name, err)
		}
		routes, err := scanRoutes(name, src)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, routes...)
	}
	if len(all) < minAgentRoutes {
		t.Fatalf("scanned %d agent routes, want at least %d: %v", len(all),
			minAgentRoutes, all)
	}
	return all
}

// routeDocumented reports whether doc names "METHOD /path" as a whole: the
// next character may not continue the path, so GET /api/v1/agents is not
// documented by GET /api/v1/agents/{id} alone.
func routeDocumented(doc, route string) bool {
	re := regexp.MustCompile(regexp.QuoteMeta(route) + `(?:[^A-Za-z0-9_/{}-]|$)`)
	return re.MatchString(doc)
}

// undocumentedRoutes lists the routes doc does not name, sorted.
func undocumentedRoutes(routes []string, doc string) []string {
	var missing []string
	for _, r := range routes {
		if !routeDocumented(doc, r) {
			missing = append(missing, r)
		}
	}
	sort.Strings(missing)
	return missing
}

func TestAgentRoutesDocumented(t *testing.T) {
	doc, err := readGovernanceDoc(agentGovDoc)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range undocumentedRoutes(scanAgentRoutes(t), doc) {
		t.Errorf("docs/agent-governance.md does not document %q", r)
	}
}

// TestAgentRouteScanMatchesMux proves the scanner reads the patterns the
// router really registers: each scanned pattern is the one the mux picks
// for a request built from it.
func TestAgentRouteScanMatchesMux(t *testing.T) {
	mux := http.NewServeMux()
	registerAgentRoutes(mux, nil, nil)
	registerAgentKillRoutes(mux, nil)
	registerAgentActivityRoutes(mux, nil)
	registerAgentGrantRoutes(mux, nil)
	registerAgentEnvRoutes(mux, nil)
	registerMCPTokenRoutes(mux, nil)
	registerAgentIdentityRoutes(mux, nil, nil)
	wildcard := regexp.MustCompile(`\{[^}]+\}`)
	for _, route := range scanAgentRoutes(t) {
		method, path, ok := strings.Cut(route, " ")
		if !ok {
			t.Errorf("route %q has no method", route)
			continue
		}
		req := httptest.NewRequest(method, wildcard.ReplaceAllString(path, "w1"), nil)
		if _, got := mux.Handler(req); got != route {
			t.Errorf("request for %q matched pattern %q", route, got)
		}
	}
}

func TestAgentRoutesDocumented_NamesAddedRoute(t *testing.T) {
	src := []byte(`package api
import "net/http"
const extra = "/x"
func registerDummy(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/agents"+extra, http.NotFoundHandler())
}`)
	dummy, err := scanRoutes("dummy.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(dummy) != 1 || dummy[0] != "GET /api/v1/agents/x" {
		t.Fatalf("scanned %v, want [GET /api/v1/agents/x]", dummy)
	}
	doc, err := readGovernanceDoc(agentGovDoc)
	if err != nil {
		t.Fatal(err)
	}
	routes := append(scanAgentRoutes(t), dummy...)
	missing := undocumentedRoutes(routes, doc)
	if len(missing) != 1 || missing[0] != "GET /api/v1/agents/x" {
		t.Fatalf("undocumented = %v, want exactly [GET /api/v1/agents/x]", missing)
	}
}

func TestRouteDocumented_WholePatternOnly(t *testing.T) {
	doc := "Read one with `GET /api/v1/agents/{id}`.\nPATCH /api/v1/agents/{id}"
	cases := []struct {
		route string
		want  bool
	}{
		{"GET /api/v1/agents/{id}", true},
		{"GET /api/v1/agents", false},       // a prefix of a documented route
		{"POST /api/v1/agents/{id}", false}, // the method is part of the route
		{"PATCH /api/v1/agents/{id}", true}, // at the end of the doc
		{"GET /api/v1/agents/{id}/tokens", false},
		{"", true}, // the empty route is trivially present; scanRoutes never yields it
	}
	for _, c := range cases {
		if got := routeDocumented(doc, c.route); got != c.want {
			t.Errorf("routeDocumented(%q) = %v, want %v", c.route, got, c.want)
		}
	}
	if got := undocumentedRoutes(nil, doc); len(got) != 0 {
		t.Errorf("undocumentedRoutes(nil) = %v, want none", got)
	}
}

func TestReadGovernanceDoc_MissingOrEmpty(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "agent-governance.md")
	if _, err := readGovernanceDoc(missing); err == nil ||
		!strings.Contains(err.Error(), missing) {
		t.Fatalf("missing doc: err = %v, want an error naming %s", err, missing)
	}
	for _, body := range []string{"", " \n\t\n"} {
		if err := os.WriteFile(missing, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readGovernanceDoc(missing); err == nil ||
			!strings.Contains(err.Error(), "is empty") {
			t.Fatalf("empty doc %q: err = %v, want an 'is empty' error", body, err)
		}
	}
}

func TestScanRoutes_RefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"variable pattern": `package api
func f(mux *http.ServeMux, p string) { mux.Handle(p, nil) }`,
		"call pattern": `package api
func f(mux *http.ServeMux) { mux.HandleFunc(path(), nil) }`,
		"no registrations": `package api
func f() {}`,
		"not Go": `package api func {`,
		"duplicate const": "package api\nconst a = \"/x\"\nfunc f(mux *http.ServeMux) {\n" +
			"const a = \"/y\"\nmux.Handle(\"GET \"+a, nil)\n}",
	}
	for name, src := range cases {
		if routes, err := scanRoutes(name+".go", []byte(src)); err == nil {
			t.Errorf("%s: scanned %v without an error", name, routes)
		}
	}
}

// docProfile is one row of the doc's profiles table.
type docProfile struct {
	classes []string
	ceiling string
}

var backticked = regexp.MustCompile("`([^`]+)`")

// parseProfilesTable reads the table under "### Profiles": rows of
// | `name` | `class`, ... | `ceiling` |.
func parseProfilesTable(doc string) (map[string]docProfile, error) {
	_, rest, ok := strings.Cut(doc, "\n### Profiles\n")
	if !ok {
		return nil, fmt.Errorf("the doc has no \"### Profiles\" section")
	}
	out := map[string]docProfile{}
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			break
		}
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cols := strings.Split(strings.Trim(line, "|"), "|")
		if len(cols) < 3 {
			return nil, fmt.Errorf("profiles row %q has %d columns, want 3", line, len(cols))
		}
		name := backticked.FindStringSubmatch(cols[0])
		ceiling := backticked.FindStringSubmatch(cols[2])
		if name == nil || ceiling == nil {
			return nil, fmt.Errorf("profiles row %q needs a `name` and a `ceiling`", line)
		}
		var classes []string
		for _, m := range backticked.FindAllStringSubmatch(cols[1], -1) {
			classes = append(classes, m[1])
		}
		sort.Strings(classes)
		out[name[1]] = docProfile{classes: classes, ceiling: ceiling[1]}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the \"### Profiles\" section has no table rows")
	}
	return out, nil
}

func TestProfilesDocumented(t *testing.T) {
	doc, err := readGovernanceDoc(agentGovDoc)
	if err != nil {
		t.Fatal(err)
	}
	documented, err := parseProfilesTable(doc)
	if err != nil {
		t.Fatal(err)
	}
	shipped := decide.DefaultProfiles()
	for name, p := range shipped {
		d, ok := documented[name]
		if !ok {
			t.Errorf("profile %q is shipped but not in the doc's profiles table", name)
			continue
		}
		var want []string
		for _, c := range p.Classes {
			want = append(want, string(c))
		}
		sort.Strings(want)
		if strings.Join(d.classes, ",") != strings.Join(want, ",") {
			t.Errorf("profile %q: doc classes %v, shipped %v", name, d.classes, want)
		}
		if d.ceiling != string(p.EnvCeiling) {
			t.Errorf("profile %q: doc ceiling %q, shipped %q", name, d.ceiling, p.EnvCeiling)
		}
	}
	for name := range documented {
		if _, ok := shipped[name]; !ok {
			t.Errorf("the doc lists profile %q, which is not shipped", name)
		}
	}
}

func TestParseProfilesTable_EmptyOrMalformed(t *testing.T) {
	cases := map[string]string{
		"no section": "# Agent governance\n\nNo profiles here.\n",
		"no rows":    "# A\n\n### Profiles\n\nText only.\n\n## Next\n| `x` | `read` | `prod` |\n",
		"short row":  "# A\n\n### Profiles\n\n| `x` | `read` |\n",
		"no ceiling": "# A\n\n### Profiles\n\n| `x` | `read` | prod |\n",
	}
	for name, doc := range cases {
		if got, err := parseProfilesTable(doc); err == nil {
			t.Errorf("%s: parsed %v without an error", name, got)
		}
	}
	got, err := parseProfilesTable("x\n### Profiles\n| Profile | Classes |\n|---|---|\n" +
		"| `p` | `write_update`, `read` | `stage` |\n")
	if err != nil {
		t.Fatal(err)
	}
	want := docProfile{classes: []string{"read", "write_update"}, ceiling: "stage"}
	if p := got["p"]; len(got) != 1 || strings.Join(p.classes, ",") != "read,write_update" ||
		p.ceiling != want.ceiling {
		t.Fatalf("parsed %+v, want {p: %+v}", got, want)
	}
}

// TestAgentGovernanceDocWording pins agentgov-08's other doc changes: the
// stale identity clause is gone, the role records are named, and the doc
// is linked from the site nav, the index and the MCP tokens section.
func TestAgentGovernanceDocWording(t *testing.T) {
	doc, err := readGovernanceDoc(agentGovDoc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, "Until pg_sage manages agent identities") {
		t.Error("the doc still says pg_sage does not manage agent identities")
	}
	for _, want := range []string{"# Agent governance\n", "sage.guard_cluster_roles",
		"sidecar/internal/decommission/README.md", "sage.guard_identity_bindings"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the doc does not contain %q", want)
		}
	}
	links := map[string]string{
		"../../../mkdocs.yml":    "Agent governance: agent-governance.md",
		"../../../docs/index.md": "[Agent governance](agent-governance.md)",
		"../../../docs/mcp.md":   "agent-governance.md#tokens",
	}
	for path, want := range links {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s does not contain %q", path, want)
		}
	}
}

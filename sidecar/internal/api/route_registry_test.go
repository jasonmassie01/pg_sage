package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"
)

// bareHandleCalls lists every mux.Handle / mux.HandleFunc call in one
// parsed file, as "file:line receiver.Method".
func bareHandleCalls(fset *token.FileSet, file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
			return true
		}
		recv := "?"
		if id, ok := sel.X.(*ast.Ident); ok {
			recv = id.Name
		}
		pos := fset.Position(call.Pos())
		out = append(out, fmt.Sprintf("%s:%d %s.%s",
			filepath.Base(pos.Filename), pos.Line, recv, sel.Sel.Name))
		return true
	})
	return out
}

func TestNoBareAPIHandle(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var bare []string
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "route_registry.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		bare = append(bare, bareHandleCalls(fset, f)...)
	}
	if checked < 50 {
		t.Fatalf("checked only %d files; the glob did not find the package", checked)
	}
	for _, b := range bare {
		t.Errorf("bare handler registration %s: register with route(mux, "+
			"pattern, gate, handler) so the route declares its gate", b)
	}
}

func TestNoBareAPIHandle_DetectsBareRegistration(t *testing.T) {
	src := `package api
func register(mux *http.ServeMux, root *http.ServeMux) {
	mux.Handle("POST /api/v1/x", h)
	root.HandleFunc("/y", f)
	route(mux, "GET /api/v1/z", gateViewer, h)
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "probe.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := bareHandleCalls(fset, f)
	want := []string{"probe.go:3 mux.Handle", "probe.go:4 root.HandleFunc"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("bare calls = %q, want %q", got, want)
	}
}

func TestNoBareAPIHandle_CleanFileReportsNothing(t *testing.T) {
	src := "package api\nfunc r(mux *http.ServeMux) { route(mux, \"GET /a\", gateAdmin, h) }\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "clean.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := bareHandleCalls(fset, f); len(got) != 0 {
		t.Fatalf("clean file reported %q", got)
	}
}

func TestRouteGate_WrapPerIdentity(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	// Each row: gate, then the status for nobody, viewer, operator, admin.
	cases := []struct {
		gate routeGate
		want [4]int
	}{
		{gatePublic, [4]int{418, 418, 418, 418}},
		{gateSelfAuth, [4]int{418, 418, 418, 418}},
		{gateViewer, [4]int{401, 418, 418, 418}},
		{gateViewerWrite, [4]int{401, 418, 418, 418}},
		{gateOperator, [4]int{401, 403, 418, 418}},
		{gateAdmin, [4]int{401, 403, 403, 418}},
		{gateUnset, [4]int{401, 403, 403, 403}},
		{routeGate(99), [4]int{401, 403, 403, 403}},
	}
	roles := [4]string{"", "viewer", "operator", "admin"}
	for _, tc := range cases {
		h := injectCharRole(tc.gate.wrap(ok))
		for i, role := range roles {
			code, _ := serveProbe(h, routeProbe{"GET", "/api/v1/x"}, role)
			if code != tc.want[i] {
				t.Errorf("gate %s as %q: status %d, want %d", tc.gate, role, code, tc.want[i])
			}
		}
	}
}

func TestRouteGate_UnknownRoleDeniedByViewerGate(t *testing.T) {
	h := injectCharRole(gateViewer.wrap(http.NotFoundHandler()))
	code, body := serveProbe(h, routeProbe{"GET", "/api/v1/x"}, "agent")
	if !isRoleDenied(code, body) {
		t.Fatalf("role outside admin/operator/viewer got %d %q, want 403", code, body)
	}
}

func TestRouteGate_String(t *testing.T) {
	if gateViewerWrite.String() != "viewer-write" || gateUnset.String() != "unset" ||
		routeGate(42).String() != "unset" || gateAdmin.String() != "admin" {
		t.Fatalf("gate names: %s %s %s %s", gateViewerWrite, gateUnset, routeGate(42), gateAdmin)
	}
}

func TestRoute_RecordsPatternAndGate(t *testing.T) {
	mux := http.NewServeMux()
	route(mux, "GET /api/v1/a", gateViewer, http.NotFoundHandler())
	route(mux, "POST /api/v1/a", gateAdmin, http.NotFoundHandler())
	got := registeredRoutes(mux)
	want := []routeEntry{{"GET /api/v1/a", gateViewer}, {"POST /api/v1/a", gateAdmin}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("registry = %v, want %v", got, want)
	}
	code, _ := serveProbe(injectCharRole(mux), routeProbe{"POST", "/api/v1/a"}, "operator")
	if code != http.StatusForbidden {
		t.Fatalf("operator POST behind gateAdmin: %d, want 403", code)
	}
}

func TestRoute_EmptyMuxHasNoRoutes(t *testing.T) {
	if got := registeredRoutes(http.NewServeMux()); len(got) != 0 {
		t.Fatalf("fresh mux has routes %v", got)
	}
}

func TestMount_JoinsChildRegistryOnce(t *testing.T) {
	root, child := http.NewServeMux(), http.NewServeMux()
	route(child, "GET /api/v1/c", gateOperator, http.NotFoundHandler())
	route(root, "/health", gatePublic, http.NotFoundHandler())
	mount(root, "/api/v1/", child, child)
	mount(root, "/mcp", child, child)
	// Routes added to the child after mounting still show up.
	route(child, "GET /api/v1/d", gateViewer, http.NotFoundHandler())
	got := registeredRoutes(root)
	want := []routeEntry{
		{"/health", gatePublic}, {"GET /api/v1/c", gateOperator}, {"GET /api/v1/d", gateViewer},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("registry = %v, want %v", got, want)
	}
}

func TestRoute_ConcurrentRegistration(t *testing.T) {
	// Routers are built concurrently by parallel tests and share the
	// registry map; every registration must be recorded exactly once.
	muxes := make([]*http.ServeMux, 8)
	var wg sync.WaitGroup
	for i := range muxes {
		muxes[i] = http.NewServeMux()
		wg.Add(1)
		go func(mux *http.ServeMux, n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				route(mux, fmt.Sprintf("GET /api/v1/m%d/r%d", n, j), gateViewer,
					http.NotFoundHandler())
			}
		}(muxes[i], i)
	}
	wg.Wait()
	for i, mux := range muxes {
		if got := len(registeredRoutes(mux)); got != 25 {
			t.Errorf("mux %d recorded %d routes, want 25", i, got)
		}
	}
}

func TestRegistry_DroppedWhenMuxCollected(t *testing.T) {
	key := func() weak.Pointer[http.ServeMux] {
		mux := http.NewServeMux()
		route(mux, "GET /api/v1/gc", gateViewer, http.NotFoundHandler())
		return weak.Make(mux)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		registriesMu.Lock()
		_, present := registries[key]
		registriesMu.Unlock()
		if !present {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("registry entry survived its mux being collected")
}

// --- gate declaration rules --------------------------------------------

// checkRouteGates reports every route whose declared gate breaks the
// rules: a gate must be set, gateViewer only on GET, gateViewerWrite only
// on writes listed in viewerWriteRoutes, gateViewerWriteOrToken only on
// writes listed in viewerWriteOrTokenRoutes.
func checkRouteGates(entries []routeEntry) []string {
	var problems []string
	for _, e := range entries {
		method, _, hasMethod := strings.Cut(e.Pattern, " ")
		if !hasMethod {
			method = ""
		}
		switch {
		case e.Gate.String() == "unset":
			problems = append(problems, e.Pattern+": no gate declared")
		case e.Gate == gateViewer && method != http.MethodGet:
			problems = append(problems, e.Pattern+": viewer gate on a write; "+
				"use gateOperator, or gateViewerWrite with a viewerWriteRoutes reason")
		case e.Gate == gateViewerWrite && viewerWriteRoutes[e.Pattern] == "":
			problems = append(problems, e.Pattern+": gateViewerWrite without a "+
				"documented viewerWriteRoutes entry")
		case e.Gate == gateViewerWrite && (method == http.MethodGet || method == ""):
			problems = append(problems, e.Pattern+": gateViewerWrite on a non-write pattern")
		case e.Gate == gateViewerWriteOrToken && viewerWriteOrTokenRoutes[e.Pattern] == "":
			problems = append(problems, e.Pattern+": gateViewerWriteOrToken without a "+
				"documented viewerWriteOrTokenRoutes entry")
		case e.Gate == gateViewerWriteOrToken && (method == http.MethodGet || method == ""):
			problems = append(problems, e.Pattern+": gateViewerWriteOrToken on a non-write "+
				"pattern")
		}
	}
	sort.Strings(problems)
	return problems
}

func TestCheckRouteGates_Rules(t *testing.T) {
	cases := []struct {
		name  string
		entry routeEntry
		bad   bool
	}{
		{"viewer GET", routeEntry{"GET /api/v1/x", gateViewer}, false},
		{"viewer POST", routeEntry{"POST /api/v1/x", gateViewer}, true},
		{"viewer DELETE", routeEntry{"DELETE /api/v1/x/{id}", gateViewer}, true},
		{"viewer any method", routeEntry{"/api/v1/x/", gateViewer}, true},
		{"viewer HEAD-only", routeEntry{"HEAD /api/v1/x", gateViewer}, true},
		{"viewer-write listed", routeEntry{"POST /api/v1/auth/logout", gateViewerWrite}, false},
		{"viewer-write unlisted", routeEntry{"POST /api/v1/x", gateViewerWrite}, true},
		{"unset", routeEntry{"GET /api/v1/x", gateUnset}, true},
		{"out of range", routeEntry{"GET /api/v1/x", routeGate(-1)}, true},
		{"operator POST", routeEntry{"POST /api/v1/x", gateOperator}, false},
		{"self-auth any method", routeEntry{"/api/v1/specialist/", gateSelfAuth}, false},
	}
	for _, tc := range cases {
		got := checkRouteGates([]routeEntry{tc.entry})
		if (len(got) > 0) != tc.bad {
			t.Errorf("%s: problems %q, want bad=%v", tc.name, got, tc.bad)
		}
	}
}

func TestCheckRouteGates_ViewerWriteListIsWritesOnly(t *testing.T) {
	for pattern, reason := range viewerWriteRoutes {
		method, _, _ := strings.Cut(pattern, " ")
		if method == http.MethodGet || method == http.MethodHead || reason == "" {
			t.Errorf("viewerWriteRoutes[%q] = %q: list writes only, each with a reason",
				pattern, reason)
		}
	}
}

func TestNoBareAPIHandle_PackageDirIsCurrent(t *testing.T) {
	// The AST walk reads the working directory; guard against running it
	// from somewhere that is not this package.
	if _, err := os.Stat("route_registry.go"); err != nil {
		t.Fatalf("route_registry.go not in the working directory: %v", err)
	}
}

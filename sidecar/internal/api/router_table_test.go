package api

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// Characterization of the full router before and after the router.go split:
// for several router configurations, every probe records whether the route
// exists (404/405 when it does not) and which role gate guards it. The golden
// file was written from the unmodified router; a pure move must reproduce it.

var updateRouterGolden = flag.Bool("update-router-golden", false,
	"rewrite testdata/router_table.golden from the current router")

const routerGoldenPath = "testdata/router_table.golden"

// unreachableDSN never connects: handlers that touch the database fail fast,
// so probing cannot mutate anything.
const unreachableDSN = "postgres://probe:probe@127.0.0.1:1/probe?sslmode=disable&connect_timeout=1"

type routeProbe struct{ method, path string }

type routerConfig struct {
	name  string
	build func(t *testing.T) http.Handler
}

func TestRouterTable_MatchesGolden(t *testing.T) {
	var out bytes.Buffer
	for _, rc := range routerConfigs() {
		h := rc.build(t)
		for _, p := range routeProbes {
			fmt.Fprintf(&out, "%s %s %s => %s\n", rc.name, p.method, p.path,
				classifyRoute(h, p))
		}
	}
	if *updateRouterGolden {
		if err := os.MkdirAll(filepath.Dir(routerGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(routerGoldenPath, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(routerGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update-router-golden on the "+
			"unmodified router): %v", err)
	}
	diffRouterTables(t, string(want), out.String())
}

func diffRouterTables(t *testing.T, want, got string) {
	t.Helper()
	wantLines := strings.Split(strings.TrimSpace(want), "\n")
	gotLines := strings.Split(strings.TrimSpace(got), "\n")
	if len(wantLines) != len(gotLines) {
		t.Errorf("route table has %d lines, golden has %d", len(gotLines), len(wantLines))
	}
	for i := 0; i < len(wantLines) && i < len(gotLines); i++ {
		if wantLines[i] != gotLines[i] {
			t.Errorf("route table line %d:\n got %s\nwant %s", i+1, gotLines[i], wantLines[i])
		}
	}
}

// classifyRoute answers how the router treats one probe without running a
// privileged handler: unauthenticated first, then viewer, then operator.
func classifyRoute(h http.Handler, p routeProbe) string {
	code, body := serveProbe(h, p, "")
	if code != http.StatusUnauthorized || !strings.Contains(body, "authentication required") {
		return fmt.Sprintf("open:%s", codeLabel(code))
	}
	if code, body = serveProbe(h, p, auth.RoleViewer); !isRoleDenied(code, body) {
		return "gate:viewer"
	}
	if code, body = serveProbe(h, p, "operator"); !isRoleDenied(code, body) {
		return "gate:operator"
	}
	return "gate:admin"
}

func isRoleDenied(code int, body string) bool {
	return code == http.StatusForbidden && strings.Contains(body, "insufficient permissions")
}

func codeLabel(code int) string {
	if code < 0 {
		return "panic"
	}
	return fmt.Sprint(code)
}

// serveProbe sends one JSON request as role ("" = no user) and returns the
// status and body. A handler panic is reported as status -1.
func serveProbe(h http.Handler, p routeProbe, role string) (code int, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	var reqBody *strings.Reader
	if p.method == http.MethodGet || p.method == http.MethodDelete {
		reqBody = strings.NewReader("")
	} else {
		reqBody = strings.NewReader("{}")
	}
	req := httptest.NewRequest(p.method, p.path, reqBody).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Char-Role", role)
	}
	rec := httptest.NewRecorder()
	defer func() {
		if recover() != nil {
			code, body = -1, ""
		}
	}()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// injectCharRole stands in for the production session middleware.
func injectCharRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if role := r.Header.Get("X-Char-Role"); role != "" {
			user := &auth.User{ID: 9, Email: "char@example.test", Role: role}
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
		}
		next.ServeHTTP(w, r)
	})
}

func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), unreachableDSN)
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func routerConfigs() []routerConfig {
	return []routerConfig{
		{"bare", buildBareRouter},
		{"standalone", buildStandaloneRouter},
		{"fleet-readonly", buildFleetReadOnlyRouter},
		{"store-only-actions", buildStoreOnlyActionsRouter},
	}
}

func buildBareRouter(t *testing.T) http.Handler {
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	return NewRouterFullRuntime(mgr, cfg, nil, nil, nil, nil, nil, injectCharRole)
}

func buildStandaloneRouter(t *testing.T) http.Handler {
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	mgr := fleet.NewManager(cfg)
	pool := unreachablePool(t)
	logFn := func(string, string, ...any) {}
	actions := &ActionDeps{
		Store:    store.NewActionStore(pool),
		Executor: executor.New(pool, cfg, time.Time{}, logFn),
	}
	mcp := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	rt := &RuntimeDeps{
		MCPHandler: mcp,
		Autonomy:   &AutonomyDeps{Ledgers: earned.NewRegistry(false)},
	}
	return NewRouterFullRuntime(mgr, cfg, pool, actions, nil, nil, rt, injectCharRole)
}

func buildFleetReadOnlyRouter(t *testing.T) http.Handler {
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	pool := unreachablePool(t)
	actions := &ActionDeps{Fleet: mgr}
	dbDeps := &DatabaseDeps{Store: &store.DatabaseStore{}, Fleet: mgr}
	rt := &RuntimeDeps{DisableConfigWrites: true}
	return NewRouterFullRuntime(mgr, cfg, pool, actions, dbDeps, nil, rt, injectCharRole)
}

func buildStoreOnlyActionsRouter(t *testing.T) http.Handler {
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	actions := &ActionDeps{Store: store.NewActionStore(nil)}
	return NewRouterFullRuntime(mgr, cfg, nil, actions, nil, nil, nil, injectCharRole)
}

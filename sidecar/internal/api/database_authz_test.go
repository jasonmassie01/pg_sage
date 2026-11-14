package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// apiauth-08 phase 1: with api.database_grants on in fleet mode, a
// mutating route that names a database requires the caller's role on that
// database; a route that spans databases needs an unscoped operator.
// These tests use an in-memory grant source; database_authz_db_test.go
// runs the same rules on PostgreSQL.

type dbAuthzSource struct {
	mu     sync.Mutex
	grants map[int]map[string]string
	err    error
	calls  atomic.Int64
}

func (s *dbAuthzSource) UserDatabaseGrants(_ context.Context, id int) (map[string]string,
	error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]string{}
	for db, role := range s.grants[id] {
		out[db] = role
	}
	return out, nil
}

func (s *dbAuthzSource) set(id int, grants map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grants == nil {
		s.grants = map[int]map[string]string{}
	}
	s.grants[id] = grants
}

// dbAuthzEcho answers 200 with the body it received, so a test can see
// the handler ran and the resolver left the body intact.
var dbAuthzEcho = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	jsonResponse(w, map[string]any{"reached": true, "body": string(body)})
})

func dbAuthzMux(src DatabaseGrantSource, mgr *fleet.DatabaseManager) http.Handler {
	mux := http.NewServeMux()
	routeDB(mux, "POST /api/v1/actions/{id}/approve", gateOperator, queryDatabase,
		dbAuthzEcho)
	routeDB(mux, "POST /api/v1/actions/execute", gateOperator, bodyDatabase, dbAuthzEcho)
	routeDB(mux, "POST /api/v1/approvals/{id}/approve", gateOperator,
		approvalCardDatabase(mgr), dbAuthzEcho)
	route(mux, "POST /api/v1/facts", gateOperator, dbAuthzEcho)
	route(mux, "GET /api/v1/findings", gateViewer, dbAuthzEcho)
	route(mux, "POST /api/v1/users", gateAdmin, dbAuthzEcho)
	var authz *databaseAuthz
	if src != nil {
		authz = &databaseAuthz{grants: src}
	}
	return withDatabaseAuthz(authz, mux)
}

func dbAuthzDo(h http.Handler, method, path, body string,
	user *auth.User) (int, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func dbOperator(id int) *auth.User {
	return &auth.User{ID: id, Email: "op@example.test", Role: auth.RoleOperator}
}

func requireForbidden(t *testing.T, label string, code int, body map[string]any) {
	t.Helper()
	if code != http.StatusForbidden || body["code"] != "database_forbidden" {
		t.Fatalf("%s: %d %v, want 403 database_forbidden", label, code, body)
	}
}

func requireReached(t *testing.T, label string, code int, body map[string]any) {
	t.Helper()
	if code != http.StatusOK || body["reached"] != true {
		t.Fatalf("%s: %d %v, want the handler to run (200 reached)", label, code, body)
	}
}

func TestDatabaseAuthz_ScopedOperatorApprovesOnGrantedDatabase(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	h := dbAuthzMux(src, nil)
	code, body := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=A", "{}",
		dbOperator(7))
	requireReached(t, "operator O approves on A", code, body)
}

func TestDatabaseAuthz_ScopedOperatorRefusedOnOtherDatabase(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	h := dbAuthzMux(src, nil)
	code, body := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=B", "{}",
		dbOperator(7))
	requireForbidden(t, "operator O approves on B", code, body)
	if !strings.Contains(body["error"].(string), "B") {
		t.Fatalf("refusal %q does not name the database", body["error"])
	}
	// "all" and an empty database are fleet-wide: a scoped user is refused.
	for _, q := range []string{"?database=all", ""} {
		code, body = dbAuthzDo(h, "POST", "/api/v1/actions/3/approve"+q, "{}", dbOperator(7))
		requireForbidden(t, "scoped approve with "+q, code, body)
	}
}

func TestDatabaseAuthz_NoGrantsKeepsGlobalAccess(t *testing.T) {
	src := &dbAuthzSource{}
	h := dbAuthzMux(src, nil)
	for _, db := range []string{"A", "B"} {
		code, body := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database="+db, "{}",
			dbOperator(8))
		requireReached(t, "unscoped operator on "+db, code, body)
	}
	code, body := dbAuthzDo(h, "POST", "/api/v1/facts", "{}", dbOperator(8))
	requireReached(t, "unscoped operator on a fleet-wide route", code, body)
	if src.calls.Load() != 3 {
		t.Fatalf("grant source consulted %d times, want once per request (3)", src.calls.Load())
	}
}

func TestDatabaseAuthz_LowerRoleWins(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(5, map[string]string{"A": auth.RoleOperator})
	src.set(6, map[string]string{"A": auth.RoleViewer})
	h := dbAuthzMux(src, nil)
	// A global viewer with an operator grant is still refused by the
	// route's own operator gate.
	viewer := &auth.User{ID: 5, Role: auth.RoleViewer}
	code, _ := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=A", "{}", viewer)
	if code != http.StatusForbidden {
		t.Fatalf("global viewer with an operator grant: %d, want 403", code)
	}
	code, body := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=A", "{}",
		dbOperator(6))
	requireForbidden(t, "global operator with a viewer grant on A", code, body)
}

func TestDatabaseAuthz_AdminNeverScoped(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(1, map[string]string{"A": auth.RoleViewer})
	h := dbAuthzMux(src, nil)
	admin := &auth.User{ID: 1, Role: auth.RoleAdmin}
	for _, path := range []string{"/api/v1/actions/3/approve?database=B", "/api/v1/facts",
		"/api/v1/actions/3/approve"} {
		code, body := dbAuthzDo(h, "POST", path, "{}", admin)
		requireReached(t, "admin "+path, code, body)
	}
	if src.calls.Load() != 0 {
		t.Fatalf("grant source consulted %d times for an admin, want 0", src.calls.Load())
	}
}

func TestDatabaseAuthz_StoreFailureIs503NeverAllowed(t *testing.T) {
	src := &dbAuthzSource{err: errors.New("connection refused")}
	h := dbAuthzMux(src, nil)
	for _, path := range []string{"/api/v1/actions/3/approve?database=A", "/api/v1/facts"} {
		code, body := dbAuthzDo(h, "POST", path, "{}", dbOperator(7))
		if code != http.StatusServiceUnavailable || body["code"] != "grant_store_unavailable" ||
			body["reached"] == true {
			t.Fatalf("%s with a failing grant store: %d %v, want 503 "+
				"grant_store_unavailable", path, code, body)
		}
		if strings.Contains(body["error"].(string), "connection refused") {
			t.Fatalf("503 body leaks the store error: %q", body["error"])
		}
	}
}

func TestDatabaseAuthz_DisabledIsPassThrough(t *testing.T) {
	h := dbAuthzMux(nil, nil)
	code, body := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=B", "{}",
		dbOperator(7))
	requireReached(t, "grants off", code, body)
	code, body = dbAuthzDo(h, "POST", "/api/v1/facts", "{}", dbOperator(7))
	requireReached(t, "grants off, fleet-wide route", code, body)
}

func TestDatabaseAuthz_ReadsAreScopedInPhaseTwo(t *testing.T) {
	// apiauth-08b replaced phase 1's unscoped reads: this fixture GET
	// declares no read scope, so a scoped user is refused (fail closed).
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	code, body := dbAuthzDo(dbAuthzMux(src, nil), "GET", "/api/v1/findings?database=B", "",
		dbOperator(7))
	if code != http.StatusForbidden || body["code"] != "fleet_scope_required" {
		t.Fatalf("scoped GET on B: %d %v, want 403 fleet_scope_required", code, body)
	}
	code, body = dbAuthzDo(dbAuthzMux(src, nil), "GET", "/api/v1/findings?database=B", "",
		dbOperator(8))
	requireReached(t, "unscoped GET on B", code, body)
}

func TestDatabaseAuthz_ScopedOperatorRefusedOnFleetWideRoute(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	code, body := dbAuthzDo(dbAuthzMux(src, nil), "POST", "/api/v1/facts",
		`{"database":"A"}`, dbOperator(7))
	requireForbidden(t, "scoped operator on a fleet-wide route", code, body)
}

func TestDatabaseAuthz_BodyResolver(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	h := dbAuthzMux(src, nil)
	in := `{"finding_id":1,"sql":"ANALYZE t","database":"A"}`
	code, body := dbAuthzDo(h, "POST", "/api/v1/actions/execute", in, dbOperator(7))
	requireReached(t, "execute on A", code, body)
	if body["body"] != in {
		t.Fatalf("handler saw body %q, want the original %q", body["body"], in)
	}
	cases := map[string]string{
		"other database":   `{"finding_id":1,"sql":"x","database":"B"}`,
		"duplicate key":    `{"database":"A","database":"B"}`,
		"invalid JSON":     `{"database":"A"`,
		"no database":      `{"finding_id":1}`,
		"database all":     `{"database":"all"}`,
		"non-string value": `{"database":["A"]}`,
	}
	for name, in := range cases {
		code, body := dbAuthzDo(h, "POST", "/api/v1/actions/execute", in, dbOperator(7))
		requireForbidden(t, "body resolver "+name, code, body)
	}
	// An unscoped operator always reaches the handler, which owns its 400s.
	code, body = dbAuthzDo(h, "POST", "/api/v1/actions/execute", `{"database":"A"`,
		dbOperator(8))
	requireReached(t, "unscoped invalid JSON", code, body)
}

func TestDatabaseAuthz_ApprovalCardResolverUsesTheOnlyDatabase(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "A",
		Config: config.DatabaseConfig{Name: "A"}})
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	src.set(9, map[string]string{"B": auth.RoleOperator})
	h := dbAuthzMux(src, mgr)
	code, body := dbAuthzDo(h, "POST", "/api/v1/approvals/3/approve", "{}", dbOperator(7))
	requireReached(t, "single database, no ?database", code, body)
	code, body = dbAuthzDo(h, "POST", "/api/v1/approvals/3/approve", "{}", dbOperator(9))
	requireForbidden(t, "single database A, user scoped to B", code, body)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "B",
		Config: config.DatabaseConfig{Name: "B"}})
	code, body = dbAuthzDo(h, "POST", "/api/v1/approvals/3/approve", "{}", dbOperator(7))
	requireForbidden(t, "two databases, no ?database", code, body)
	code, body = dbAuthzDo(h, "POST", "/api/v1/approvals/3/approve?database=A", "{}",
		dbOperator(7))
	requireReached(t, "two databases, ?database=A", code, body)
}

func TestDatabaseAuthz_StateTransitions(t *testing.T) {
	src := &dbAuthzSource{}
	h := dbAuthzMux(src, nil)
	approve := func() (int, map[string]any) {
		return dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=A", "{}",
			dbOperator(7))
	}
	src.set(7, map[string]string{"A": auth.RoleOperator, "C": auth.RoleViewer})
	code, body := approve()
	requireReached(t, "granted", code, body)
	src.set(7, map[string]string{"C": auth.RoleViewer})
	code, body = approve()
	requireForbidden(t, "after revoking A", code, body)
	// Revoking the last grant returns the user to their global role.
	src.set(7, nil)
	code, body = approve()
	requireReached(t, "after revoking every grant", code, body)
}

func TestDatabaseAuthz_RevokeAppliesToTheNextRequest(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator, "C": auth.RoleOperator})
	h := dbAuthzMux(src, nil)
	var revoked atomic.Bool
	var wg sync.WaitGroup
	failures := make(chan string, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				after := revoked.Load()
				code, _ := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=A", "{}",
					dbOperator(7))
				if after && code != http.StatusForbidden {
					failures <- "a request sent after the revoke was allowed"
					return
				}
				if code != http.StatusOK && code != http.StatusForbidden {
					failures <- "unexpected status"
					return
				}
			}
		}()
	}
	src.set(7, map[string]string{"C": auth.RoleOperator})
	revoked.Store(true)
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Fatal(f)
	}
	code, body := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=A", "{}",
		dbOperator(7))
	requireForbidden(t, "after the revoke", code, body)
}

func TestNewDatabaseAuthz_ModesAndSwitch(t *testing.T) {
	src := &dbAuthzSource{}
	cases := []struct {
		name    string
		mode    string
		enabled bool
		want    bool
	}{
		{"default off in fleet", "fleet", false, false},
		{"on in fleet", "fleet", true, true},
		{"on in meta", "meta", true, true},
		{"standalone ignores grants", "standalone", true, false},
	}
	for _, tc := range cases {
		cfg := config.DefaultConfig()
		cfg.Mode, cfg.API.DatabaseGrants = tc.mode, tc.enabled
		got := newDatabaseAuthz(cfg, nil, src)
		if (got != nil) != tc.want {
			t.Errorf("%s: authz enforced = %v, want %v", tc.name, got != nil, tc.want)
		}
	}
	if newDatabaseAuthz(nil, nil, src) != nil {
		t.Error("nil config must keep today's behaviour")
	}
	cfg := config.DefaultConfig()
	cfg.Mode, cfg.API.DatabaseGrants = "fleet", true
	// Enabled without a control pool or source: every scoped check fails
	// closed with 503 instead of allowing.
	a := newDatabaseAuthz(cfg, nil, nil)
	if a == nil {
		t.Fatal("enabled without a store must still enforce")
	}
	if _, err := a.roleOn(context.Background(), dbOperator(7), "A"); err == nil {
		t.Fatal("enabled without a store: roleOn allowed, want an error")
	}
}

func TestDatabaseAuthz_DefaultConfigKeepsTodaysBehaviour(t *testing.T) {
	if config.DefaultConfig().API.DatabaseGrants {
		t.Fatal("api.database_grants must default to false")
	}
}

// dbAuthzRouter is the production fleet router with grants on (or off)
// and an injected grant source; the caller is the given user.
func dbAuthzRouter(t *testing.T, enabled bool, src DatabaseGrantSource,
	user *auth.User) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode, cfg.API.DatabaseGrants = "fleet", enabled
	mgr := fleet.NewManager(cfg)
	for _, name := range []string{"A", "B"} {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name,
			Config: config.DatabaseConfig{Name: name}})
	}
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, user))
		})
	}
	return newTestRouter(mgr, cfg, unreachablePool(t), &ActionDeps{Fleet: mgr}, nil, nil,
		&RuntimeDeps{DatabaseGrants: src}, inject)
}

func TestDatabaseAuthz_ProductionRouterEnforcesWhenOn(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	h := dbAuthzRouter(t, true, src, dbOperator(7))
	code, body := dbAuthzDo(h, "POST", "/api/v1/managed-changes/3/approve?database=B", "{}",
		nil)
	requireForbidden(t, "router: managed change on B", code, body)
	// On A the handler runs: the registered instance has no pool, so it
	// answers its own 404.
	code, body = dbAuthzDo(h, "POST", "/api/v1/managed-changes/3/approve?database=A", "{}",
		nil)
	if code != http.StatusNotFound || body["error"] != "database not found" {
		t.Fatalf("router: managed change on A: %d %v, want the handler's 404", code, body)
	}
	paths := []string{"/api/v1/actions/3/approve?database=B",
		"/api/v1/actions/3/reject?database=B", "/api/v1/actions/3/rollback?database=B",
		"/api/v1/approvals/3/approve?database=B", "/api/v1/emergency-stop?database=B",
		"/api/v1/emergency-stop", "/api/v1/resume?database=B"}
	for _, p := range paths {
		code, body := dbAuthzDo(h, "POST", p, `{"reason":"r"}`, nil)
		requireForbidden(t, "router: "+p, code, body)
	}
	code, body = dbAuthzDo(h, "POST", "/api/v1/actions/execute",
		`{"finding_id":1,"sql":"ANALYZE t","database":"B"}`, nil)
	requireForbidden(t, "router: execute on B", code, body)
	code, body = dbAuthzDo(h, "POST", "/api/v1/agents/ag-1/grant-requests/4/approve",
		`{"database":"B"}`, nil)
	requireForbidden(t, "router: agent grant request on B", code, body)
}

func TestDatabaseAuthz_ProductionRouterOffByDefault(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"A": auth.RoleOperator})
	h := dbAuthzRouter(t, false, src, dbOperator(7))
	code, body := dbAuthzDo(h, "POST", "/api/v1/managed-changes/3/approve?database=B", "{}",
		nil)
	if code != http.StatusNotFound || body["error"] != "database not found" {
		t.Fatalf("grants off: managed change on B: %d %v, want the handler's 404", code, body)
	}
	if src.calls.Load() != 0 {
		t.Fatalf("grants off: the source was consulted %d times", src.calls.Load())
	}
}

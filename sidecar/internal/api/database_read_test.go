package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// apiauth-08b: per-database read filtering. With api.database_grants on
// in fleet or meta mode, a user scoped by grants reads only the granted
// databases: a database outside the set answers exactly like an unknown
// one, "all" means every visible database, and fleet-only reads are 403.
// These tests use an in-memory grant source; database_read_db_test.go
// runs the real router on PostgreSQL.

func readFleet() *fleet.DatabaseManager {
	return grantsFleet("billing", "search")
}

// readEcho answers with the databases the handler can see through mgr.
func readEcho(m *fleet.DatabaseManager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names := []string{}
		for name := range m.Instances() {
			names = append(names, name)
		}
		sort.Strings(names)
		jsonResponse(w, map[string]any{"reached": true, "databases": names})
	})
}

func readMux(src DatabaseGrantSource, mgr *fleet.DatabaseManager) http.Handler {
	mux := http.NewServeMux()
	routeRead(mux, "GET /api/v1/databases/{db}/runbooks", gateViewer,
		readDatabase(pathDB), readEcho(mgr))
	routeRead(mux, "GET /api/v1/one", gateViewer, readDatabase(queryDatabase),
		readEcho(mgr))
	routeRead(mux, "GET /api/v1/global", gateViewer, readGlobal, readEcho(mgr))
	routeRead(mux, "GET /api/v1/fleet-only", gateViewer, readFleetOnly, readEcho(mgr))
	routeAggregate(mux, "GET /api/v1/findings", gateViewer, mgr, readEcho)
	route(mux, "GET /api/v1/undeclared-fixture", gateViewer, readEcho(mgr))
	var authz *databaseAuthz
	if src != nil {
		authz = &databaseAuthz{grants: src}
	}
	return withDatabaseAuthz(authz, mux)
}

func dbViewer(id int) *auth.User {
	return &auth.User{ID: id, Email: "viewer@example.test", Role: auth.RoleViewer}
}

func readGet(h http.Handler, path string, user *auth.User) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func requireSees(t *testing.T, label string, w *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"reached":true`) {
		t.Fatalf("%s: %d %s, want the handler to run", label, w.Code, w.Body.String())
	}
	if want == nil {
		want = []string{}
	}
	got := fmt.Sprintf(`"databases":%s`, jsonList(want))
	if !strings.Contains(w.Body.String(), got) {
		t.Fatalf("%s: body %s, want %s", label, w.Body.String(), got)
	}
}

func jsonList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + n + `"`
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func requireCode(t *testing.T, label string, w *httptest.ResponseRecorder, status int,
	code string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("%s: %d %s, want %d %s", label, w.Code, w.Body.String(), status, code)
	}
}

func TestDatabaseRead_ScopedViewerAggregateSeesGrantedOnly(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	h := readMux(src, readFleet())
	for _, q := range []string{"", "?database=all", "?database=billing"} {
		requireSees(t, "aggregate "+q, readGet(h, "/api/v1/findings"+q, dbViewer(7)),
			"billing")
	}
	requireSees(t, "path billing", readGet(h, "/api/v1/databases/billing/runbooks",
		dbViewer(7)), "billing", "search")
	requireSees(t, "query billing", readGet(h, "/api/v1/one?database=billing",
		dbViewer(7)), "billing", "search")
}

func TestDatabaseRead_OtherDatabaseLooksUnknown(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	h := readMux(src, readFleet())
	for _, p := range []string{"/api/v1/findings?database=", "/api/v1/one?database=",
		"/api/v1/databases/%s/runbooks"} {
		path := func(db string) string {
			if strings.Contains(p, "%s") {
				return fmt.Sprintf(p, db)
			}
			return p + db
		}
		hidden := readGet(h, path("search"), dbViewer(7))
		unknown := readGet(h, path("nosuch"), dbViewer(7))
		if hidden.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s, want 404", path("search"), hidden.Code, hidden.Body)
		}
		if hidden.Code != unknown.Code || hidden.Body.String() != unknown.Body.String() {
			t.Fatalf("%s: hidden %d %q differs from unknown %d %q (existence oracle)",
				p, hidden.Code, hidden.Body, unknown.Code, unknown.Body)
		}
		if !strings.Contains(hidden.Body.String(), "database not found") {
			t.Fatalf("%s: body %q, want the unknown-database body", p, hidden.Body)
		}
	}
}

func TestDatabaseRead_MalformedNameIs400(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	h := readMux(src, readFleet())
	bad := strings.Repeat("x", 200)
	for _, p := range []string{"/api/v1/findings?database=", "/api/v1/one?database="} {
		w := readGet(h, p+bad, dbViewer(7))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s<long>: %d %s, want 400", p, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "reached") {
			t.Fatalf("%s: the handler ran on a malformed name", p)
		}
	}
}

func TestDatabaseRead_SingleDatabaseRouteNeedsANamedDatabase(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	h := readMux(src, readFleet())
	for _, q := range []string{"", "?database=all"} {
		requireCode(t, "one "+q, readGet(h, "/api/v1/one"+q, dbViewer(7)),
			http.StatusForbidden, "fleet_scope_required")
	}
}

func TestDatabaseRead_FleetOnlyAndGlobal(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	h := readMux(src, readFleet())
	requireCode(t, "fleet-only scoped", readGet(h, "/api/v1/fleet-only", dbViewer(7)),
		http.StatusForbidden, "fleet_scope_required")
	requireCode(t, "undeclared GET fails closed",
		readGet(h, "/api/v1/undeclared-fixture", dbViewer(7)),
		http.StatusForbidden, "fleet_scope_required")
	requireSees(t, "fleet-only unscoped", readGet(h, "/api/v1/fleet-only", dbViewer(8)),
		"billing", "search")
	admin := &auth.User{ID: 9, Role: auth.RoleAdmin}
	src.set(9, map[string]string{"billing": auth.RoleViewer})
	requireSees(t, "fleet-only admin", readGet(h, "/api/v1/fleet-only", admin),
		"billing", "search")
	requireSees(t, "admin aggregate", readGet(h, "/api/v1/findings", admin),
		"billing", "search")
	before := src.calls.Load()
	requireSees(t, "global scoped", readGet(h, "/api/v1/global", dbViewer(7)),
		"billing", "search")
	if src.calls.Load() != before {
		t.Fatal("a global read looked up grants")
	}
}

func TestDatabaseRead_NoGrantsSwitchOffAndStandalone(t *testing.T) {
	src := &dbAuthzSource{}
	unscoped := readMux(src, readFleet())
	requireSees(t, "no grants aggregate", readGet(unscoped, "/api/v1/findings",
		dbViewer(7)), "billing", "search")
	requireSees(t, "no grants other db", readGet(unscoped,
		"/api/v1/databases/search/runbooks", dbViewer(7)), "billing", "search")

	src.set(7, map[string]string{"billing": auth.RoleViewer})
	off := readMux(nil, readFleet()) // switch off: no authorizer in the context
	requireSees(t, "switch off ignores grants", readGet(off, "/api/v1/findings",
		dbViewer(7)), "billing", "search")
	requireSees(t, "switch off fleet-only", readGet(off, "/api/v1/fleet-only",
		dbViewer(7)), "billing", "search")

	for _, mode := range []string{"standalone", ""} {
		cfg := config.DefaultConfig()
		cfg.Mode, cfg.API.DatabaseGrants = mode, true
		if a := newDatabaseAuthz(cfg, nil, src); a != nil {
			t.Fatalf("mode %q: grants enforced on a single-database mode", mode)
		}
	}
}

func TestDatabaseRead_GrantStoreFailureIs503(t *testing.T) {
	src := &dbAuthzSource{err: errors.New("connection refused")}
	h := readMux(src, readFleet())
	for _, p := range []string{"/api/v1/findings?database=all", "/api/v1/one?database=billing",
		"/api/v1/databases/billing/runbooks", "/api/v1/fleet-only"} {
		w := readGet(h, p, dbViewer(7))
		requireCode(t, p, w, http.StatusServiceUnavailable, "grant_store_unavailable")
		if strings.Contains(w.Body.String(), "reached") {
			t.Fatalf("%s: the handler ran after a grant store failure", p)
		}
	}
}

func TestDatabaseRead_GrantOnRemovedDatabaseShowsNothing(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"retired": auth.RoleViewer})
	h := readMux(src, readFleet())
	requireSees(t, "aggregate over a removed database", readGet(h, "/api/v1/findings",
		dbViewer(7)))
}

func TestDatabaseRead_StateTransitionLastGrantRevoked(t *testing.T) {
	src := &dbAuthzSource{}
	h := readMux(src, readFleet())
	requireSees(t, "before any grant", readGet(h, "/api/v1/findings", dbViewer(7)),
		"billing", "search")
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	requireSees(t, "granted billing", readGet(h, "/api/v1/findings", dbViewer(7)), "billing")
	src.set(7, map[string]string{"billing": auth.RoleViewer, "search": auth.RoleOperator})
	requireSees(t, "granted both", readGet(h, "/api/v1/findings", dbViewer(7)),
		"billing", "search")
	src.set(7, map[string]string{})
	requireSees(t, "last grant revoked: global again", readGet(h, "/api/v1/findings",
		dbViewer(7)), "billing", "search")
	requireSees(t, "fleet-only after revoke", readGet(h, "/api/v1/fleet-only",
		dbViewer(7)), "billing", "search")
}

func TestDatabaseRead_ConcurrentUsersSeeOwnSets(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(1, map[string]string{"billing": auth.RoleViewer})
	src.set(2, map[string]string{"search": auth.RoleViewer})
	h := readMux(src, readFleet())
	var wg sync.WaitGroup
	errs := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, want := 1+i%2, `"databases":["billing"]`
			if id == 2 {
				want = `"databases":["search"]`
			}
			w := readGet(h, "/api/v1/findings?database=all", dbViewer(id))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
				errs <- fmt.Sprintf("user %d: %d %s, want %s", id, w.Code, w.Body, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestDatabaseRead_AggregateWithoutFleetFailsClosed(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	mux := http.NewServeMux()
	routeAggregate(mux, "GET /api/v1/findings", gateViewer, nil,
		func(*fleet.DatabaseManager) http.Handler { return dbAuthzEcho })
	h := withDatabaseAuthz(&databaseAuthz{grants: src}, mux)
	requireCode(t, "scoped with no fleet", readGet(h, "/api/v1/findings", dbViewer(7)),
		http.StatusForbidden, "fleet_scope_required")
	w := readGet(h, "/api/v1/findings", dbViewer(8))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "reached") {
		t.Fatalf("unscoped with no fleet: %d %s, want the handler", w.Code, w.Body)
	}
}

func TestDatabaseRead_WritesKeepPhaseOneRefusal(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleOperator})
	h := dbAuthzMux(src, nil)
	code, body := dbAuthzDo(h, "POST", "/api/v1/actions/3/approve?database=search", "{}",
		dbOperator(7))
	requireForbidden(t, "write on a hidden database stays 403", code, body)
}

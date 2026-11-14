package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// apiauth-08b registry rules: every GET route a signed-in user can reach
// (viewer or operator gate) declares how it filters by database for a
// user scoped by grants: database:<resolver>, aggregate, global or
// fleet-only. Admin GETs are admin-only (admins are never scoped). The
// scope table (testdata/route_scopes.golden) shows each route's scope.
// No DB or concurrency test here: registration happens once per router.

func TestRouteReadScopes_EveryReadDeclaresScope(t *testing.T) {
	for _, rc := range routerConfigs() {
		h := rc.build(t, injectCharRole)
		routes := builtRoutes(t, h, rc.name)
		reads := registeredReadScopes(h.(*http.ServeMux))
		for _, p := range checkReadScopes(routes, reads) {
			t.Errorf("%s: %s", rc.name, p)
		}
	}
}

func TestRouteReadScopes_CheckRules(t *testing.T) {
	cases := []struct {
		name    string
		entry   routeEntry
		scope   string
		problem bool
	}{
		{"viewer read with database scope", routeEntry{"GET /api/v1/x", gateViewer},
			"database:path:db", false},
		{"viewer read aggregate", routeEntry{"GET /api/v1/x", gateViewer}, "aggregate", false},
		{"operator read fleet-only", routeEntry{"GET /api/v1/x", gateOperator}, "fleet-only",
			false},
		{"viewer read global", routeEntry{"GET /api/v1/x", gateViewer}, "global", false},
		{"fixture viewer read with no scope", routeEntry{"GET /api/v1/x", gateViewer}, "",
			true},
		{"operator read with no scope", routeEntry{"GET /api/v1/x", gateOperator}, "", true},
		{"unknown kind", routeEntry{"GET /api/v1/x", gateViewer}, "everything", true},
		{"admin read is admin-only", routeEntry{"GET /api/v1/x", gateAdmin}, "admin-only",
			false},
		{"admin read with a user scope", routeEntry{"GET /api/v1/x", gateAdmin}, "aggregate",
			true},
		{"public read needs none", routeEntry{"GET /api/v1/x", gatePublic}, "", false},
		{"write carries no read scope", routeEntry{"POST /api/v1/x", gateOperator},
			"aggregate", true},
	}
	for _, tc := range cases {
		got := checkReadScopes([]routeEntry{tc.entry},
			map[string]string{tc.entry.Pattern: tc.scope})
		if (len(got) > 0) != tc.problem {
			t.Errorf("%s: problems %q, want problem=%v", tc.name, got, tc.problem)
		}
	}
}

func TestRouteReadScopes_FixtureRouteWithoutScopeFails(t *testing.T) {
	mux := http.NewServeMux()
	route(mux, "GET /api/v1/fixture-without-read-scope", gateViewer, http.NotFoundHandler())
	problems := checkReadScopes(registeredRoutes(mux), registeredReadScopes(mux))
	if len(problems) != 1 || !strings.Contains(problems[0], "fixture-without-read-scope") {
		t.Fatalf("problems = %q, want one naming the fixture route", problems)
	}
}

func TestRouteReadScopes_TableHasNoStaleEntries(t *testing.T) {
	registered := map[string]bool{}
	for _, rc := range routerConfigs() {
		h := rc.build(t, injectCharRole)
		for _, e := range builtRoutes(t, h, rc.name) {
			registered[e.Pattern] = true
		}
	}
	for p, s := range readRouteScopes {
		if !registered[p] {
			t.Errorf("readRouteScopes lists %s (%s), which no router registers", p, s)
		}
		if isWriteRoute(p) {
			t.Errorf("readRouteScopes lists a write: %s", p)
		}
	}
}

func TestRouteReadScopes_PlanRoutesAreFiltered(t *testing.T) {
	want := map[string]string{
		"GET /api/v1/databases":                       readKindAggregate,
		"GET /api/v1/findings":                        readKindAggregate,
		"GET /api/v1/actions":                         readKindAggregate,
		"GET /api/v1/incidents":                       readKindAggregate,
		"GET /api/v1/cases":                           readKindAggregate,
		"GET /api/v1/events":                          readKindAggregate,
		"GET /api/v1/databases/{db}/investigations":   "database:path:db",
		"GET /api/v1/agent-classifications/{database}": "database:path:database",
		"GET /api/v1/auth/me":                         readKindGlobal,
	}
	seen := map[string]string{}
	for _, rc := range routerConfigs() {
		h := rc.build(t, injectCharRole)
		for p, s := range registeredReadScopes(h.(*http.ServeMux)) {
			seen[p] = s
		}
	}
	for p, s := range want {
		if seen[p] != s {
			t.Errorf("%s: read scope %q, want %q", p, seen[p], s)
		}
	}
}

func TestRouteReadScopes_FleetOnlyIsForbiddenForScopedUser(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	src.set(9, map[string]string{"billing": auth.RoleViewer})
	mux := http.NewServeMux()
	path := "GET /api/v1/onboarding"
	if readRouteScopes[path].kind != readKindFleetOnly {
		t.Fatalf("%s must be fleet-only for this test, got %v", path, readRouteScopes[path])
	}
	route(mux, path, gateViewer, readEcho(fleet.NewManager(nil)))
	h := withDatabaseAuthz(&databaseAuthz{grants: src}, mux)
	requireCode(t, "scoped viewer", readGet(h, "/api/v1/onboarding", dbViewer(7)),
		http.StatusForbidden, "fleet_scope_required")
	w := readGet(h, "/api/v1/onboarding", &auth.User{ID: 9, Role: auth.RoleAdmin})
	if w.Code != http.StatusOK {
		t.Fatalf("admin on a fleet-only read: %d %s, want 200", w.Code, w.Body)
	}
}

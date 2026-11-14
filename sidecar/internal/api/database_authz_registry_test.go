package api

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// apiauth-08 registry rules: every operator-gated write either resolves
// the one database it acts on (routeDB) or is fleet-wide, and a
// fleet-wide one is listed in fleetWideRoutes with its reason. A
// database-scoped user can use only the first kind. The generated scope
// table (testdata/route_scopes.golden) shows each route's scope.
// No DB or concurrency test here: registration happens once per router.

const routeScopesGoldenPath = "testdata/route_scopes.golden"

// checkRouteScopes reports every route whose database scope breaks the
// rules.
func checkRouteScopes(entries []routeEntry, scopes map[string]string) []string {
	var problems []string
	for _, e := range entries {
		scope := scopes[e.Pattern]
		needsScope := e.Gate == gateOperator && isWriteRoute(e.Pattern)
		switch {
		case !needsScope && scope != "":
			problems = append(problems, e.Pattern+": scope "+scope+" on a route that "+
				"is not an operator write")
		case !needsScope:
		case strings.HasPrefix(scope, "database:") && fleetWideRoutes[e.Pattern] != "":
			problems = append(problems, e.Pattern+": resolves its database but is "+
				"also listed in fleetWideRoutes")
		case strings.HasPrefix(scope, "database:"):
		case scope == scopeFleetWide && fleetWideRoutes[e.Pattern] == "":
			problems = append(problems, e.Pattern+": operator write with no database "+
				"resolver; register it with routeDB, or list it in fleetWideRoutes "+
				"with the reason it spans databases")
		case scope != scopeFleetWide:
			problems = append(problems, e.Pattern+": operator write with no scope recorded")
		}
	}
	sort.Strings(problems)
	return problems
}

func TestDatabaseAuthz_EveryOperatorWriteDeclaresScope(t *testing.T) {
	for _, rc := range routerConfigs() {
		h := rc.build(t, injectCharRole)
		routes := builtRoutes(t, h, rc.name)
		scopes := registeredScopes(h.(*http.ServeMux))
		for _, p := range checkRouteScopes(routes, scopes) {
			t.Errorf("%s: %s", rc.name, p)
		}
	}
}

func TestCheckRouteScopes_Rules(t *testing.T) {
	listed := "POST /api/v1/agents/{id}/freeze"
	if fleetWideRoutes[listed] == "" {
		t.Fatalf("%s must be listed in fleetWideRoutes for this test", listed)
	}
	cases := []struct {
		name    string
		entry   routeEntry
		scope   string
		problem bool
	}{
		{"database scoped", routeEntry{"POST /api/v1/x", gateOperator}, "database:query",
			false},
		{"fleet-wide listed", routeEntry{listed, gateOperator}, scopeFleetWide, false},
		{"fleet-wide unlisted", routeEntry{"POST /api/v1/x", gateOperator}, scopeFleetWide,
			true},
		{"no scope", routeEntry{"POST /api/v1/x", gateOperator}, "", true},
		{"listed but resolved", routeEntry{listed, gateOperator}, "database:body", true},
		{"methodless operator", routeEntry{"/api/v1/x/", gateOperator}, "", true},
		{"operator GET needs none", routeEntry{"GET /api/v1/x", gateOperator}, "", false},
		{"admin write needs none", routeEntry{"POST /api/v1/x", gateAdmin}, "", false},
		{"scope on a viewer read", routeEntry{"GET /api/v1/x", gateViewer},
			"database:query", true},
	}
	for _, tc := range cases {
		got := checkRouteScopes([]routeEntry{tc.entry},
			map[string]string{tc.entry.Pattern: tc.scope})
		if (len(got) > 0) != tc.problem {
			t.Errorf("%s: problems %q, want problem=%v", tc.name, got, tc.problem)
		}
	}
}

func TestRoute_OperatorWriteRecordsFleetWide(t *testing.T) {
	mux := http.NewServeMux()
	route(mux, "POST /api/v1/a", gateOperator, http.NotFoundHandler())
	route(mux, "GET /api/v1/a", gateOperator, http.NotFoundHandler())
	route(mux, "POST /api/v1/b", gateAdmin, http.NotFoundHandler())
	routeDB(mux, "POST /api/v1/c", gateOperator, queryDatabase, http.NotFoundHandler())
	got := registeredScopes(mux)
	want := map[string]string{"POST /api/v1/a": scopeFleetWide,
		"POST /api/v1/c": "database:query"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}
	if routes := registeredRoutes(mux); len(routes) != 4 || routes[3].Gate != gateOperator {
		t.Fatalf("routeDB must record its route and gate too: %v", routes)
	}
}

func TestDatabaseAuthz_PlanRoutesResolveTheirDatabase(t *testing.T) {
	want := []string{
		"POST /api/v1/actions/{id}/approve", "POST /api/v1/actions/{id}/reject",
		"POST /api/v1/actions/{id}/rollback", "POST /api/v1/actions/execute",
		"POST /api/v1/actions/{id}/schedule", "POST /api/v1/actions/{id}/unschedule",
		"POST /api/v1/approvals/{id}/approve", "POST /api/v1/approvals/{id}/reject",
		"POST /api/v1/approvals/{id}/snooze",
		"POST /api/v1/managed-changes/{id}/approve", "POST /api/v1/managed-changes/{id}/reject",
		"POST /api/v1/agents/{id}/grant-requests/{request}/approve",
		"POST /api/v1/agents/{id}/grant-requests/{request}/deny",
		"POST /api/v1/agents/{id}/grants", "POST /api/v1/agents/{id}/grants/{grant}/revoke",
		"POST /api/v1/emergency-stop", "POST /api/v1/resume",
	}
	seen := map[string]string{}
	for _, rc := range routerConfigs() {
		h := rc.build(t, injectCharRole)
		for p, s := range registeredScopes(h.(*http.ServeMux)) {
			if prev, ok := seen[p]; ok && prev != s {
				t.Errorf("%s: %s is %s here but %s elsewhere", rc.name, p, s, prev)
			}
			seen[p] = s
		}
	}
	for _, p := range want {
		if !strings.HasPrefix(seen[p], "database:") {
			t.Errorf("%s: scope %q, want a database resolver", p, seen[p])
		}
	}
}

func TestFleetWideRoutes_NoStaleEntries(t *testing.T) {
	registered := map[string]bool{}
	for _, rc := range routerConfigs() {
		h := rc.build(t, injectCharRole)
		for p, s := range registeredScopes(h.(*http.ServeMux)) {
			if s == scopeFleetWide {
				registered[p] = true
			}
		}
	}
	for p, reason := range fleetWideRoutes {
		if !registered[p] {
			t.Errorf("fleetWideRoutes lists %s, which no router registers as a "+
				"fleet-wide operator write", p)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("fleetWideRoutes[%s] has no reason", p)
		}
	}
}

func TestRouterTable_ScopesMatchGolden(t *testing.T) {
	var out bytes.Buffer
	for _, rc := range routerConfigs() {
		h := rc.build(t, injectCharRole)
		scopes := registeredScopes(h.(*http.ServeMux))
		reads := registeredReadScopes(h.(*http.ServeMux)) // apiauth-08b
		for _, e := range builtRoutes(t, h, rc.name) {
			scope := scopes[e.Pattern]
			if scope == "" {
				scope = reads[e.Pattern]
			}
			if scope == "" {
				scope = "-"
			}
			fmt.Fprintf(&out, "%s %s gate:%s scope:%s\n", rc.name, e.Pattern, e.Gate, scope)
		}
	}
	if *updateRouterGolden {
		if err := os.WriteFile(routeScopesGoldenPath, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(routeScopesGoldenPath)
	if err != nil {
		t.Fatalf("read %s (run with -update-router-golden): %v", routeScopesGoldenPath, err)
	}
	diffRouterTables(t, strings.ReplaceAll(string(want), "\r\n", "\n"), out.String())
}

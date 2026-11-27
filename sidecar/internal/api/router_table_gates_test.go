package api

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The registry-driven half of the router table: every route a router
// records is probed as nobody (through the real session middleware),
// viewer, operator and admin, and must answer as its declared gate says.
// No DB or concurrency test here: registration happens once per router
// build, and the probes run against an unreachable pool.

// charSessionAuth injects the X-Char-Role identity when the header is set
// and otherwise runs the production session middleware (nil pool: a
// request without a cookie is 401), so one router answers both.
func charSessionAuth(next http.Handler) http.Handler {
	session := SessionAuthMiddleware(nil)(next)
	inject := injectCharRole(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Char-Role") != "" {
			inject.ServeHTTP(w, r)
			return
		}
		session.ServeHTTP(w, r)
	})
}

var patternWildcard = regexp.MustCompile(`\{[^}]*\}`)

// probeForPattern turns a ServeMux pattern into a request that matches it:
// wildcards become "7", {$} matches the bare prefix, and a pattern without
// a method is probed with GET.
func probeForPattern(pattern string) routeProbe {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		method, path = http.MethodGet, pattern
	}
	path = patternWildcard.ReplaceAllStringFunc(path, func(w string) string {
		if w == "{$}" {
			return ""
		}
		return "7"
	})
	return routeProbe{method, path}
}

func probeKey(p routeProbe) string {
	path, _, _ := strings.Cut(p.path, "?")
	return p.method + " " + path
}

func builtRoutes(t *testing.T, h http.Handler, config string) []routeEntry {
	t.Helper()
	root, ok := h.(*http.ServeMux)
	if !ok {
		t.Fatalf("%s: router is %T, want *http.ServeMux", config, h)
	}
	routes := registeredRoutes(root)
	if len(routes) < 20 {
		t.Fatalf("%s: registry holds %d routes; route() is not recording", config, len(routes))
	}
	return routes
}

func isGated(g routeGate) bool {
	return g == gateViewer || g == gateViewerWrite || g == gateViewerWriteOrToken ||
		g == gateOperator || g == gateAdmin
}

// checkProbeAgainstGate probes one request four ways and reports every
// answer that contradicts gate.
func checkProbeAgainstGate(t *testing.T, h http.Handler, label string, p routeProbe,
	gate routeGate) {
	t.Helper()
	code, body := serveProbe(h, p, "")
	if refusedForEveryone(code, body) {
		return
	}
	unauth := code == http.StatusUnauthorized && strings.Contains(body, "authentication required")
	if isGated(gate) && !unauth {
		t.Errorf("%s as nobody: %d %.80q, want 401 authentication required", label, code, body)
	}
	if !isGated(gate) && unauth {
		t.Errorf("%s (%s) as nobody: the session middleware refused it; "+
			"its path must skip session auth", label, gate)
	}
	denied := map[string]bool{
		"viewer":   gate == gateOperator || gate == gateAdmin,
		"operator": gate == gateAdmin,
		"admin":    false,
	}
	for _, role := range []string{"viewer", "operator", "admin"} {
		code, body := serveProbe(h, p, role)
		if refusedForEveryone(code, body) {
			continue
		}
		if got := isRoleDenied(code, body); got != denied[role] {
			t.Errorf("%s (%s) as %s: denied=%v (%d %.80q), want denied=%v",
				label, gate, role, got, code, body, denied[role])
		}
	}
}

// refusedForEveryone reports the Databricks App protocol guard, which
// refuses agent-token minting and the token transports before routing,
// whoever asks: fail closed, so the route's own gate is never reached.
func refusedForEveryone(code int, body string) bool {
	return code == http.StatusNotImplemented &&
		strings.Contains(body, "databricks_app_protocol_unsupported")
}

func TestRouterTable_RegistryGates(t *testing.T) {
	for _, rc := range routerConfigs() {
		h := rc.build(t, charSessionAuth)
		// Without the session middleware, a gated route must still refuse
		// nobody: the gate is on the route, not only in front of the mux.
		noSession := rc.build(t, injectCharRole)
		routes := builtRoutes(t, h, rc.name)
		for _, problem := range checkRouteGates(routes) {
			t.Errorf("%s: %s", rc.name, problem)
		}
		for _, e := range routes {
			p := probeForPattern(e.Pattern)
			label := rc.name + " " + e.Pattern
			checkProbeAgainstGate(t, h, label, p, e.Gate)
			if code, body := serveProbe(noSession, p, ""); isGated(e.Gate) &&
				!refusedForEveryone(code, body) && (code != http.StatusUnauthorized ||
				!strings.Contains(body, "authentication required")) {
				t.Errorf("%s without session middleware as nobody: %d %.80q, want the "+
					"route's own gate to answer 401", label, code, body)
			}
			if p.method == http.MethodGet {
				// GET patterns also answer HEAD, behind the same gate.
				checkProbeAgainstGate(t, h, label+" [HEAD]",
					routeProbe{http.MethodHead, p.path}, e.Gate)
			}
		}
	}
}

func TestRouterTable_OneGatePerPatternAcrossConfigs(t *testing.T) {
	// Optional dependencies change which routes exist, never their gate:
	// a router built with nil deps registers the same gated set.
	seen := map[string]routeGate{}
	where := map[string]string{}
	for _, rc := range routerConfigs() {
		for _, e := range builtRoutes(t, rc.build(t, injectCharRole), rc.name) {
			if g, ok := seen[e.Pattern]; ok && g != e.Gate {
				t.Errorf("%s: %s is %s here but %s in %s", rc.name, e.Pattern, e.Gate,
					g, where[e.Pattern])
				continue
			}
			seen[e.Pattern], where[e.Pattern] = e.Gate, rc.name
		}
	}
}

func TestRouterTable_ProbesCoverRegistry(t *testing.T) {
	golden := map[string]bool{}
	for _, p := range routeProbes {
		golden[probeKey(p)] = true
	}
	registry := map[string]bool{}
	for _, rc := range routerConfigs() {
		for _, e := range builtRoutes(t, rc.build(t, injectCharRole), rc.name) {
			registry[probeKey(probeForPattern(e.Pattern))] = true
		}
	}
	var missing []string
	covered := 0
	for key := range registry {
		if golden[key] {
			covered++
		} else {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("registered route %s has no golden probe in routeProbes", m)
	}
	if covered != len(registry) {
		t.Errorf("golden probes cover %d of %d registered routes", covered, len(registry))
	}
	t.Logf("golden probes cover %d of %d registered routes", covered, len(registry))
}

func TestRouterTable_ProbePatternBoundaries(t *testing.T) {
	cases := map[string]routeProbe{
		"GET /api/v1/debug/pprof/{$}":          {"GET", "/api/v1/debug/pprof/"},
		"GET /api/v1/debug/pprof/{name}":       {"GET", "/api/v1/debug/pprof/7"},
		"POST /api/v1/facts/{id}/confirm":      {"POST", "/api/v1/facts/7/confirm"},
		"GET /api/v1/databases/{db}/{rest...}": {"GET", "/api/v1/databases/7/7"},
		"/api/v1/specialist/":                  {"GET", "/api/v1/specialist/"},
	}
	for pattern, want := range cases {
		if got := probeForPattern(pattern); got != want {
			t.Errorf("probeForPattern(%q) = %v, want %v", pattern, got, want)
		}
	}
}

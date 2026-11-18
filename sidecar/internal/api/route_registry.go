package api

import (
	"net/http"
	"runtime"
	"strings"
	"sync"
	"weak"

	"github.com/pg-sage/sidecar/internal/auth"
)

// Route authorization is declared, not opted into. Every handler in this
// package is registered with route (or a sub-mux with mount), which wraps
// it with the gate it declares and records {pattern, gate} for the mux.
// Registering with mux.Handle or mux.HandleFunc directly fails
// TestNoBareAPIHandle, and the router table test probes every recorded
// route as nobody, viewer, operator and admin.

// routeGate is the authorization a route declares.
type routeGate int

const (
	gateUnset routeGate = iota
	// gatePublic needs no identity: login, OAuth, health, the dashboard.
	// Its path must be in the session middleware's skip list.
	gatePublic
	// gateSelfAuth authenticates in the handler itself (MCP bearer tokens,
	// signed ingest, signed ChatOps callbacks, the specialist contract,
	// break-glass and link grants). Its path must skip session auth.
	gateSelfAuth
	// gateViewer allows any signed-in role; only on GET (and HEAD).
	gateViewer
	// gateViewerWrite allows any signed-in role on a write; only for the
	// patterns in viewerWriteRoutes.
	gateViewerWrite
	// gateViewerWriteOrToken allows any signed-in role, or instead a
	// read-scope MCP API token sent as `Authorization: Bearer`, on a write;
	// only for the patterns in viewerWriteOrTokenRoutes, and only for a
	// handler built by newTokenOrSession (any other is session-only).
	gateViewerWriteOrToken
	// gateOperator allows operators and admins (on a plan route, or
	// instead an operator API token with the approve scope whose owner is
	// an operator or admin now: plan_token.go).
	gateOperator
	// gateAdmin allows admins only.
	gateAdmin
)

var gateNames = map[routeGate]string{
	gatePublic:             "public",
	gateSelfAuth:           "self-auth",
	gateViewer:             "viewer",
	gateViewerWrite:        "viewer-write",
	gateViewerWriteOrToken: "viewer-write-or-token",
	gateOperator:           "operator",
	gateAdmin:              "admin",
}

func (g routeGate) String() string {
	if name, ok := gateNames[g]; ok {
		return name
	}
	return "unset"
}

// viewerWriteRoutes is the documented exception list: writes any
// signed-in user may make, each with the reason it is safe.
var viewerWriteRoutes = map[string]string{
	"POST /api/v1/auth/logout":     "ends the caller's own session",
	"DELETE /api/v1/auth/sessions": "signs out the caller's own other sessions",
	"PUT /api/v1/auth/password": "changes the caller's own password after checking " +
		"the current one (apiauth-03)",
	"POST " + askBasePath: "Ask Sage answers from read-only tools and cannot " +
		"execute or approve anything; the conversation is the caller's own",
	"POST " + askBasePath + "/runs": "an asynchronous Ask Sage question (mcp-07b): the " +
		"same read-only tools and gates as the question route; the run is the caller's own",
	"POST " + askBasePath + "/runs/{id}/cancel": "cancels the caller's own Ask Sage run; " +
		"another actor's run is not found (mcp-07b)",
	"POST " + inboxSeenPath: "moves forward only the caller's own Inbox seen time on " +
		"their user row (ux-01b); it changes no database and decides nothing",
	"POST " + reviewViewedPath: "records only the caller's own first opening of a " +
		"finished review in sage.review_view (review-01b); it decides nothing and " +
		"changes no monitored data",
}

// viewerWriteOrTokenRoutes lists the writes a signed-in user or a
// read-scope API token may make (gateViewerWriteOrToken), each with the
// reason it is safe. Their Bearer requests skip session auth.
var viewerWriteOrTokenRoutes = map[string]string{
	"POST " + migrationLintPath: "migration lint never runs the SQL: it reads " +
		"catalog statistics under a 2 s bound and changes nothing (migration-11); " +
		"CI calls it with a read token (migration-11b)",
}

var (
	viewerRoles   = []string{"admin", "operator", "viewer"}
	operatorRoles = []string{"admin", "operator"}
	adminRoles    = []string{"admin"}
)

// wrap applies the gate to h. Public and self-authenticating routes are
// returned unchanged; an unknown gate fails closed for every request.
func (g routeGate) wrap(h http.Handler) http.Handler {
	// A plan route's handler gates its session requests itself and takes
	// an operator API token instead (plan-03a, plan_token.go).
	if ts, ok := h.(*planTokenOrSession); ok && ts.gate == g {
		return ts
	}
	// The pressure route gates its session requests itself and takes a
	// read-scope API token instead (guard-01a, pressure_routes.go).
	if ts, ok := h.(*readTokenOrSession); ok && g == gateViewer {
		return ts
	}
	switch g {
	case gatePublic, gateSelfAuth:
		return h
	case gateViewer, gateViewerWrite:
		return RequireRole(viewerRoles...)(h)
	case gateViewerWriteOrToken:
		if ts, ok := h.(*tokenOrSession); ok {
			return ts
		}
		return RequireRole(viewerRoles...)(h)
	case gateOperator:
		return RequireRole(operatorRoles...)(h)
	case gateAdmin:
		return RequireRole(adminRoles...)(h)
	}
	return RequireRole()(h)
}

// routeEntry is one registered route and the gate it declared.
type routeEntry struct {
	Pattern string
	Gate    routeGate
}

// muxRoutes is the registry of one mux: its own routes, their database
// scopes (apiauth-08: writes; apiauth-08b: reads) and the registries of
// the muxes mounted under it.
type muxRoutes struct {
	mu      sync.Mutex
	entries []routeEntry
	scopes  map[string]string
	reads   map[string]string
	mounts  []*muxRoutes
}

var (
	registriesMu sync.Mutex
	registries   = map[weak.Pointer[http.ServeMux]]*muxRoutes{}
)

// registryFor returns the registry of mux, creating it on first use. The
// entry is dropped when mux is garbage collected, so routers built and
// discarded (tests) do not accumulate.
func registryFor(mux *http.ServeMux) *muxRoutes {
	key := weak.Make(mux)
	registriesMu.Lock()
	defer registriesMu.Unlock()
	if reg, ok := registries[key]; ok {
		return reg
	}
	reg := &muxRoutes{}
	registries[key] = reg
	runtime.AddCleanup(mux, func(k weak.Pointer[http.ServeMux]) {
		registriesMu.Lock()
		delete(registries, k)
		registriesMu.Unlock()
	}, key)
	return reg
}

// route registers h on mux under pattern behind gate and records it. An
// operator write registered here is fleet-wide (apiauth-08): with database
// grants on, only an operator with no grants may use it. A route that
// acts on one database registers with routeDB instead. A GET behind a
// signed-in gate takes its read scope from readRouteScopes (apiauth-08b,
// route_read.go); one whose handler aggregates through the fleet
// registers with routeAggregate instead.
func route(mux *http.ServeMux, pattern string, gate routeGate, h http.Handler) {
	if gate == gateOperator && isWriteRoute(pattern) {
		registerRoute(mux, pattern, gate, scopeFleetWide,
			requireDatabaseRole(auth.RoleOperator, fleetWide)(h))
		return
	}
	if isUserRead(pattern, gate) {
		routeUserRead(mux, pattern, gate, h)
		return
	}
	registerRoute(mux, pattern, gate, "", h)
}

// routeDB registers an operator write that acts on the one database res
// resolves: with database grants on, the caller needs operator there.
func routeDB(mux *http.ServeMux, pattern string, gate routeGate, res databaseResolver,
	h http.Handler) {
	registerRoute(mux, pattern, gate, "database:"+res.name,
		requireDatabaseRole(auth.RoleOperator, res)(h))
}

// scopeFleetWide marks an operator write that spans databases.
const scopeFleetWide = "fleet-wide"

func isWriteRoute(pattern string) bool {
	method, _, ok := strings.Cut(pattern, " ")
	return !ok || (method != http.MethodGet && method != http.MethodHead)
}

func registerRoute(mux *http.ServeMux, pattern string, gate routeGate, scope string,
	h http.Handler) {
	mux.Handle(pattern, gate.wrap(h))
	reg := registryFor(mux)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.entries = append(reg.entries, routeEntry{Pattern: pattern, Gate: gate})
	if scope != "" {
		if reg.scopes == nil {
			reg.scopes = map[string]string{}
		}
		reg.scopes[pattern] = scope
	}
}

// recordReadScope records pattern's read scope (apiauth-08b).
func recordReadScope(mux *http.ServeMux, pattern, scope string) {
	reg := registryFor(mux)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.reads == nil {
		reg.reads = map[string]string{}
	}
	reg.reads[pattern] = scope
}

// registeredReadScopes maps each GET route of mux (and its mounts) that
// declared a read scope to it.
func registeredReadScopes(mux *http.ServeMux) map[string]string {
	out := map[string]string{}
	registryFor(mux).collectReads(out)
	return out
}

func (reg *muxRoutes) collectReads(out map[string]string) {
	reg.mu.Lock()
	for p, s := range reg.reads {
		out[p] = s
	}
	mounts := append([]*muxRoutes(nil), reg.mounts...)
	reg.mu.Unlock()
	for _, m := range mounts {
		m.collectReads(out)
	}
}

// registeredScopes maps each scoped route of mux (and its mounts) to its
// database scope: "fleet-wide" or "database:<resolver>".
func registeredScopes(mux *http.ServeMux) map[string]string {
	out := map[string]string{}
	registryFor(mux).collectScopes(out)
	return out
}

func (reg *muxRoutes) collectScopes(out map[string]string) {
	reg.mu.Lock()
	for p, s := range reg.scopes {
		out[p] = s
	}
	mounts := append([]*muxRoutes(nil), reg.mounts...)
	reg.mu.Unlock()
	for _, m := range mounts {
		m.collectScopes(out)
	}
}

// mount serves h (child wrapped by its middleware chain) on parent under
// pattern; child's routes carry their own gates and join parent's
// registry.
func mount(parent *http.ServeMux, pattern string, child *http.ServeMux, h http.Handler) {
	parent.Handle(pattern, h)
	reg := registryFor(parent)
	childReg := registryFor(child)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, m := range reg.mounts {
		if m == childReg {
			return
		}
	}
	reg.mounts = append(reg.mounts, childReg)
}

// registeredRoutes lists the routes recorded for mux and every mux
// mounted under it, in registration order.
func registeredRoutes(mux *http.ServeMux) []routeEntry {
	return registryFor(mux).collect()
}

func (reg *muxRoutes) collect() []routeEntry {
	reg.mu.Lock()
	out := append([]routeEntry(nil), reg.entries...)
	mounts := append([]*muxRoutes(nil), reg.mounts...)
	reg.mu.Unlock()
	for _, m := range mounts {
		out = append(out, m.collect()...)
	}
	return out
}

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
	// gateOperator allows operators and admins.
	gateOperator
	// gateAdmin allows admins only.
	gateAdmin
)

var gateNames = map[routeGate]string{
	gatePublic:      "public",
	gateSelfAuth:    "self-auth",
	gateViewer:      "viewer",
	gateViewerWrite: "viewer-write",
	gateOperator:    "operator",
	gateAdmin:       "admin",
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
}

var (
	viewerRoles   = []string{"admin", "operator", "viewer"}
	operatorRoles = []string{"admin", "operator"}
	adminRoles    = []string{"admin"}
)

// wrap applies the gate to h. Public and self-authenticating routes are
// returned unchanged; an unknown gate fails closed for every request.
func (g routeGate) wrap(h http.Handler) http.Handler {
	switch g {
	case gatePublic, gateSelfAuth:
		return h
	case gateViewer, gateViewerWrite:
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
// scopes (apiauth-08) and the registries of the muxes mounted under it.
type muxRoutes struct {
	mu      sync.Mutex
	entries []routeEntry
	scopes  map[string]string
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
// acts on one database registers with routeDB instead.
func route(mux *http.ServeMux, pattern string, gate routeGate, h http.Handler) {
	if gate == gateOperator && isWriteRoute(pattern) {
		registerRoute(mux, pattern, gate, scopeFleetWide,
			requireDatabaseRole(auth.RoleOperator, fleetWide)(h))
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

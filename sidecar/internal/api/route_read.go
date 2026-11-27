package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Per-database reads (apiauth-08b, apiauth-08 phase 2). With
// api.database_grants on in fleet or meta-db mode, a user scoped by grants
// (auth.VisibleDatabases) reads only the granted databases. Every GET
// route a signed-in user can reach declares how it filters:
//
//   - database:<resolver>: the route reads the one database the resolver
//     names. A database outside the visible set answers exactly like an
//     unknown database (404 "database not found"), so a read is no
//     existence oracle; naming none (or "all") is 403 fleet_scope_required.
//   - aggregate: "" and "all" mean every visible database. routeAggregate
//     builds the handler over fleet.ReadView, so lists, counts and
//     summaries cover the visible set only; a named database outside it is
//     404 as above. The SSE stream filters its events itself (readStream).
//   - global: no database data (the caller's own identity, static lists).
//   - fleet-only: the route cannot be filtered; a scoped user gets 403
//     fleet_scope_required.
//
// Unscoped users (no grants, admins, grants off, standalone) pass through
// untouched. A GET registered with route() that declares no scope fails
// closed as fleet-only and fails TestRouteReadScopes_EveryReadDeclaresScope.
// Writes keep phase 1's rules (requireDatabaseRole).

const (
	readKindDatabase  = "database"
	readKindAggregate = "aggregate"
	readKindGlobal    = "global"
	readKindFleetOnly = "fleet-only"
	readKindAdminOnly = "admin-only"
)

// readScope is how a GET route filters for a user scoped by grants.
type readScope struct {
	kind string
	res  databaseResolver // readKindDatabase only
}

func (s readScope) String() string {
	if s.kind == readKindDatabase {
		return "database:" + s.res.name
	}
	return s.kind
}

var (
	readGlobal    = readScope{kind: readKindGlobal}
	readFleetOnly = readScope{kind: readKindFleetOnly}
	// readStream is an aggregate route whose handler filters with the
	// visible set from the request context (the SSE stream).
	readStream = readScope{kind: readKindAggregate}
)

// readDatabase is the scope of a route that reads the database res names.
func readDatabase(res databaseResolver) readScope {
	return readScope{kind: readKindDatabase, res: res}
}

// routeRead registers a GET route with its read scope.
func routeRead(mux *http.ServeMux, pattern string, gate routeGate, scope readScope,
	h http.Handler) {
	registerRoute(mux, pattern, gate, "", requireReadScope(scope)(h))
	recordReadScope(mux, pattern, scope.String())
}

// routeAggregate registers a GET route whose handler reads the fleet only
// through mgr: build(mgr) serves unscoped callers; a scoped caller's
// request is served by build over a view holding their visible databases.
func routeAggregate(mux *http.ServeMux, pattern string, gate routeGate,
	mgr *fleet.DatabaseManager, build func(*fleet.DatabaseManager) http.Handler) {
	routeRead(mux, pattern, gate, readScope{kind: readKindAggregate}, aggregateRead(mgr, build))
}

// aggregateRead serves unscoped callers with build(mgr) and a scoped
// caller with build over a view holding their visible databases.
func aggregateRead(mgr *fleet.DatabaseManager,
	build func(*fleet.DatabaseManager) http.Handler) http.Handler {
	whole := build(mgr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visible, scoped := visibleDatabasesFrom(r.Context())
		switch {
		case !scoped:
			whole.ServeHTTP(w, r)
		case mgr == nil:
			writeFleetScopeRequired(w)
		default:
			build(mgr.ReadView(visible)).ServeHTTP(w, r)
		}
	})
}

// aggregateHandler adapts a handler factory to routeAggregate.
func aggregateHandler(f func(*fleet.DatabaseManager) http.HandlerFunc) func(
	*fleet.DatabaseManager) http.Handler {
	return func(m *fleet.DatabaseManager) http.Handler { return f(m) }
}

// routeUserRead registers a GET route with route(): an admin route is
// admin-only; any other takes its scope from readRouteScopes, and one
// with none fails closed as fleet-only.
func routeUserRead(mux *http.ServeMux, pattern string, gate routeGate, h http.Handler) {
	if gate == gateAdmin {
		registerRoute(mux, pattern, gate, "", h)
		recordReadScope(mux, pattern, readKindAdminOnly)
		return
	}
	scope, ok := readRouteScopes[pattern]
	if !ok {
		registerRoute(mux, pattern, gate, "", requireReadScope(readFleetOnly)(h))
		return
	}
	routeRead(mux, pattern, gate, scope, h)
}

// isUserRead reports a GET route behind a signed-in role gate.
func isUserRead(pattern string, gate routeGate) bool {
	if isWriteRoute(pattern) {
		return false
	}
	return gate == gateViewer || gate == gateOperator || gate == gateAdmin
}

const visibleDatabasesKey contextKey = "sage_visible_databases"

// visibleDatabasesFrom returns the scoped caller's visible databases; ok
// is false for an unscoped caller (no filter).
func visibleDatabasesFrom(ctx context.Context) (map[string]bool, bool) {
	set, ok := ctx.Value(visibleDatabasesKey).(map[string]bool)
	return set, ok
}

// requireReadScope enforces s for a scoped caller and puts their visible
// set in the request context. It runs after the route's role gate.
func requireReadScope(s readScope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if s.kind == readKindGlobal {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ctx, ok := scopeRead(w, r, s); ok {
				next.ServeHTTP(w, r.WithContext(ctx))
			}
		})
	}
}

func scopeRead(w http.ResponseWriter, r *http.Request, s readScope) (context.Context, bool) {
	ctx := r.Context()
	a, user := databaseAuthzFrom(ctx), UserFromContext(ctx)
	if a == nil || user == nil || user.Role == auth.RoleAdmin {
		return ctx, true
	}
	visible, scoped, err := a.visibleTo(ctx, user)
	if err != nil {
		slog.Error("database grant check failed; refusing the read",
			"user_id", user.ID, "path", r.URL.Path, "error", err)
		writeGrantStoreUnavailable(w)
		return nil, false
	}
	if !scoped {
		return ctx, true
	}
	if !allowScopedRead(w, r, s, visible) {
		return nil, false
	}
	return context.WithValue(ctx, visibleDatabasesKey, visible), true
}

// allowScopedRead applies s to a scoped caller, answering refusals itself.
func allowScopedRead(w http.ResponseWriter, r *http.Request, s readScope,
	visible map[string]bool) bool {
	switch s.kind {
	case readKindDatabase:
		name := ""
		if s.res.resolve != nil {
			name = s.res.resolve(r)
		}
		if name == "" {
			writeFleetScopeRequired(w)
			return false
		}
		return allowNamedRead(w, name, visible)
	case readKindAggregate:
		return allowNamedRead(w, oneDatabase(r.URL.Query().Get("database")), visible)
	}
	writeFleetScopeRequired(w)
	return false
}

// allowNamedRead refuses a malformed name (400, as the handlers do) and a
// database outside visible (404, the unknown-database answer); "" passes.
func allowNamedRead(w http.ResponseWriter, name string, visible map[string]bool) bool {
	if name == "" {
		return true
	}
	if err := validateDatabaseParam(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if !visible[name] {
		jsonError(w, "database not found", http.StatusNotFound)
		return false
	}
	return true
}

// visibleTo is user's visible set; scoped is false when no filter applies.
func (a *databaseAuthz) visibleTo(ctx context.Context, user *auth.User) (map[string]bool,
	bool, error) {
	grants, err := a.grants.UserDatabaseGrants(ctx, user.ID)
	if err != nil {
		return nil, false, err
	}
	set, scoped := auth.VisibleDatabases(user.Role, grants)
	return set, scoped, nil
}

func writeFleetScopeRequired(w http.ResponseWriter) {
	sreErrorCode(w, "this view spans databases and you can read only the databases "+
		"you are granted; open one of your databases, or ask an admin",
		"fleet_scope_required", http.StatusForbidden)
}

// validReadScope reports a scope a signed-in user's GET route may declare.
func validReadScope(scope string) bool {
	switch scope {
	case readKindAggregate, readKindGlobal, readKindFleetOnly:
		return true
	}
	res, ok := strings.CutPrefix(scope, readKindDatabase+":")
	return ok && res != ""
}

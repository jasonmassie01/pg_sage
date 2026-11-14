package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Per-database authorization (apiauth-08, phase 1: writes). With
// api.database_grants on in fleet or meta-db mode, an operator write that
// names a database (routeDB) needs the caller's role on that database, and
// an operator write that spans databases needs an operator with no
// grants. The rules are auth.EffectiveDatabaseRole's. Reads are scoped by
// route_read.go (apiauth-08b). The authorizer travels in the request context;
// without it (grants off, standalone, embedders) every check passes, which
// is today's behaviour.

// DatabaseGrantSource reads a user's grants as database → role.
type DatabaseGrantSource interface {
	UserDatabaseGrants(ctx context.Context, userID int) (map[string]string, error)
}

// databaseAuthz enforces database grants; nil means grants are off.
type databaseAuthz struct{ grants DatabaseGrantSource }

// newDatabaseAuthz returns the authorizer for cfg, or nil when grants are
// off or the mode has one database. src overrides the control-pool store;
// with neither, every scoped check fails closed.
func newDatabaseAuthz(cfg *config.Config, pool *pgxpool.Pool,
	src DatabaseGrantSource) *databaseAuthz {
	if cfg == nil || !cfg.API.DatabaseGrants || !multiDatabaseMode(cfg) {
		return nil
	}
	if src == nil {
		src = auth.NewDatabaseGrantStore(pool)
	}
	return &databaseAuthz{grants: src}
}

// routerDatabaseAuthz is newDatabaseAuthz, saying once per router when the
// switch is on in a mode that ignores it.
func routerDatabaseAuthz(cfg *config.Config, pool *pgxpool.Pool,
	src DatabaseGrantSource) *databaseAuthz {
	a := newDatabaseAuthz(cfg, pool, src)
	if a == nil && cfg != nil && cfg.API.DatabaseGrants {
		slog.Info("api.database_grants is on but this mode monitors one database; " +
			"grants are not enforced")
	}
	return a
}

func multiDatabaseMode(cfg *config.Config) bool {
	return cfg.Mode == "fleet" || cfg.Mode == "meta" || cfg.HasMetaDB()
}

const databaseAuthzKey contextKey = "sage_database_authz"

// withDatabaseAuthz puts a into every request's context for the route
// wrappers; a nil a leaves next unchanged.
func withDatabaseAuthz(a *databaseAuthz, next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), databaseAuthzKey, a)))
	})
}

func databaseAuthzFrom(ctx context.Context) *databaseAuthz {
	a, _ := ctx.Value(databaseAuthzKey).(*databaseAuthz)
	return a
}

// roleOn is user's role on database ("" = an action spanning databases).
// An admin is never scoped, so their grants are not read.
func (a *databaseAuthz) roleOn(ctx context.Context, user *auth.User,
	database string) (string, error) {
	if user.Role == auth.RoleAdmin {
		return auth.RoleAdmin, nil
	}
	grants, err := a.grants.UserDatabaseGrants(ctx, user.ID)
	if err != nil {
		return "", err
	}
	return auth.EffectiveDatabaseRole(user.Role, grants, database), nil
}

// allow reports whether user holds min on database, answering the refusal
// (403) or the store failure (503) itself.
func (a *databaseAuthz) allow(w http.ResponseWriter, r *http.Request, user *auth.User,
	database, min string) bool {
	role, err := a.roleOn(r.Context(), user, database)
	if err != nil {
		slog.Error("database grant check failed; refusing the request",
			"user_id", user.ID, "database", database, "error", err)
		writeGrantStoreUnavailable(w)
		return false
	}
	if auth.RoleAtLeast(role, min) {
		return true
	}
	writeDatabaseForbidden(w, database, min)
	return false
}

func writeGrantStoreUnavailable(w http.ResponseWriter) {
	sreErrorCode(w, "database grants cannot be checked right now; try again",
		"grant_store_unavailable", http.StatusServiceUnavailable)
}

func writeDatabaseForbidden(w http.ResponseWriter, database, min string) {
	msg := "this action spans databases: it needs an " + min +
		" who is not limited to some databases"
	if database != "" {
		msg = fmt.Sprintf("you are not an %s on database %q; ask an admin for a grant",
			min, database)
	}
	sreErrorCode(w, msg, "database_forbidden", http.StatusForbidden)
}

// databaseResolver names the one database a request acts on, read exactly
// where its handler reads it; "" when the request names none or all.
type databaseResolver struct {
	name    string
	resolve func(*http.Request) string
}

func oneDatabase(name string) string {
	if name == "all" {
		return ""
	}
	return name
}

// queryDatabase reads ?database=.
var queryDatabase = databaseResolver{name: "query", resolve: func(r *http.Request) string {
	return oneDatabase(r.URL.Query().Get("database"))
}}

// bodyDatabase reads the JSON body's "database" with the handlers' own
// decoder (so a duplicate key resolves as the handler reads it) and puts
// the body back for the handler, read error included.
var bodyDatabase = databaseResolver{name: "body", resolve: func(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(r.Body)
	var rest io.Reader = bytes.NewReader(raw)
	if err != nil {
		rest = io.MultiReader(rest, errorReader{err})
	}
	r.Body = io.NopCloser(rest)
	if err != nil {
		return ""
	}
	var v struct {
		Database string `json:"database"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return oneDatabase(v.Database)
}}

type errorReader struct{ err error }

func (e errorReader) Read([]byte) (int, error) { return 0, e.err }

// approvalCardDatabase reads ?database=, or the fleet's only database when
// it is omitted, as approvalCardHandlers.cardTarget and the facts' target do.
func approvalCardDatabase(mgr *fleet.DatabaseManager) databaseResolver {
	return databaseResolver{name: "query-or-only", resolve: func(r *http.Request) string {
		db := oneDatabase(r.URL.Query().Get("database"))
		if db != "" || mgr == nil {
			return db
		}
		if instances := sortedFleetInstances(mgr); len(instances) == 1 {
			return instances[0].Name
		}
		return ""
	}}
}

// pathDatabase reads the {key} path value.
func pathDatabase(key string) databaseResolver {
	return databaseResolver{name: "path:" + key, resolve: func(r *http.Request) string {
		return oneDatabase(r.PathValue(key))
	}}
}

var (
	pathDB  = pathDatabase("db")       // /api/v1/databases/{db}/...
	classDB = pathDatabase("database") // /api/v1/agent-classifications/{database}/...
)

// fleetWide resolves no database: only an unscoped caller passes.
var fleetWide = databaseResolver{name: scopeFleetWide}

// requireDatabaseRole refuses a caller below min on the database res
// resolves. It runs after the route's role gate.
func requireDatabaseRole(min string, res databaseResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a, user := databaseAuthzFrom(r.Context()), UserFromContext(r.Context())
			if a == nil || user == nil {
				next.ServeHTTP(w, r)
				return
			}
			database := ""
			if res.resolve != nil && user.Role != auth.RoleAdmin {
				database = res.resolve(r)
			}
			if a.allow(w, r, user, database, min) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

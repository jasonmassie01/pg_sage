package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The admin grant editor (apiauth-08):
//
//	GET    /api/v1/users/{id}/database-grants            → {grants, enforced, scoped}
//	PUT    /api/v1/users/{id}/database-grants/{database} {role: viewer|operator}
//	DELETE /api/v1/users/{id}/database-grants/{database}
//
// A grant must name a database the fleet monitors. Every change is written
// to sage.auth_audit in the same transaction. enforced says whether
// api.database_grants is on in this mode: grants are stored either way.
// idpManaged says oauth.database_grant_mapping is set (apiauth-08d): the
// grants it wrote (source idp) cannot be removed here, since the next
// login would restore them; a PUT turns one into a manual grant.

type databaseGrantHandlers struct {
	store      *auth.DatabaseGrantStore
	mgr        *fleet.DatabaseManager
	enforced   bool
	idpManaged bool
}

func registerDatabaseGrantRoutes(mux *http.ServeMux, pool *pgxpool.Pool,
	mgr *fleet.DatabaseManager, enforced, idpManaged bool) {
	h := databaseGrantHandlers{store: auth.NewDatabaseGrantStore(pool), mgr: mgr,
		enforced: enforced, idpManaged: idpManaged}
	const base = "/api/v1/users/{id}/database-grants"
	route(mux, "GET "+base, gateAdmin, http.HandlerFunc(h.list))
	route(mux, "PUT "+base+"/{database}", gateAdmin, http.HandlerFunc(h.set))
	route(mux, "DELETE "+base+"/{database}", gateAdmin, http.HandlerFunc(h.revoke))
}

func grantUserID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		sreErrorCode(w, "invalid user id", "invalid_request", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func (h databaseGrantHandlers) list(w http.ResponseWriter, r *http.Request) {
	id, ok := grantUserID(w, r)
	if !ok {
		return
	}
	grants, err := h.store.ListUserGrants(r.Context(), id)
	if err != nil {
		writeGrantChangeError(w, r, err)
		return
	}
	if grants == nil {
		grants = []auth.DatabaseGrant{}
	}
	jsonResponse(w, map[string]any{"user_id": id, "grants": grants,
		"enforced": h.enforced, "scoped": len(grants) > 0, "idp_managed": h.idpManaged})
}

// change reads the user id and the {database}. A new grant must name a
// monitored database; a revoke need not, so a grant on a database that
// has left the fleet (which keeps its user scoped) can still be removed.
func (h databaseGrantHandlers) change(w http.ResponseWriter, r *http.Request,
	monitored bool) (auth.GrantChange, bool) {
	id, ok := grantUserID(w, r)
	if !ok {
		return auth.GrantChange{}, false
	}
	db := r.PathValue("database")
	known := h.mgr != nil && h.mgr.GetInstance(db) != nil
	if db == "all" || validateDatabaseParam(db) != nil || (monitored && !known) {
		sreErrorCode(w, "unknown database: a grant must name a database pg_sage monitors",
			"unknown_database", http.StatusBadRequest)
		return auth.GrantChange{}, false
	}
	return auth.GrantChange{ActorID: userID(r), UserID: id, Database: db,
		SourceIP: requestClientIP(r)}, true
}

func (h databaseGrantHandlers) set(w http.ResponseWriter, r *http.Request) {
	c, ok := h.change(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sreErrorCode(w, "invalid JSON body", "invalid_request", http.StatusBadRequest)
		return
	}
	c.Role = body.Role
	g, err := h.store.SetGrant(r.Context(), c)
	if err != nil {
		writeGrantChangeError(w, r, err)
		return
	}
	jsonResponse(w, g)
}

func (h databaseGrantHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	c, ok := h.change(w, r, false)
	if !ok {
		return
	}
	c.ProtectIdP = h.idpManaged
	if err := h.store.RevokeGrant(r.Context(), c); err != nil {
		writeGrantChangeError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"revoked": true, "user_id": c.UserID,
		"database": c.Database})
}

func writeGrantChangeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidGrantRole):
		sreErrorCode(w, "role must be viewer or operator", "invalid_role",
			http.StatusBadRequest)
	case errors.Is(err, auth.ErrInvalidGrantDatabase):
		sreErrorCode(w, err.Error(), "unknown_database", http.StatusBadRequest)
	case errors.Is(err, auth.ErrGrantUserNotFound):
		sreErrorCode(w, "user not found", "not_found", http.StatusNotFound)
	case errors.Is(err, auth.ErrGrantNotFound):
		sreErrorCode(w, "the user has no grant on this database", "not_found",
			http.StatusNotFound)
	case errors.Is(err, auth.ErrIdPGrantReadOnly):
		sreErrorCode(w, "this grant comes from the SSO group mapping: change the user's IdP "+
			"groups or oauth.database_grant_mapping, or set a manual grant to override it",
			"idp_grant", http.StatusConflict)
	case errors.Is(err, auth.ErrConfigGrantReadOnly):
		sreErrorCode(w, "this grant is set by a file under policy.directory (cli-02a): "+
			"edit the file in its repository", "config_managed", http.StatusConflict)
	case errors.Is(err, auth.ErrGrantStoreUnavailable):
		writeGrantStoreUnavailable(w)
	default:
		internalError(w, r, "database grant", err)
	}
}

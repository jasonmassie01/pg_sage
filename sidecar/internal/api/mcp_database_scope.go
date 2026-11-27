package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// Per-database MCP reads by people (apiauth-08c, apiauth-08 phase 2).
// With api.database_grants on in fleet or meta-db mode, a person's MCP
// token (an operator token) reads only the databases on its own list that
// its owner may read now: the list is intersected with the owner's
// visible set (auth.VisibleDatabases) on every request, so a revoke
// applies at the next call and a grant added later widens the token up
// to its own list, never beyond. Agent tokens keep their own lists. A
// person's token with no owner reads no database while grants are on.

// scopeMCPPrincipal narrows a person's principal to the databases its
// owner may read. On a grant store failure it returns the error and a
// principal that reads nothing and is marked unavailable, never a wider
// one.
func scopeMCPPrincipal(ctx context.Context, a *databaseAuthz, p mcp.Principal) (
	mcp.Principal, error) {
	if a == nil || p.Kind == mcp.KindAgent || p.Role == auth.RoleAdmin {
		return p, nil
	}
	if p.OwnerUserID <= 0 {
		p.Databases = []string{}
		return p, nil
	}
	grants, err := a.grants.UserDatabaseGrants(ctx, p.OwnerUserID)
	if err != nil {
		p.Databases, p.DatabasesUnavailable = []string{}, true
		return p, err
	}
	if visible, scoped := auth.VisibleDatabases(p.Role, grants); scoped {
		p.Databases = visibleTokenDatabases(p.Databases, visible)
	}
	return p, nil
}

// visibleTokenDatabases is the token's list (nil: every database) limited
// to visible; never nil, so an empty result reads nothing.
func visibleTokenDatabases(list []string, visible map[string]bool) []string {
	out := []string{}
	if list == nil {
		for db := range visible {
			out = append(out, db)
		}
		sort.Strings(out)
		return out
	}
	for _, db := range list {
		if visible[db] {
			out = append(out, db)
		}
	}
	return out
}

// mintWithinOwnerGrants refuses a person's token that names a database
// (or "*") its owner, scoped by grants, may not read: 400
// database_forbidden. It answers refusals itself. An unknown owner, or
// one who may not own a token, is left to the store's own refusal.
func mintWithinOwnerGrants(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool,
	body createMCPTokenBody) bool {
	a := databaseAuthzFrom(r.Context())
	if a == nil || mcptoken.Kind(body.Kind) != mcptoken.KindOperator ||
		body.OwnerUserID <= 0 {
		return true
	}
	visible, scoped, err := ownerVisibleDatabases(r.Context(), a, pool, body.OwnerUserID)
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		return true
	case err != nil:
		slog.Error("database grant check failed; refusing the token",
			"owner_user_id", body.OwnerUserID, "error", err)
		writeGrantStoreUnavailable(w)
		return false
	case !scoped:
		return true
	}
	for _, db := range body.Databases {
		if !visible[db] {
			writeMintDatabaseForbidden(w, body.OwnerUserID, db, visible)
			return false
		}
	}
	return true
}

// ownerVisibleDatabases is the token owner's visible set; scoped is false
// for an unscoped owner and for a role that cannot own a token.
func ownerVisibleDatabases(ctx context.Context, a *databaseAuthz, pool *pgxpool.Pool,
	owner int) (map[string]bool, bool, error) {
	if pool == nil {
		return nil, false, fmt.Errorf("%w: no control database pool",
			auth.ErrGrantStoreUnavailable)
	}
	role, err := auth.UserRole(ctx, pool, owner)
	if err != nil {
		return nil, false, err
	}
	if role != auth.RoleOperator {
		return nil, false, nil
	}
	grants, err := a.grants.UserDatabaseGrants(ctx, owner)
	if err != nil {
		return nil, false, err
	}
	visible, scoped := auth.VisibleDatabases(role, grants)
	return visible, scoped, nil
}

func writeMintDatabaseForbidden(w http.ResponseWriter, owner int, db string,
	visible map[string]bool) {
	names := visibleTokenDatabases(nil, visible)
	what := fmt.Sprintf("database %q", db)
	if db == "*" {
		what = `"*" (every database)`
	}
	sreErrorCode(w, fmt.Sprintf("user %d is limited to databases [%s]; their token "+
		"cannot name %s", owner, strings.Join(names, ", "), what),
		"database_forbidden", http.StatusBadRequest)
}

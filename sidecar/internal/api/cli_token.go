package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// API-token auth on the routes the decision-loop CLI calls (cli-01a), by
// the pattern of the plan routes (plan_token.go): `pg_sage_sidecar plan`,
// `approve`, `apply` and `explain` send `Authorization: Bearer <operator
// API token>`. The token is the only credential such a request carries (a
// session cookie beside it is ignored, a bad token is never retried as a
// session). Only an operator token qualifies: an agent token is refused
// by its kind, and on the approve route with approval_reserved_for_humans
// (agents never hold the approve scope; mcptoken and the SQL check refuse
// it too). The read routes need the read scope, the approve route the
// approve scope. The token's owner, with their current role and database
// grants, is the person the request acts as; the route's own gate then
// applies as for a signed-in user. A token limited to some databases must
// name one with ?database=. In a Databricks App the platform owns the
// Authorization header, so this is not mounted (router.go).

// cliTokenRoute is one route the CLI calls: its method, its path segments
// after /api/v1/ ("{id}" matches one positive id), the scope it needs,
// and whether it is an approval card route (where an agent token's
// refusal is approval_reserved_for_humans: the CLI reads the card it
// approves first, so the read refuses it the same way).
type cliTokenRoute struct {
	method   string
	segments []string
	scope    string
	approval bool
}

var cliTokenRoutes = []cliTokenRoute{
	{http.MethodGet, []string{"inbox"}, mcptoken.ScopeRead, false},
	{http.MethodGet, []string{"actions", "pending"}, mcptoken.ScopeRead, false},
	{http.MethodGet, []string{"whynot"}, mcptoken.ScopeRead, false},
	{http.MethodGet, []string{"approvals", "{id}"}, mcptoken.ScopeRead, true},
	{http.MethodPost, []string{"approvals", "{id}", "approve"}, mcptoken.ScopeApprove, true},
}

// cliTokenRouteOf returns the CLI route r asks for, ignoring credentials.
// A path with escapes that decode to a different spelling (RawPath set,
// as "%2F" sets it) never matches: the mux splits the escaped path, so
// "actions%2Fpending" would be read here as two segments but routed as
// one, and the CLI sends none.
func cliTokenRouteOf(r *http.Request) (cliTokenRoute, bool) {
	if r.URL.RawPath != "" {
		return cliTokenRoute{}, false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/")
	if !ok {
		return cliTokenRoute{}, false
	}
	parts := strings.Split(rest, "/")
	for _, route := range cliTokenRoutes {
		if route.method == r.Method && segmentsMatch(route.segments, parts) {
			return route, true
		}
	}
	return cliTokenRoute{}, false
}

func segmentsMatch(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		if w == "{id}" {
			if got[i] == "" || strings.Trim(got[i], "0123456789") != "" {
				return false
			}
			continue
		}
		if w != got[i] {
			return false
		}
	}
	return true
}

// isCLITokenRequest reports a Bearer request to a CLI route, which
// withCLIToken authenticates instead of the session middleware.
func isCLITokenRequest(r *http.Request) bool {
	if _, bearer := bearerCredential(r); !bearer {
		return false
	}
	_, ok := cliTokenRouteOf(r)
	return ok
}

// withCLIToken authenticates each CLI token request's token and serves it
// as the token's owner; every other request passes through unchanged.
func withCLIToken(tokens lintTokenValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, bearer := bearerCredential(r)
		route, ok := cliTokenRouteOf(r)
		if !bearer || !ok {
			next.ServeHTTP(w, r)
			return
		}
		user, p, ok := authenticateCLIToken(w, r, tokens, secret, route)
		if !ok || !allowTokenDatabase(w, r, p) {
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(mcp.WithPrincipal(ctx, p)))
	})
}

// authenticateCLIToken validates secret as an operator token holding
// scope, answering every refusal itself. The secret is never logged.
func authenticateCLIToken(w http.ResponseWriter, r *http.Request, tokens lintTokenValidator,
	secret string, route cliTokenRoute) (*auth.User, mcp.Principal, bool) {
	scope := route.scope
	if tokens == nil {
		jsonError(w, "token validation unavailable: no control database; sign in instead",
			http.StatusServiceUnavailable)
		return nil, mcp.Principal{}, false
	}
	if secret == "" {
		refuseLintToken(w)
		return nil, mcp.Principal{}, false
	}
	grant, err := tokens.Validate(r.Context(), secret)
	switch {
	case errors.Is(err, mcptoken.ErrUnauthorized):
		refuseLintToken(w)
		return nil, mcp.Principal{}, false
	case err != nil:
		slog.Error("CLI route token validation failed", "path", r.URL.Path, "err", err)
		w.Header().Set("Retry-After", "5")
		jsonError(w, "token validation unavailable", http.StatusServiceUnavailable)
		return nil, mcp.Principal{}, false
	case grant.Kind != mcptoken.KindOperator:
		refuseAgentToken(w, route.approval)
		return nil, mcp.Principal{}, false
	case !slices.Contains(grant.Scopes, scope):
		sreErrorCode(w, "the token lacks the "+scope+" scope this route needs",
			"scope_missing", http.StatusForbidden)
		return nil, mcp.Principal{}, false
	case grant.OwnerUserID <= 0 || !slices.Contains(viewerRoles, grant.OwnerRole):
		jsonError(w, "insufficient permissions", http.StatusForbidden)
		return nil, mcp.Principal{}, false
	}
	user := &auth.User{ID: grant.OwnerUserID, Role: grant.OwnerRole}
	return user, grantPrincipal(grant), true
}

// refuseAgentToken answers an agent token: an approval is a person's.
func refuseAgentToken(w http.ResponseWriter, approval bool) {
	if approval {
		sreErrorCode(w, "an agent token can never approve a change; a person approves "+
			"with an operator token", "approval_reserved_for_humans", http.StatusForbidden)
		return
	}
	sreErrorCode(w, "an agent token cannot use the decision routes; use an operator token",
		"agent_token", http.StatusForbidden)
}

// allowTokenDatabase refuses a database-limited token that names no
// database (403) or one it may not use (404, as an unknown database).
func allowTokenDatabase(w http.ResponseWriter, r *http.Request, p mcp.Principal) bool {
	if p.Databases == nil {
		return true
	}
	named := oneDatabase(r.URL.Query().Get("database"))
	switch {
	case named == "":
		sreErrorCode(w, "this token is limited to some databases: name one with ?database=",
			"database_required", http.StatusForbidden)
		return false
	case !p.MayUseDatabase(named):
		jsonError(w, "database not found", http.StatusNotFound)
		return false
	}
	return true
}

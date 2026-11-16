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

// API-token auth on the plan routes (plan-03a), by the pattern of the
// migration lint route: the CLI sends `Authorization: Bearer <operator
// API token>` instead of signing in. The token is the only credential such
// a request carries: a session cookie beside it is ignored and a bad token
// is never retried as a session. Only an operator token qualifies (an
// agent token is refused whatever scopes it lists): the read scope reads,
// the approve scope decides, and the token's owner, with their current
// role and database grants, is the person the decision is attributed to.
// A token limited to some databases sees the others as unknown.

const planRoutePrefix = "/api/v1/plans"

// isPlanTokenRequest reports a Bearer request to a plan route, which
// authenticates its token itself.
func isPlanTokenRequest(r *http.Request) bool {
	_, bearer := bearerCredential(r)
	p := r.URL.Path
	return bearer && (p == planRoutePrefix || strings.HasPrefix(p, planRoutePrefix+"/"))
}

// planTokenOrSession serves a plan route: a Bearer request by an operator
// token holding scope, any other request by a signed-in role behind gate.
type planTokenOrSession struct {
	gate    routeGate
	tokens  lintTokenValidator
	scope   string
	roles   []string
	next    http.Handler
	session http.Handler
}

func newPlanTokenOrSession(gate routeGate, tokens lintTokenValidator, scope string,
	next http.Handler) *planTokenOrSession {
	roles := operatorRoles
	if gate == gateViewer {
		roles = viewerRoles
	}
	return &planTokenOrSession{gate: gate, tokens: tokens, scope: scope, roles: roles,
		next: next, session: RequireRole(roles...)(next)}
}

func (h *planTokenOrSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secret, bearer := bearerCredential(r)
	if !bearer {
		h.session.ServeHTTP(w, r)
		return
	}
	user, p, ok := h.authenticate(w, r, secret)
	if !ok {
		return
	}
	// Never both: the token's owner replaces any session user.
	ctx := context.WithValue(r.Context(), userContextKey, user)
	h.next.ServeHTTP(w, r.WithContext(mcp.WithPrincipal(ctx, p)))
}

// authenticate validates secret as an operator token with h.scope whose
// owner holds one of h.roles now, answering every refusal itself. The
// secret is never logged.
func (h *planTokenOrSession) authenticate(w http.ResponseWriter, r *http.Request,
	secret string) (*auth.User, mcp.Principal, bool) {
	if h.tokens == nil {
		jsonError(w, "token validation unavailable: no control database; sign in instead",
			http.StatusServiceUnavailable)
		return nil, mcp.Principal{}, false
	}
	if secret == "" {
		refuseLintToken(w)
		return nil, mcp.Principal{}, false
	}
	grant, err := h.tokens.Validate(r.Context(), secret)
	switch {
	case errors.Is(err, mcptoken.ErrUnauthorized):
		refuseLintToken(w)
		return nil, mcp.Principal{}, false
	case err != nil:
		slog.Error("plan route token validation failed", "path", r.URL.Path, "err", err)
		w.Header().Set("Retry-After", "5")
		jsonError(w, "token validation unavailable", http.StatusServiceUnavailable)
		return nil, mcp.Principal{}, false
	case grant.Kind != mcptoken.KindOperator:
		sreErrorCode(w, "an agent token cannot read or decide plans; use an operator token",
			"agent_token", http.StatusForbidden)
		return nil, mcp.Principal{}, false
	case !slices.Contains(grant.Scopes, h.scope):
		sreErrorCode(w, "the token lacks the "+h.scope+" scope this route needs",
			"scope_missing", http.StatusForbidden)
		return nil, mcp.Principal{}, false
	case grant.OwnerUserID <= 0 || !slices.Contains(h.roles, grant.OwnerRole):
		jsonError(w, "insufficient permissions", http.StatusForbidden)
		return nil, mcp.Principal{}, false
	}
	user := &auth.User{ID: grant.OwnerUserID, Role: grant.OwnerRole}
	return user, grantPrincipal(grant), true
}

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// MCP API tokens authenticate the MCP endpoint, and only it; the endpoint
// accepts nothing else. An invalid token is refused and never falls back
// to a session.

const mcpEndpointPath = "/api/v1/mcp"

// errTokenAuthUnavailable means tokens cannot be checked at all (no control
// database); the request is refused as unauthenticated.
var errTokenAuthUnavailable = errors.New("mcp token authentication unavailable")

// mcpTokenStore is the token store on the control pool; nil without one,
// which refuses every token.
func mcpTokenStore(pool *pgxpool.Pool) *mcptoken.Store {
	if pool == nil {
		return nil
	}
	return mcptoken.NewStore(pool)
}

// isMCPTokenRequest reports a request the session middleware must leave to
// the MCP token check: every request to the MCP endpoint, which is
// token-only (a session cookie there is ignored, never used).
func isMCPTokenRequest(r *http.Request) bool {
	return r.URL.Path == mcpEndpointPath || strings.HasPrefix(r.URL.Path, mcpDatabasePrefix)
}

// bearerCredential returns the credential of an `Authorization: Bearer`
// header, and whether the header uses the Bearer scheme at all.
func bearerCredential(r *http.Request) (string, bool) {
	scheme, credential, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(credential), true
}

// tokenPrincipal validates secret and builds the principal it grants. An
// agent token also yields the agent identity it acts for (agent
// governance): the principal is loaded once, at authentication.
func tokenPrincipal(
	ctx context.Context, tokens *mcptoken.Store, secret string,
) (mcp.Principal, *agentguard.Identity, error) {
	if tokens == nil {
		return mcp.Principal{}, nil, errTokenAuthUnavailable
	}
	grant, err := tokens.Validate(ctx, secret)
	if err != nil {
		return mcp.Principal{}, nil, err
	}
	id, isAgent, err := agentguard.IdentityForGrant(ctx,
		agentguard.NewStore(tokens.Pool()), grant)
	if err != nil {
		return mcp.Principal{}, nil, err
	}
	scopes := make([]mcp.Scope, 0, len(grant.Scopes))
	for _, s := range grant.Scopes {
		scopes = append(scopes, mcp.Scope(s))
	}
	kind := mcp.KindHuman
	if grant.Kind == mcptoken.KindAgent {
		kind = mcp.KindAgent
	}
	p := mcp.Principal{
		Actor: "token:" + grant.TokenID, Role: grant.OwnerRole, Kind: kind, Name: grant.Name,
		Scopes: scopes, Databases: grant.Databases, TokenID: grant.TokenID,
		PrincipalID: grant.PrincipalID,
	}
	if !isAgent {
		return p, nil, nil
	}
	return p, &id, nil
}

// serveMCPToken authenticates a Bearer request to the MCP endpoint.
func serveMCPToken(
	w http.ResponseWriter, r *http.Request, next http.Handler,
	tokens *mcptoken.Store, secret string,
) {
	p, identity, err := tokenPrincipal(r.Context(), tokens, secret)
	switch {
	case errors.Is(err, mcptoken.ErrUnauthorized), errors.Is(err, errTokenAuthUnavailable):
		jsonError(w, "invalid, expired or revoked MCP token", http.StatusUnauthorized)
		return
	case err != nil:
		// A storage failure is not the client's fault: 503, so a client
		// does not discard a valid token. The secret is never logged.
		slog.Error("mcp token validation failed", "path", r.URL.Path, "err", err)
		jsonError(w, "token validation unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx := mcp.WithPrincipal(r.Context(), p)
	if identity != nil {
		ctx = agentguard.WithIdentity(ctx, *identity)
	}
	next.ServeHTTP(w, r.WithContext(ctx))
}

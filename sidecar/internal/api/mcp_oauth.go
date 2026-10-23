package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcpauth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// mcpDatabasePrefix is the per-database MCP endpoint: each monitored
// database is its own OAuth protected resource (E2, CG-06).
const mcpDatabasePrefix = mcpEndpointPath + "/databases/"

// principalGetter loads an agent principal (agentguard.Store in
// production).
type principalGetter interface {
	Get(ctx context.Context, id string) (agentguard.Principal, error)
}

// registerMCPRoutes mounts the MCP endpoint and its per-database twin.
// oauth nil accepts pg_sage's own MCP tokens only; principals loads the
// principal an OAuth identity is bound to.
func registerMCPRoutes(mux *http.ServeMux, handler http.Handler,
	tokens *mcptoken.Store, oauth *mcpauth.Validator, principals principalGetter) {
	h := bindMCPPrincipalOAuth(handler, tokens, oauth, principals)
	mux.Handle("POST "+mcpEndpointPath, h)
	mux.Handle("POST "+mcpDatabasePrefix+"{database}", h)
}

// registerMCPMetadata publishes RFC 9728 metadata on the root mux (no
// session: clients read it before they hold a token).
func registerMCPMetadata(root *http.ServeMux, oauth *mcpauth.Validator) {
	if oauth == nil {
		return
	}
	root.Handle("GET "+mcpauth.MetadataPrefix+"/", oauth.MetadataHandler())
}

// bindMCPPrincipalOAuth authenticates an MCP request with an OAuth access
// token (a JWT, when oauth is configured) or with a pg_sage MCP token.
func bindMCPPrincipalOAuth(next http.Handler, tokens *mcptoken.Store,
	oauth *mcpauth.Validator, principals principalGetter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := bearerCredential(r)
		if !ok {
			refuseMissingToken(w, oauth)
			return
		}
		if oauth != nil && looksLikeJWT(secret) {
			serveMCPOAuth(w, r, next, oauthDeps{oauth, principals}, secret)
			return
		}
		serveMCPToken(w, r, narrowToDatabase(next, r.PathValue("database")), tokens,
			secret)
	})
}

func refuseMissingToken(w http.ResponseWriter, oauth *mcpauth.Validator) {
	w.Header().Set("Content-Type", "application/json")
	if oauth != nil {
		w.Header().Set("WWW-Authenticate", oauth.Challenge(oauth.Resource(), ""))
	} else {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pg_sage MCP"`)
	}
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": mcpTokenRequired, "code": "mcp_token_required"})
}

// looksLikeJWT tells an access token (three base64url segments) from a
// pg_sage MCP token.
func looksLikeJWT(secret string) bool {
	return !strings.HasPrefix(secret, mcptoken.SecretPrefix) &&
		strings.Count(secret, ".") == 2
}

type oauthDeps struct {
	validator  *mcpauth.Validator
	principals principalGetter
}

// serveMCPOAuth validates an access token for the resource the request
// addresses, loads the principal its identity is bound to from core's
// store, and binds the agent identity (agentguard) for the gate.
func serveMCPOAuth(w http.ResponseWriter, r *http.Request, next http.Handler,
	d oauthDeps, raw string) {
	resource, _, ok := d.validator.ResourceForPath(r.URL.EscapedPath())
	if !ok {
		jsonError(w, "unknown MCP resource", http.StatusNotFound)
		return
	}
	tok, err := d.validator.Validate(r.Context(), raw, resource)
	if err != nil {
		refuseOAuth(w, d.validator, resource, err)
		return
	}
	pr, err := loadOAuthPrincipal(r.Context(), d.principals, tok.PrincipalID)
	if err != nil {
		refuseOAuth(w, d.validator, resource, err)
		return
	}
	p := mcp.Principal{Actor: "principal:" + pr.ID, Kind: mcp.KindAgent,
		Name: tok.Subject, Databases: tok.Databases, Scopes: mcpScopes(tok.Scopes),
		PrincipalID: pr.ID}
	id := agentguard.Identity{Principal: pr, Databases: tok.Databases,
		TaskID: tok.TaskID, OnBehalfOf: tok.OnBehalfOf}
	ctx := agentguard.WithIdentity(mcp.WithPrincipal(r.Context(), p), id)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// errPrincipalRetired refuses an identity bound to a retired principal.
var errPrincipalRetired = errors.New("the bound agent principal is retired")

// loadOAuthPrincipal loads the bound principal, mapping core's outcomes to
// the OAuth refusals: gone is unbound, retired is refused, a storage
// failure is unavailable.
func loadOAuthPrincipal(ctx context.Context, ps principalGetter, id string) (
	agentguard.Principal, error) {
	if ps == nil {
		return agentguard.Principal{}, fmt.Errorf("%w: no principal store", mcpauth.ErrResolver)
	}
	p, err := ps.Get(ctx, id)
	switch {
	case errors.Is(err, agentguard.ErrNotFound):
		return p, mcpauth.ErrNoBinding
	case err != nil:
		return p, fmt.Errorf("%w: %w", mcpauth.ErrResolver, err)
	case p.Retired():
		return p, errPrincipalRetired
	}
	return p, nil
}

func mcpScopes(scopes []string) []mcp.Scope {
	out := make([]mcp.Scope, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, mcp.Scope(s))
	}
	return out
}

// refuseOAuth maps a validation failure to its status. The token is never
// echoed or logged.
func refuseOAuth(w http.ResponseWriter, oauth *mcpauth.Validator, resource string,
	err error) {
	switch {
	case errors.Is(err, mcpauth.ErrIssuerUnavailable), errors.Is(err, mcpauth.ErrResolver):
		slog.Error("mcp oauth validation unavailable", "resource", resource, "err", err)
		jsonError(w, "token validation unavailable", http.StatusServiceUnavailable)
	case errors.Is(err, errPrincipalRetired):
		writeJSONCode(w, http.StatusForbidden, "agent_retired", err.Error())
	case errors.Is(err, mcpauth.ErrNoBinding):
		writeJSONCode(w, http.StatusForbidden, "identity_unbound",
			"no agent principal is bound to this token's identity; an admin binds it")
	case errors.Is(err, mcpauth.ErrInsufficientScope):
		w.Header().Set("WWW-Authenticate", oauth.Challenge(resource, "insufficient_scope"))
		writeJSONCode(w, http.StatusForbidden, "insufficient_scope",
			"the token lacks the "+mcpauth.ScopeRead+" scope")
	default:
		slog.Info("mcp oauth token refused", "resource", resource, "reason", err)
		w.Header().Set("WWW-Authenticate", oauth.Challenge(resource, "invalid_token"))
		writeJSONCode(w, http.StatusUnauthorized, "invalid_token",
			"invalid, expired or foreign access token")
	}
}

func writeJSONCode(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

// narrowToDatabase limits the bound principal to db at a per-database
// endpoint: db when the principal may use it, otherwise nothing.
func narrowToDatabase(next http.Handler, db string) http.Handler {
	if db == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if p, ok := mcp.PrincipalFromContext(ctx); ok {
			p.Databases = narrowed(p.MayUseDatabase(db), db)
			ctx = mcp.WithPrincipal(ctx, p)
		}
		if id, ok := agentguard.IdentityFromContext(ctx); ok {
			id.Databases = narrowed(id.MayUseDatabase(db), db)
			ctx = agentguard.WithIdentity(ctx, id)
		}
		r = r.WithContext(ctx)
		next.ServeHTTP(w, r)
	})
}

func narrowed(allowed bool, db string) []string {
	if allowed {
		return []string{db}
	}
	return []string{}
}

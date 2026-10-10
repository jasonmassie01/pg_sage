package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcpauth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// mcpDatabasePrefix is the per-database MCP endpoint: each monitored
// database is its own OAuth protected resource (E2, CG-06).
const mcpDatabasePrefix = mcpEndpointPath + "/databases/"

// registerMCPRoutes mounts the MCP endpoint and its per-database twin.
// oauth nil accepts pg_sage's own MCP tokens only.
func registerMCPRoutes(mux *http.ServeMux, handler http.Handler,
	tokens *mcptoken.Store, oauth *mcpauth.Validator) {
	h := bindMCPPrincipalOAuth(handler, tokens, oauth)
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
	oauth *mcpauth.Validator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := bearerCredential(r)
		if !ok {
			refuseMissingToken(w, oauth)
			return
		}
		if oauth != nil && looksLikeJWT(secret) {
			serveMCPOAuth(w, r, next, oauth, secret)
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

// serveMCPOAuth validates an access token for the resource the request
// addresses and binds the agent principal it maps to.
func serveMCPOAuth(w http.ResponseWriter, r *http.Request, next http.Handler,
	oauth *mcpauth.Validator, raw string) {
	resource, _, ok := oauth.ResourceForPath(r.URL.EscapedPath())
	if !ok {
		jsonError(w, "unknown MCP resource", http.StatusNotFound)
		return
	}
	id, err := oauth.Validate(r.Context(), raw, resource)
	if err != nil {
		refuseOAuth(w, oauth, resource, err)
		return
	}
	p := mcp.Principal{Actor: "principal:" + id.PrincipalID, Kind: mcp.KindAgent,
		Name: id.Subject, Databases: id.Databases, Scopes: mcpScopes(id.Scopes)}
	ctx := mcpauth.WithIdentity(mcp.WithPrincipal(r.Context(), p), id)
	next.ServeHTTP(w, r.WithContext(ctx))
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
		p, ok := mcp.PrincipalFromContext(r.Context())
		if ok {
			allowed := []string{}
			if p.MayUseDatabase(db) {
				allowed = []string{db}
			}
			p.Databases = allowed
			r = r.WithContext(mcp.WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

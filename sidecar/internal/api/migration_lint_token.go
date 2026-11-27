package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// API-token auth on POST /api/v1/migrations/lint (migration-11b): a CI job
// sends `Authorization: Bearer <MCP API token>` holding the read scope
// instead of signing in. The token is the only credential such a request
// carries: a session cookie beside it is ignored, a bad token is never
// retried as a session. A token limited to some databases may name only
// those; any other is 404, as an unknown database. With api.database_grants
// on, a person's token is further limited to what its owner may read now
// (apiauth-08c). Cross-origin
// protection still runs first: CI clients send no Origin or
// Sec-Fetch-Site and pass it, and a browser cannot send the header
// cross-origin without a CORS grant pg_sage never gives. In a Databricks
// App the platform proxy sets its own Authorization header, so the route
// is registered session-only there (registerMigrationLintRoutes).

// lintTokenValidator authenticates an MCP API token (*mcptoken.Store).
type lintTokenValidator interface {
	Validate(ctx context.Context, secret string) (mcptoken.Grant, error)
}

// lintTokenStore is the control pool's token store as a validator; nil
// (not a typed nil) without a control pool.
func lintTokenStore(store *mcptoken.Store) lintTokenValidator {
	if store == nil {
		return nil
	}
	return store
}

// isLintTokenRequest reports a Bearer POST to the lint route, which
// authenticates its token itself (gateViewerWriteOrToken).
func isLintTokenRequest(r *http.Request) bool {
	_, bearer := bearerCredential(r)
	return bearer && r.Method == http.MethodPost && r.URL.Path == migrationLintPath
}

// tokenOrSession serves a gateViewerWriteOrToken route: a Bearer request
// by its read-scope token alone, any other request by a signed-in role.
type tokenOrSession struct {
	tokens  lintTokenValidator
	token   http.Handler
	session http.Handler
}

func newTokenOrSession(tokens lintTokenValidator, next http.Handler) *tokenOrSession {
	return &tokenOrSession{tokens: tokens, token: accessPrincipalMiddleware(next),
		session: RequireRole(viewerRoles...)(next)}
}

func (h *tokenOrSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secret, bearer := bearerCredential(r)
	if !bearer {
		h.session.ServeHTTP(w, r)
		return
	}
	p, ok := authenticateReadToken(w, r, h.tokens, secret)
	if !ok {
		return
	}
	// A person's token lints only the databases its owner may read now
	// (apiauth-08c); a grant store failure refuses the request.
	p, err := scopeMCPPrincipal(r.Context(), databaseAuthzFrom(r.Context()), p)
	if err != nil {
		slog.Error("migration lint grant check failed; refusing the token request",
			"token_id", p.TokenID, "owner_user_id", p.OwnerUserID, "err", err)
		writeGrantStoreUnavailable(w)
		return
	}
	// Never both: drop any session user, so only the token acts.
	ctx := context.WithValue(r.Context(), userContextKey, (*auth.User)(nil))
	h.token.ServeHTTP(w, r.WithContext(mcp.WithPrincipal(ctx, p)))
}

// authenticateReadToken validates secret and requires the read scope,
// answering every refusal itself. The secret is never logged.
func authenticateReadToken(w http.ResponseWriter, r *http.Request,
	tokens lintTokenValidator, secret string) (mcp.Principal, bool) {
	if tokens == nil {
		jsonError(w, "token validation unavailable: no control database; "+
			"sign in instead", http.StatusServiceUnavailable)
		return mcp.Principal{}, false
	}
	if secret == "" {
		refuseLintToken(w)
		return mcp.Principal{}, false
	}
	grant, err := tokens.Validate(r.Context(), secret)
	switch {
	case errors.Is(err, mcptoken.ErrUnauthorized):
		refuseLintToken(w)
		return mcp.Principal{}, false
	case err != nil:
		// Not the caller's fault: 503, so a CI job does not discard a
		// valid token.
		slog.Error("migration lint token validation failed", "path", r.URL.Path, "err", err)
		w.Header().Set("Retry-After", "5")
		jsonError(w, "token validation unavailable", http.StatusServiceUnavailable)
		return mcp.Principal{}, false
	case !slices.Contains(grant.Scopes, mcptoken.ScopeRead):
		sreErrorCode(w, "the token lacks the read scope this route needs",
			"scope_missing", http.StatusForbidden)
		return mcp.Principal{}, false
	}
	return grantPrincipal(grant), true
}

func refuseLintToken(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="pg_sage"`)
	jsonError(w, "invalid, expired or revoked API token", http.StatusUnauthorized)
}

// registerLintTokenRoute registers the lint handler for d behind
// gateViewerWriteOrToken; nil tokens answer every Bearer request 503.
func registerLintTokenRoute(mux *http.ServeMux, d migrationLintDeps,
	tokens lintTokenValidator) {
	route(mux, "POST "+migrationLintPath, gateViewerWriteOrToken,
		newTokenOrSession(tokens, migrationLintHandler(d)))
}

// reviewDatabase is the database the review reads: the request's own,
// or, for a token limited to some databases, the one a live review that
// names none resolves to, so the catalog read is the database checked. A
// token caller is refused a database outside its list as an unknown
// database, so the list does not reveal which databases exist; a static
// review that names none reads no catalog. Session callers keep the
// request's database (any signed-in role may lint any of them).
func (d migrationLintDeps) reviewDatabase(ctx context.Context,
	req migrationLintRequest) (string, error) {
	p, ok := mcp.PrincipalFromContext(ctx)
	if !ok || p.Databases == nil {
		return req.Database, nil
	}
	name := req.Database
	if name == "" {
		if req.Static {
			return "", nil
		}
		if d.resolve == nil {
			return "", errLintNoDatabase
		}
		var err error
		if name, err = d.resolve(""); err != nil {
			return "", err
		}
	}
	if !p.MayUseDatabase(name) {
		return "", errLintUnknownDatabase
	}
	return name, nil
}

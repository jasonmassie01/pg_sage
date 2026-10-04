package api

import (
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// bindMCPPrincipal binds the authenticated session user to the MCP
// request so the MCP server can enforce per-tool roles and persist the
// real actor (G6-B02 / SURF-01). Requests without a user fail closed.
// A Bearer credential is an MCP API token checked against tokens (nil:
// refused); it never falls back to the session.
func bindMCPPrincipal(next http.Handler, tokens *mcptoken.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret, ok := bearerCredential(r); ok {
			serveMCPToken(w, r, next, tokens, secret)
			return
		}
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required",
				http.StatusUnauthorized)
			return
		}
		ctx := mcp.WithPrincipal(r.Context(), mcp.Principal{
			Actor: "user:" + strconv.Itoa(user.ID),
			Role:  user.Role,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// mcpTokenRequired tells a caller without an MCP token how to get one.
const mcpTokenRequired = "MCP over HTTP needs an MCP token: an admin creates one under " +
	"MCP tokens in the dashboard; send it as Authorization: Bearer <token>"

// bindMCPPrincipal authenticates the MCP endpoint with an MCP API token
// and binds the principal the token grants, so the MCP server enforces its
// scopes and databases and records the token as the actor (G6-B02 /
// SURF-01). MCP over HTTP is token-only: a dashboard session cookie never
// authenticates it (a browser sends the cookie cross-site; MCP clients
// send bearer tokens), and an invalid token never falls back to anything.
func bindMCPPrincipal(next http.Handler, tokens *mcptoken.Store) http.Handler {
	return bindMCPPrincipalOAuth(next, tokens, nil, nil)
}

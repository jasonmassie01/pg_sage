package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// An agent token carries its principal into the request (agent governance
// §6.4): the MCP principal names it and the agentguard identity is bound
// on the context, loaded once at authentication. A retired principal's
// token no longer authenticates.

type identityCapture struct {
	mcp      mcp.Principal
	identity agentguard.Identity
	bound    bool
	calls    int
}

func (c *identityCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls++
		c.mcp, _ = mcp.PrincipalFromContext(r.Context())
		c.identity, c.bound = agentguard.IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func postMCP(h http.Handler, secret string) int {
	req := httptest.NewRequest(http.MethodPost, mcpEndpointPath, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func TestMCPBearerAgentTokenBindsItsPrincipal(t *testing.T) {
	pool := surfacePool(t)
	tok := mintAgentToken(t, pool, []string{"orders"})
	capture := &identityCapture{}
	h := bindMCPPrincipal(capture.handler(), mcptoken.NewStore(pool))
	require.Equal(t, http.StatusOK, postMCP(h, tok.Secret))
	require.True(t, capture.bound, "the agent identity is on the context")
	require.Equal(t, tok.PrincipalID, capture.identity.Principal.ID)
	require.Equal(t, tok.PrincipalID, capture.mcp.PrincipalID)
	require.Equal(t, mcp.KindAgent, capture.mcp.Kind)
	require.Equal(t, tok.ID, capture.identity.TokenID)
	require.Equal(t, []string{"orders"}, capture.identity.Databases)
	require.True(t, capture.identity.Principal.Sponsored())

	_, err := agentguard.NewStore(pool).SetStatus(context.Background(), tok.PrincipalID,
		agentguard.StatusRetired, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, postMCP(h, tok.Secret),
		"a retired principal's token is refused")
	require.Equal(t, 1, capture.calls)
}

func TestMCPBearerOperatorTokenBindsNoAgent(t *testing.T) {
	pool := surfacePool(t)
	tok := mcpRoleToken(t, pool, "operator")
	capture := &identityCapture{}
	h := bindMCPPrincipal(capture.handler(), mcptoken.NewStore(pool))
	require.Equal(t, http.StatusOK, postMCP(h, tok.Secret))
	require.False(t, capture.bound, "a person's token is not an agent")
	require.Equal(t, "", capture.mcp.PrincipalID)
	require.Equal(t, mcp.KindHuman, capture.mcp.Kind)
}

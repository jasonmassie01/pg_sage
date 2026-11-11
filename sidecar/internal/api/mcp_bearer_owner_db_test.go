package api

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// playbooks-06: an operator token's principal carries its owner's user id,
// the approver a statistics rollback over MCP is recorded under. The
// owner's current role decides whether that rollback is allowed.
func TestTokenPrincipalCarriesTheOwnerUserID(t *testing.T) {
	pool := surfacePool(t)
	tok := metricsToken(t, pool, mcptoken.ScopeRead, mcptoken.ScopePropose)
	p, identity, err := tokenPrincipal(context.Background(), mcptoken.NewStore(pool), tok.Secret)
	require.NoError(t, err)
	require.True(t, identity == nil, "an operator token is not an agent identity")
	require.True(t, p.OwnerUserID > 0, "owner user id = %d", p.OwnerUserID)
	require.Equal(t, "operator", p.Role)
	ctx := mcp.WithPrincipal(context.Background(), p)
	_, err = mcp.RollbackApprover(ctx)
	require.ErrorIs(t, err, mcp.ErrAdminRequired)
}

package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// AGENTDB-SPEC §6.2.1, §6.2.6: an agent token binds the policy gate's
// principal ref too, so every gate request under the call is the agent's.

type refCapture struct {
	ref   policy.PrincipalRef
	bound bool
}

func (c *refCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.ref, c.bound = policy.PrincipalRefFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestMCPBearerAgentTokenBindsPolicyRef(t *testing.T) {
	pool := surfacePool(t)
	tok := mintAgentToken(t, pool, []string{"orders"})
	capture := &refCapture{}
	h := bindMCPPrincipal(capture.handler(), mcptoken.NewStore(pool))
	require.Equal(t, http.StatusOK, postMCP(h, tok.Secret))
	require.True(t, capture.bound)
	require.Equal(t, tok.PrincipalID, capture.ref.ID)
	require.True(t, capture.ref.SponsorID > 0, "the sponsor is carried")
}

func TestMCPBearerHumanTokenBindsNoPolicyRef(t *testing.T) {
	pool := surfacePool(t)
	tok := mcpRoleToken(t, pool, "operator")
	capture := &refCapture{}
	h := bindMCPPrincipal(capture.handler(), mcptoken.NewStore(pool))
	require.Equal(t, http.StatusOK, postMCP(h, tok.Secret))
	require.False(t, capture.bound, "a person's call is not agent-originated")
}

func TestPrincipalRefOfIdentity(t *testing.T) {
	sponsor := 12
	ref := principalRef(agentguard.Identity{
		Principal: agentguard.Principal{ID: "agp_aaaaaaaaaaaaaaaaaaaa",
			SponsorUserID: &sponsor},
		TaskID: "task-1", OnBehalfOf: "alice"})
	require.Equal(t, policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa", SponsorID: 12,
		TaskID: "task-1", OnBehalfOf: "alice"}, ref)
	unsponsored := principalRef(agentguard.Identity{
		Principal: agentguard.Principal{ID: "agp_bbbbbbbbbbbbbbbbbbbb"}})
	require.Equal(t, 0, unsponsored.SponsorID)
}

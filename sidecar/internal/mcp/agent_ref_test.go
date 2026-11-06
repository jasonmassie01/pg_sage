package mcp

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-02: the policy gate's view of an agent comes from the
// authenticated identity whatever transport bound it, so an OAuth caller
// keeps its sponsor, task and act claim even when no ref was bound.

// No concurrent access tests: bindAgentRef is a pure function of an
// immutable context and shares no state.

const refAgent = "agp_aaaaaaaaaaaaaaaaaaaa"

func sponsoredIdentity(principalID string, sponsor int) agentguard.Identity {
	return agentguard.Identity{
		Principal: agentguard.Principal{ID: principalID, SponsorUserID: &sponsor},
		TaskID:    "task-7", OnBehalfOf: "alice"}
}

func boundRef(t *testing.T, ctx context.Context) policy.PrincipalRef {
	t.Helper()
	ref, ok := policy.PrincipalRefFromContext(bindAgentRef(ctx, "propose_policy_change"))
	require.True(t, ok, "an agent's call carries a ref")
	return ref
}

func TestBindAgentRefBuildsTheRefFromTheIdentity(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal(refAgent))
	ctx = agentguard.WithIdentity(ctx, sponsoredIdentity(refAgent, 41))
	require.Equal(t, policy.PrincipalRef{ID: refAgent, SponsorID: 41, TaskID: "task-7",
		OnBehalfOf: "alice", Tool: "propose_policy_change"}, boundRef(t, ctx))
}

func TestBindAgentRefReplacesARefForAnotherPrincipal(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal(refAgent))
	ctx = agentguard.WithIdentity(ctx, sponsoredIdentity(refAgent, 41))
	ctx = policy.WithPrincipalRef(ctx, policy.PrincipalRef{
		ID: "agp_zzzzzzzzzzzzzzzzzzzz", SponsorID: 99, TaskID: "other-task"})
	require.Equal(t, policy.PrincipalRef{ID: refAgent, SponsorID: 41, TaskID: "task-7",
		OnBehalfOf: "alice", Tool: "propose_policy_change"}, boundRef(t, ctx))
}

// An identity for another principal is not the caller's: neither its
// sponsor nor its task is borrowed.
func TestBindAgentRefIgnoresAnIdentityForAnotherPrincipal(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal(refAgent))
	ctx = agentguard.WithIdentity(ctx, sponsoredIdentity("agp_zzzzzzzzzzzzzzzzzzzz", 99))
	require.Equal(t, policy.PrincipalRef{ID: refAgent, Tool: "propose_policy_change"},
		boundRef(t, ctx))
}

// The transport's ref for the same principal stays the authority (the
// token path binds it from the same identity).
func TestBindAgentRefKeepsAMatchingRef(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal(refAgent))
	ctx = policy.WithPrincipalRef(ctx, policy.PrincipalRef{ID: refAgent, SponsorID: 7,
		TaskID: "task-1", OnBehalfOf: "bob"})
	require.Equal(t, policy.PrincipalRef{ID: refAgent, SponsorID: 7, TaskID: "task-1",
		OnBehalfOf: "bob", Tool: "propose_policy_change"}, boundRef(t, ctx))
}

func TestBindAgentRefUnsponsoredIdentityHasNoSponsor(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal(refAgent))
	ctx = agentguard.WithIdentity(ctx, agentguard.Identity{
		Principal: agentguard.Principal{ID: refAgent}})
	require.Equal(t, policy.PrincipalRef{ID: refAgent, Tool: "propose_policy_change"},
		boundRef(t, ctx))
}

// The stdio client without mcp.stdio_principal is an agent with no
// principal and no identity: an unsponsored ref, as before.
func TestBindAgentRefStdioWithoutPrincipal(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal(""))
	require.Equal(t, policy.PrincipalRef{Tool: "propose_policy_change"}, boundRef(t, ctx))
}

func TestBindAgentRefPersonCarriesNoRef(t *testing.T) {
	person := Principal{Actor: "user:alice", Kind: KindHuman,
		Scopes: []Scope{ScopeRead, ScopePropose}}
	for name, ctx := range map[string]context.Context{
		"person": WithPrincipal(context.Background(), person),
		"person with a stray identity": agentguard.WithIdentity(
			WithPrincipal(context.Background(), person), sponsoredIdentity(refAgent, 41)),
		"no principal": context.Background(),
	} {
		_, ok := policy.PrincipalRefFromContext(bindAgentRef(ctx, "list_facts"))
		require.False(t, ok, "%s: a person's call is not agent-originated", name)
	}
}

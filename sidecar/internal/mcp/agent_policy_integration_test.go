package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// G1-14 end to end through the MCP surface: an agent's
// propose_policy_change is stored with its principal and its widening
// flag, and the policy store then refuses to activate it on one person's
// approval. The agent itself has no tool that ratifies.
func TestAgentPolicyProposalNeedsTwoPeople(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()
	databaseID := int64(78)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.policy WHERE database_id = $1", databaseID)
	current, err := access.policies.Bootstrap(ctx, policy.Scope{DatabaseID: &databaseID},
		"unattended", "mcp-agent-policy-test")
	require.NoError(t, err)

	agent := WithPrincipal(ctx, agentPrincipal("agp_aaaaaaaaaaaaaaaaaaaa"))
	agent = bindAgentRef(agent, "propose_policy_change")
	proposal, err := access.ProposePolicyChangeDryRun(agent, PolicyProposalRequest{
		DatabaseID: &databaseID,
		Delta:      json.RawMessage(`{"lock_duration_ceiling_ms":600000}`),
	})
	require.NoError(t, err)
	var principal string
	var widening bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT proposed_principal_id, widening
		FROM sage.policy WHERE id = $1`, proposal.ProposalID).Scan(&principal, &widening))
	require.Equal(t, "agp_aaaaaaaaaaaaaaaaaaaa", principal)
	require.True(t, widening, "raising the lock ceiling widens")

	_, err = access.policies.Ratify(ctx, policy.RatifyRequest{ProposalID: proposal.ProposalID,
		ExpectedVersion: current.Version, Actor: "alice@test.com", ApproverUserID: 501})
	require.True(t, errors.Is(err, policy.ErrSecondApprovalRequired), "one person: %v", err)
	_, err = access.policies.Ratify(ctx, policy.RatifyRequest{ProposalID: proposal.ProposalID,
		ExpectedVersion: current.Version, Actor: "bob@test.com", ApproverUserID: 502})
	require.NoError(t, err)
}

func TestNoMCPToolChangesPolicyProfilesOrPrompts(t *testing.T) {
	// G1-14: an agent reaches policy only through proposals. Every MCP tool
	// an agent may call is read or propose scope; the ones that touch
	// policy only propose (a person ratifies over REST, which agent tokens
	// cannot reach), and none edits profiles, prompts or configuration.
	writers := map[string]bool{"propose_policy_change": true,
		"set_maintenance_policy": true, "sre_draft_runbook": true, "sre_compile_runbook": true}
	for _, tool := range NewServer(nil).Tools() {
		scope, _ := RequiredScope(tool.Name, json.RawMessage(`{}`))
		if scope == ScopeApprove {
			continue // refused to agents (-32005)
		}
		for _, word := range []string{"ratify", "profile", "prompt", "config", "approve"} {
			if containsWord(tool.Name, word) {
				t.Fatalf("agent-reachable tool %s names %q: agents never change it",
					tool.Name, word)
			}
		}
		if writers[tool.Name] && scope != ScopePropose {
			t.Fatalf("%s must be propose scope", tool.Name)
		}
	}
}

func containsWord(name, word string) bool {
	for i := 0; i+len(word) <= len(name); i++ {
		if name[i:i+len(word)] == word {
			return true
		}
	}
	return false
}

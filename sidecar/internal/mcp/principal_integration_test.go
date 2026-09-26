package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// G6-B02: the authenticated principal is persisted as the proposal
// actor instead of a generic "mcp-agent-proposal" string.
func TestPostgresAccessPersistsPrincipalAsProposalActor(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.policy WHERE database_id = 77")
	databaseID := int64(77)
	_, err := access.policies.Bootstrap(
		ctx, policy.Scope{DatabaseID: &databaseID}, "unattended",
		"mcp-actor-test",
	)
	require.NoError(t, err)

	operator := WithPrincipal(ctx, Principal{Actor: "user:7", Role: "operator"})
	proposal, err := access.ProposePolicyChangeDryRun(operator,
		PolicyProposalRequest{
			DatabaseID: &databaseID,
			Delta:      json.RawMessage(`{"lock_duration_ceiling_ms":1200}`),
		})
	require.NoError(t, err)
	var proposedBy string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT proposed_by FROM sage.policy WHERE id = $1",
		proposal.ProposalID).Scan(&proposedBy))
	require.Equal(t, "mcp:user:7", proposedBy)
}

func TestTableContractDeclaredByPrincipal(t *testing.T) {
	store := &recordingIntentStore{}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner())
	ctx := WithPrincipal(context.Background(),
		Principal{Actor: "user:5", Role: "admin"})
	_, err := executor.Execute(ctx, policy.ActionRequest{
		Contract:  &policy.ActionContract{ActionType: "declare_table_contract"},
		Arguments: json.RawMessage(`{"table":"public.events","append_only":true}`),
	}, policy.Decision{Verdict: policy.VerdictExecute, EvidenceID: "ev-1"})
	require.NoError(t, err)
	require.Equal(t, "mcp:user:5", store.contract.DeclaredBy)
}

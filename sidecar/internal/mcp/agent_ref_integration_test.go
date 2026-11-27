package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-02 (G1-14) on real Postgres: an OAuth-authenticated agent's
// context carries the principal and the identity (no transport ref). Its
// widening proposal must still store the sponsor, so the sponsor cannot
// be one of the two approvers.

// oauthAgentContext is what serveMCPOAuth binds before the MCP server
// runs a tool: the agent principal and its identity.
func oauthAgentContext(identity agentguard.Identity) context.Context {
	ctx := WithPrincipal(context.Background(), agentPrincipal(identity.Principal.ID))
	return bindAgentRef(agentguard.WithIdentity(ctx, identity), "propose_policy_change")
}

func proposeWidening(t *testing.T, access *PostgresAccess, pool *pgxpool.Pool,
	databaseID int64, agent context.Context) (int64, policy.Policy) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, "DELETE FROM sage.policy WHERE database_id = $1", databaseID)
	require.NoError(t, err)
	current, err := access.policies.Bootstrap(ctx, policy.Scope{DatabaseID: &databaseID},
		"unattended", "mcp-agent-ref-test")
	require.NoError(t, err)
	proposal, err := access.ProposePolicyChangeDryRun(agent, PolicyProposalRequest{
		DatabaseID: &databaseID,
		Delta:      json.RawMessage(`{"lock_duration_ceiling_ms":600000}`),
	})
	require.NoError(t, err)
	return proposal.ProposalID, current
}

func storedSponsor(t *testing.T, pool *pgxpool.Pool, proposalID int64) *int {
	t.Helper()
	var sponsor *int
	var widening bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT proposed_sponsor_id,
		widening FROM sage.policy WHERE id = $1`, proposalID).Scan(&sponsor, &widening))
	require.True(t, widening, "raising the lock ceiling widens")
	return sponsor
}

func ratifyProposal(access *PostgresAccess, proposalID int64, current policy.Policy,
	actor string, userID int) error {
	_, err := access.policies.Ratify(context.Background(), policy.RatifyRequest{
		ProposalID: proposalID, ExpectedVersion: current.Version, Actor: actor,
		ApproverUserID: userID})
	return err
}

func TestOAuthAgentWideningStoresAndRefusesTheSponsor(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	agent := oauthAgentContext(sponsoredIdentity(refAgent, 541))
	proposalID, current := proposeWidening(t, access, pool, 79, agent)

	sponsor := storedSponsor(t, pool, proposalID)
	require.NotNil(t, sponsor, "the OAuth agent's sponsor is stored")
	require.Equal(t, 541, *sponsor)

	err := ratifyProposal(access, proposalID, current, "sponsor@test.com", 541)
	require.True(t, errors.Is(err, policy.ErrSponsorCannotApprove), "sponsor: %v", err)
	err = ratifyProposal(access, proposalID, current, "alice@test.com", 501)
	require.True(t, errors.Is(err, policy.ErrSecondApprovalRequired), "one person: %v", err)
	require.NoError(t, ratifyProposal(access, proposalID, current, "bob@test.com", 502))
}

// Boundary: an unsponsored OAuth agent stores no sponsor and still needs
// two people.
func TestOAuthAgentWideningWithoutSponsorNeedsTwoPeople(t *testing.T) {
	access, pool := newPostgresIntentAccess(t)
	agent := oauthAgentContext(agentguard.Identity{
		Principal: agentguard.Principal{ID: refAgent}, TaskID: "task-7"})
	proposalID, current := proposeWidening(t, access, pool, 80, agent)

	require.Nil(t, storedSponsor(t, pool, proposalID), "no sponsor to store")
	err := ratifyProposal(access, proposalID, current, "alice@test.com", 501)
	require.True(t, errors.Is(err, policy.ErrSecondApprovalRequired), "one person: %v", err)
	require.NoError(t, ratifyProposal(access, proposalID, current, "bob@test.com", 502))
}

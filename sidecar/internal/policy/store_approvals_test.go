package policy

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// G1-14 (SAFE-TOOL-08, spec §6.11): an agent can propose a policy
// change but never ratify it, and a widening agent proposal needs two
// people (neither of them the agent's sponsor) unless
// agents.single_operator_mode lets one person approve with a recorded
// reason, which then enters a post-hoc review queue.

const sponsorUserID = 41

func agentProposal(t *testing.T, store *Store, current Policy, widen bool) Proposal {
	t.Helper()
	doc := UnattendedProfile()
	doc.LockDurationCeilingMS = 1000 // narrower than the bootstrap's 3000
	if widen {
		doc.LockDurationCeilingMS = 60000
	}
	raw, err := MarshalDocument(doc)
	require.NoError(t, err)
	proposal, err := store.Propose(context.Background(), ProposalRequest{
		ExpectedVersion: current.Version, Profile: "unattended", Document: raw,
		Actor: "mcp:token:t1",
		Principal: &PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa", SponsorID: sponsorUserID,
			Tool: "propose_policy_change"},
	})
	require.NoError(t, err)
	require.Equal(t, widen, proposal.Widening)
	require.Equal(t, "agp_aaaaaaaaaaaaaaaaaaaa", proposal.PrincipalID)
	return proposal
}

func ratifyAs(store *Store, p Proposal, version int64, actor string, userID int,
	single bool, reason string) (Policy, error) {
	return store.Ratify(context.Background(), RatifyRequest{ProposalID: p.ID,
		ExpectedVersion: version, Actor: actor, ApproverUserID: userID,
		SingleOperatorMode: single, Reason: reason})
}

func TestAgentWideningProposalNeedsTwoPeople(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	proposal := agentProposal(t, store, current, true)

	_, err := ratifyAs(store, proposal, current.Version, "alice@test.com", 1, false, "")
	require.True(t, errors.Is(err, ErrSecondApprovalRequired), "first approval: %v", err)
	still, err := store.Current(context.Background(), Scope{})
	require.NoError(t, err)
	require.Equal(t, current.ID, still.ID)

	// The same person again is not a second person.
	_, err = ratifyAs(store, proposal, current.Version, "alice@test.com", 1, false, "")
	require.True(t, errors.Is(err, ErrSecondApprovalRequired), "repeat approval: %v", err)

	ratified, err := ratifyAs(store, proposal, current.Version, "bob@test.com", 2, false, "")
	require.NoError(t, err)
	require.Equal(t, current.Version+1, ratified.Version)
	approvals, err := store.Approvals(context.Background(), proposal.ID)
	require.NoError(t, err)
	require.Len(t, approvals, 2)
	require.Equal(t, "alice@test.com", approvals[0].Approver)
	require.Equal(t, "bob@test.com", approvals[1].Approver)
	require.False(t, approvals[0].SingleOperator)
}

func TestAgentWideningProposalRefusesTheSponsor(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	proposal := agentProposal(t, store, current, true)
	_, err := ratifyAs(store, proposal, current.Version, "sponsor@test.com",
		sponsorUserID, false, "")
	require.True(t, errors.Is(err, ErrSponsorCannotApprove), "sponsor: %v", err)
	approvals, err := store.Approvals(context.Background(), proposal.ID)
	require.NoError(t, err)
	require.Len(t, approvals, 0)
}

func TestAgentWideningProposalRequiresApproverIdentity(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	proposal := agentProposal(t, store, current, true)
	_, err := ratifyAs(store, proposal, current.Version, "alice@test.com", 0, false, "")
	require.True(t, errors.Is(err, ErrInvalidDocument), "no user id: %v", err)
}

func TestSingleOperatorModeOneApprovalWithReason(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	proposal := agentProposal(t, store, current, true)
	_, err := ratifyAs(store, proposal, current.Version, "solo@test.com", 1, true, "  ")
	require.True(t, errors.Is(err, ErrReasonRequired), "no reason: %v", err)

	ratified, err := ratifyAs(store, proposal, current.Version, "solo@test.com", 1, true,
		"lifeos: I am the only operator")
	require.NoError(t, err)
	require.Equal(t, current.Version+1, ratified.Version)
	reviews, err := store.PendingReviews(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, reviews, 1)
	require.Equal(t, proposal.ID, reviews[0].PolicyID)
	require.Equal(t, "lifeos: I am the only operator", reviews[0].Reason)
	require.True(t, reviews[0].SingleOperator)
}

func TestSingleOperatorModeStillRefusesTheSponsorWithoutReason(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	proposal := agentProposal(t, store, current, true)
	// Single-operator mode lets the sponsor approve (the only person), but
	// only with a recorded reason.
	_, err := ratifyAs(store, proposal, current.Version, "sponsor@test.com",
		sponsorUserID, true, "")
	require.True(t, errors.Is(err, ErrReasonRequired), "sponsor solo: %v", err)
	_, err = ratifyAs(store, proposal, current.Version, "sponsor@test.com",
		sponsorUserID, true, "only operator")
	require.NoError(t, err)
}

func TestAgentNarrowingProposalNeedsOnePerson(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	proposal := agentProposal(t, store, current, false)
	ratified, err := ratifyAs(store, proposal, current.Version, "alice@test.com", 1,
		false, "")
	require.NoError(t, err)
	require.Equal(t, current.Version+1, ratified.Version)
	reviews, err := store.PendingReviews(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, reviews, 0)
}

func TestHumanWideningProposalUnchanged(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	proposal := proposePolicy(t, store, Scope{}, current.Version, "staffed")
	require.Equal(t, "", proposal.PrincipalID)
	ratified, err := store.Ratify(context.Background(), RatifyRequest{
		ProposalID: proposal.ID, ExpectedVersion: current.Version, Actor: "admin"})
	require.NoError(t, err)
	require.Equal(t, current.Version+1, ratified.Version)
}

func TestAgentProposalWithUnreadableBaseCountsAsWidening(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	_, err := store.pool.Exec(context.Background(),
		`UPDATE sage.policy SET doc = '{"broken": true}'::jsonb WHERE id = $1`, current.ID)
	require.NoError(t, err)
	proposal := agentProposal(t, store, current, true)
	require.True(t, proposal.Widening)
}

func TestConcurrentSecondApprovalsActivateOnce(t *testing.T) {
	store := newTestStore(t)
	current := bootstrapPolicy(t, store, Scope{})
	proposal := agentProposal(t, store, current, true)
	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = ratifyAs(store, proposal, current.Version,
				"admin"+string(rune('a'+i))+"@test.com", 100+i, false, "")
		}(i)
	}
	wg.Wait()
	activated, pending, other := 0, 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			activated++
		case errors.Is(err, ErrSecondApprovalRequired):
			pending++
		default:
			other++ // ErrNotFound / version conflict after activation
		}
	}
	require.Equal(t, 1, activated, "results: %v", results)
	require.Equal(t, 1, pending, "results: %v", results)
	final, err := store.Current(context.Background(), Scope{})
	require.NoError(t, err)
	require.Equal(t, current.Version+1, final.Version)
	require.Equal(t, 2, other, "results: %v", results)
}

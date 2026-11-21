package mcp

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// How each store's row becomes the state an agent sees (mcp-01b). The
// state is the item's real state; final says nothing more will happen.

// No concurrent access tests: these maps are pure functions of a row.

type stateWant struct {
	state, by string
	final     bool
}

func requireState(t *testing.T, label string, got RequestRecord, want stateWant) {
	t.Helper()
	require.Equal(t, want.state, got.State, label)
	require.Equal(t, want.by, got.DecidedByKind, label)
	require.Equal(t, want.final, got.Final, label)
	require.NotEmpty(t, got.Outcome, "%s: the outcome says what happened", label)
}

func TestQueueRowStates(t *testing.T) {
	person := 3
	cases := []struct {
		status, verification string
		decidedBy            *int
		want                 stateWant
	}{
		{"pending", "not_started", nil, stateWant{"pending", "", false}},
		{"scheduled", "not_started", &person, stateWant{"approved", "person", false}},
		{"approved", "not_started", &person, stateWant{"approved", "person", false}},
		{"approved", "not_started", nil, stateWant{"approved", "policy", false}},
		{"executed", "pending", &person, stateWant{"executed", "person", false}},
		{"executed", "not_started", nil, stateWant{"executed", "policy", false}},
		{"executed", "verified", &person, stateWant{"verified", "person", true}},
		{"executed", "failed", &person, stateWant{"failed", "person", true}},
		{"rejected", "not_started", &person, stateWant{"rejected", "person", true}},
		{"rejected", "not_started", nil, stateWant{"rejected", "system", true}},
		{"expired", "not_started", nil, stateWant{"expired", "system", true}},
		{"failed", "failed", &person, stateWant{"failed", "person", true}},
		{"rolled_back", "verified", &person, stateWant{"rolled_back", "person", true}},
		{"", "", nil, stateWant{"unknown", "", false}},
		{"something_new", "", nil, stateWant{"unknown", "", false}},
	}
	for _, c := range cases {
		requireState(t, c.status+"/"+c.verification, queueState(c.status, c.verification,
			c.decidedBy), c.want)
	}
}

func TestPlanRowStates(t *testing.T) {
	cases := map[string]stateWant{
		"draft": {"pending", "", false}, "proposed": {"pending", "", false},
		"approved": {"approved", "person", false}, "running": {"running", "person", false},
		"halted":    {"halted", "person", false},
		"completed": {"completed", "person", true},
		"reverted":  {"reverted", "person", true}, "rejected": {"rejected", "person", true},
		"voided": {"cancelled", "system", true}, "cancelled": {"cancelled", "person", true},
		"": {"unknown", "", false}, "mystery": {"unknown", "", false},
	}
	for status, want := range cases {
		requireState(t, "plan "+status, planState(status), want)
	}
}

func TestGrantRequestRowStates(t *testing.T) {
	cases := []struct {
		status  string
		decided bool
		want    stateWant
	}{
		{"pending", false, stateWant{"pending", "", false}},
		{"approved", true, stateWant{"approved", "person", true}},
		{"denied", true, stateWant{"rejected", "person", true}},
		{"expired", false, stateWant{"expired", "system", true}},
		{"failed", true, stateWant{"failed", "person", true}},
		{"recorded", false, stateWant{"recorded", "policy", true}},
		{"", false, stateWant{"unknown", "", false}},
	}
	for _, c := range cases {
		requireState(t, "grant "+c.status, grantState(c.status, c.decided), c.want)
	}
}

func TestPolicyProposalRowStates(t *testing.T) {
	cases := []struct {
		status   string
		ratified bool
		want     stateWant
	}{
		{"proposed", false, stateWant{"pending", "", false}},
		{"active", true, stateWant{"approved", "person", true}},
		{"ratified", true, stateWant{"approved", "person", true}},
		{"superseded", true, stateWant{"approved", "person", true}},
		{"superseded", false, stateWant{"cancelled", "system", true}},
		{"rejected", false, stateWant{"rejected", "person", true}},
		{"", false, stateWant{"unknown", "", false}},
	}
	for _, c := range cases {
		requireState(t, "policy "+c.status, policyState(c.status, c.ratified), c.want)
	}
}

// A ledger decision the policy check withheld has no approval item: it is
// held, and nothing waits on a person, so a waiter does not sit on it.
func TestLedgerDecisionStates(t *testing.T) {
	cases := []struct {
		verdict            string
		resolved, executed bool
		want               stateWant
	}{
		{"parked", false, false, stateWant{"held", "policy", true}},
		{"queue_approval", false, false, stateWant{"held", "policy", true}},
		{"blocked", false, false, stateWant{"blocked", "policy", true}},
		{"observe_only", false, false, stateWant{"held", "policy", true}},
		{"recommended", false, false, stateWant{"held", "policy", true}},
		{"execute", false, true, stateWant{"executed", "policy", true}},
		{"execute", false, false, stateWant{"approved", "policy", true}},
		{"parked", true, false, stateWant{"resolved", "policy", true}},
		{"", false, false, stateWant{"unknown", "", false}},
	}
	for _, c := range cases {
		requireState(t, "decision "+c.verdict, decisionState(c.verdict, c.resolved,
			c.executed), c.want)
	}
}

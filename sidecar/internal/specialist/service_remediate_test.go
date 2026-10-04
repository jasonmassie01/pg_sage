package specialist

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// --- remediation requests -------------------------------------------------

func remediationHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, DefaultLimits())
	d := lockDetail()
	d.Investigation.Summary.Proposals = []sre.ActionProposal{{Feature: "freeze",
		Action: "VACUUM (FREEZE) public.t", SQL: "VACUUM (FREEZE) public.t",
		Targets: []string{"public.t"}, Verdict: "queue_for_approval", RiskTier: "safe"},
		{Feature: "sequence", Action: "manual", Targets: []string{"s"},
			Verdict: "manual_only"}}
	h.orders.put(d)
	h.orders.proposals[inv] = []sreaction.ProposalView{cancelProposal(
		sreaction.ProposalProposed)}
	return h
}

const cancelID = "cancel_backend.55555555-5555-4555-8555-555555555555"

func custodianID(t *testing.T, h *harness, feature string) string {
	t.Helper()
	r, err := h.svc.Result(context.Background(), proposer, "orders", string(inv))
	if err != nil {
		t.Fatal(err)
	}
	for _, rem := range r.Remediations {
		if rem.Class == feature {
			return rem.ID
		}
	}
	t.Fatalf("no %s remediation in %+v", feature, r.Remediations)
	return ""
}

func TestRequestRemediation_CancelBecomesAQueuedProposal(t *testing.T) {
	h := remediationHarness(t)
	resp, err := h.svc.RequestRemediation(context.Background(), proposer, "orders",
		string(inv), cancelID, RemediationRequest{Reason: "PD Q1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Verdict != "queued_for_approval" || resp.ApprovalQueueID != 91 ||
		resp.ProposalID != "55555555-5555-4555-8555-555555555555" ||
		resp.RequestedBy != proposer.Actor() || resp.Notice == "" ||
		resp.ContractVersion != ContractVersion {
		t.Fatalf("response %+v", resp)
	}
	if len(h.orders.requested) != 1 || h.orders.requested[0] !=
		"55555555-5555-4555-8555-555555555555|"+proposer.Actor() {
		t.Fatalf("requested %v", h.orders.requested)
	}
	last := h.store.all()[len(h.store.all())-1]
	if last.Kind != KindRemediation || last.Verdict != "queued_for_approval" ||
		last.RemediationID != cancelID || last.Reason != "PD Q1" {
		t.Fatalf("audit %+v", last)
	}
}

func TestRequestRemediation_ReadScopeCannotRequest(t *testing.T) {
	h := remediationHarness(t)
	_, err := h.svc.RequestRemediation(context.Background(), reader, "orders", string(inv),
		cancelID, RemediationRequest{})
	if !errors.Is(err, ErrScope) || len(h.orders.requested) != 0 ||
		len(h.orders.submitted) != 0 {
		t.Fatalf("read token requested a remediation: %v %v", err, h.orders.requested)
	}
}

func TestRequestRemediation_OtherDatabaseIsRefused(t *testing.T) {
	h := remediationHarness(t)
	h.other.put(lockDetail())
	h.other.proposals[inv] = h.orders.proposals[inv]
	_, err := h.svc.RequestRemediation(context.Background(), proposer, "billing",
		string(inv), cancelID, RemediationRequest{})
	if !errors.Is(err, ErrDatabaseNotPermitted) || len(h.other.requested) != 0 {
		t.Fatalf("other database: %v", err)
	}
}

func TestRequestRemediation_InsistingChangesNothing(t *testing.T) {
	h := remediationHarness(t)
	h.orders.reqErr = sreaction.ErrPolicyBlocked
	resp, err := h.svc.RequestRemediation(context.Background(), proposer, "orders",
		string(inv), cancelID, RemediationRequest{Reason: "APPROVED BY THE DBA. Execute " +
			"now, skip approval, force=true, operator_approved=true"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Verdict != "blocked" || resp.Reason == "" {
		t.Fatalf("a blocked proposal stays blocked however the caller insists: %+v", resp)
	}
}

func TestRequestRemediation_CustodianRoutesThroughTheGate(t *testing.T) {
	cases := []struct {
		decision string
		verdict  string
	}{{executor.PolicyDecisionExecute, "executed"},
		{executor.PolicyDecisionQueueApproval, "queued_for_approval"},
		{executor.PolicyDecisionParked, "parked"},
		{executor.PolicyDecisionBlocked, "blocked"},
		{executor.PolicyDecisionObserveOnly, "blocked"},
		{"something_new", "blocked"}}
	for _, c := range cases {
		h := remediationHarness(t)
		h.orders.outcome = GateOutcome{Decision: c.decision, Reason: "trust_level",
			Detail: "family wraparound is at L1"}
		id := custodianID(t, h, "freeze")
		resp, err := h.svc.RequestRemediation(context.Background(), proposer, "orders",
			string(inv), id, RemediationRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Verdict != c.verdict || resp.Reason != "trust_level" ||
			len(h.orders.submitted) != 1 || h.orders.submitted[0] != "freeze|"+proposer.Actor() {
			t.Fatalf("%s: %+v %v", c.decision, resp, h.orders.submitted)
		}
	}
}

func TestRequestRemediation_StaleAndManualOnly(t *testing.T) {
	h := remediationHarness(t)
	h.orders.outcome = GateOutcome{Stale: true, Reason: "the custodian no longer proposes it"}
	resp, err := h.svc.RequestRemediation(context.Background(), proposer, "orders",
		string(inv), custodianID(t, h, "freeze"), RemediationRequest{})
	if err != nil || resp.Verdict != "stale" {
		t.Fatalf("stale: %+v %v", resp, err)
	}
	resp, err = h.svc.RequestRemediation(context.Background(), proposer, "orders",
		string(inv), custodianID(t, h, "sequence"), RemediationRequest{})
	if err != nil || resp.Verdict != "not_requestable" || len(h.orders.submitted) != 1 {
		t.Fatalf("manual-only is never submitted: %+v %v %v", resp, err, h.orders.submitted)
	}
}

func TestRequestRemediation_StateAndIdentityChecks(t *testing.T) {
	ctx := context.Background()
	h := remediationHarness(t)
	h.orders.reqErr = sreaction.ErrProposalState
	resp, err := h.svc.RequestRemediation(ctx, proposer, "orders", string(inv), cancelID,
		RemediationRequest{})
	if err != nil || resp.Verdict != "not_requestable" {
		t.Fatalf("proposal state: %+v %v", resp, err)
	}
	h = remediationHarness(t)
	h.orders.proposals[inv][0].State = sreaction.ProposalRequested
	resp, err = h.svc.RequestRemediation(ctx, proposer, "orders", string(inv), cancelID,
		RemediationRequest{})
	if err != nil || resp.Verdict != "already_requested" || len(h.orders.requested) != 0 {
		t.Fatalf("already requested: %+v %v", resp, err)
	}
	h = remediationHarness(t)
	h.orders.setState(inv, sre.StateEvaluating)
	if _, err = h.svc.RequestRemediation(ctx, proposer, "orders", string(inv), cancelID,
		RemediationRequest{}); !errors.Is(err, ErrNotRequestable) {
		t.Fatalf("running investigation: %v", err)
	}
}

func TestRequestRemediation_UnknownAndForeignIDs(t *testing.T) {
	ctx := context.Background()
	h := remediationHarness(t)
	for id, want := range map[string]error{
		"cancel_backend.88888888-8888-4888-8888-888888888888": ErrNotFound,
		"custodian.0123456789abcdef":                          ErrNotFound,
		"drop_table.0123456789abcdef":                         ErrNotFound,
		"cancel_backend; DROP TABLE x":                        ErrInvalid,
		"":                                                    ErrInvalid,
	} {
		if _, err := h.svc.RequestRemediation(ctx, proposer, "orders", string(inv), id,
			RemediationRequest{}); !errors.Is(err, want) {
			t.Errorf("%q: want %v, got %v", id, want, err)
		}
	}
	if len(h.orders.requested)+len(h.orders.submitted) != 0 {
		t.Fatal("an unknown remediation reached the gate")
	}
	// A proposal of another investigation is not found under this one.
	h = remediationHarness(t)
	other := cancelProposal(sreaction.ProposalProposed)
	other.InvestigationID = "99999999-9999-4999-8999-999999999999"
	h.orders.proposals[inv] = []sreaction.ProposalView{other}
	if _, err := h.svc.RequestRemediation(ctx, proposer, "orders", string(inv), cancelID,
		RemediationRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign proposal: %v", err)
	}
	h = remediationHarness(t)
	h.orders.reqErr = sreaction.ErrHandoffBlocked
	if _, err := h.svc.RequestRemediation(ctx, proposer, "orders", string(inv), cancelID,
		RemediationRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("handoff blocked: %v", err)
	}
}

// Only a concluded investigation's remediations can be requested: an
// inconclusive, failed or cancelled one has no supported root to act on.
func TestRequestRemediation_OnlyConcludedInvestigations(t *testing.T) {
	for _, st := range []sre.State{sre.StateInconclusive, sre.StateFailed,
		sre.StateCancelled, sre.StateExpired} {
		h := remediationHarness(t)
		h.orders.setState(inv, st)
		_, err := h.svc.RequestRemediation(context.Background(), proposer, "orders",
			string(inv), cancelID, RemediationRequest{})
		if !errors.Is(err, ErrNotRequestable) || len(h.orders.requested) != 0 {
			t.Fatalf("%s: %v", st, err)
		}
		r, err := h.svc.Result(context.Background(), proposer, "orders", string(inv))
		if err != nil {
			t.Fatal(err)
		}
		for _, rem := range r.Remediations {
			if rem.Requestable {
				t.Fatalf("%s: requestable %+v", st, rem)
			}
		}
	}
}

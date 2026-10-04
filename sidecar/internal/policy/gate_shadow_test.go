package policy

import (
	"context"
	"testing"
)

// Roadmap 1.4 (shadow mode): when the ledger keeps a governed class from
// running unattended, the decision also carries what the gate would have
// decided had the class been trusted at L3 (Decision.Trusted), so the
// executor can record a shadow decision with that verdict. It is computed
// from the same inputs (no extra reads) and never changes the verdict.

func assertTrusted(t *testing.T, d Decision, verdict Verdict, reason Reason, granted int) {
	t.Helper()
	if d.Trusted == nil {
		t.Fatalf("decision %+v carries no trusted verdict", d)
	}
	if d.Trusted.Verdict != verdict || d.Trusted.Reason != reason ||
		d.Trusted.Granted != granted {
		t.Fatalf("trusted = %+v, want %s/%s granted L%d", *d.Trusted, verdict, reason, granted)
	}
}

func TestTrustedVerdictBelowL2IsExecuteAtL3(t *testing.T) {
	f, limiter := newScopedFixture(1)
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	assertDecision(t, d, VerdictObserveOnly, ReasonAutonomyLevel)
	assertTrusted(t, d, VerdictExecute, ReasonAutonomyL3, 1)
	if d.Trusted.Level != 1 {
		t.Fatalf("effective level %d, want 1", d.Trusted.Level)
	}
}

func TestTrustedVerdictAtL2IsExecuteAtL3(t *testing.T) {
	f, limiter := newScopedFixture(2)
	d := f.scopedGate(limiter).Authorize(context.Background(), selfIndexRequest())
	assertDecision(t, d, VerdictQueueApproval, ReasonAutonomyHandoff)
	assertTrusted(t, d, VerdictExecute, ReasonAutonomyL3, 2)
}

func TestTrustedVerdictKeepsTheCeilingsApproval(t *testing.T) {
	f, limiter := newScopedFixture(1)
	f.runtime.TrustLevel = TrustAdvisory // MODERATE needs approval at advisory
	d := f.scopedGate(limiter).Authorize(context.Background(), selfIndexRequest())
	assertDecision(t, d, VerdictObserveOnly, ReasonAutonomyLevel)
	assertTrusted(t, d, VerdictQueueApproval, ReasonApprovalRequired, 1)
}

func TestTrustedVerdictNamesTheL3Blocker(t *testing.T) {
	f, limiter := newScopedFixture(1)
	req := selfVacuumRequest()
	req.TargetObjs = []string{"public.orders", "public.items"}
	d := f.scopedGate(limiter).Authorize(context.Background(), req)
	assertTrusted(t, d, VerdictQueueApproval, ReasonAutonomyHandoff, 1)
	if d.Trusted.Detail == "" {
		t.Fatal("the L3 blocker is not named")
	}
}

func TestTrustedVerdictWhileDowngraded(t *testing.T) {
	f, limiter := newScopedFixture(3)
	limiter.limit.Downgraded, limiter.limit.Reasons = true, []string{"error_budget"}
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	assertDecision(t, d, VerdictObserveOnly, ReasonAutonomyDowngraded)
	assertTrusted(t, d, VerdictExecute, ReasonAutonomyL3, 3)
}

// A trusted class (L3) needs no shadow: the decision carries none.
func TestNoTrustedVerdictAtL3(t *testing.T) {
	f, limiter := newScopedFixture(3)
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	assertDecision(t, d, VerdictExecute, ReasonAutonomyL3)
	if d.Trusted != nil {
		t.Fatalf("an L3 execution carries a trusted verdict: %+v", d.Trusted)
	}
	f.runtime.TrustLevel = TrustAdvisory
	d = f.scopedGate(limiter).Authorize(context.Background(), selfIndexRequest())
	if d.Verdict != VerdictQueueApproval || d.Trusted != nil {
		t.Fatalf("an L3 class held by the ceiling: %+v", d)
	}
}

// Requests the ledger does not restrict carry no trusted verdict:
// operator approvals, rollbacks, ungoverned requests, hard stops and
// verdicts the gate decided before the ledger (observation).
func TestNoTrustedVerdictOutsideTheLedger(t *testing.T) {
	ctx := context.Background()
	f, limiter := newScopedFixture(1)
	rollback := selfIndexRequest()
	rollback.Rollback = true
	ungoverned := selfVacuumRequest()
	ungoverned.Contract.ActionType = "alter_table"
	for name, req := range map[string]ActionRequest{
		"rollback": rollback, "ungoverned": ungoverned,
	} {
		if d := f.scopedGate(limiter).Authorize(ctx, req); d.Trusted != nil {
			t.Errorf("%s: trusted %+v", name, d.Trusted)
		}
	}
	f.runtime.EmergencyStop = true
	if d := f.scopedGate(limiter).Authorize(ctx, selfVacuumRequest()); d.Trusted != nil ||
		d.Verdict != VerdictBlocked {
		t.Fatalf("emergency stop: %+v", d)
	}
	f.runtime.EmergencyStop = false
	f.runtime.TrustLevel = TrustObservation
	if d := f.scopedGate(limiter).Authorize(ctx, selfVacuumRequest()); d.Trusted != nil ||
		d.Verdict != VerdictObserveOnly {
		t.Fatalf("observation: %+v", d)
	}
}

// Explain (no recording) carries the same trusted verdict.
func TestExplainCarriesTheTrustedVerdict(t *testing.T) {
	f, limiter := newScopedFixture(1)
	explainer, ok := f.scopedGate(limiter).(Explainer)
	if !ok {
		t.Fatal("the gate does not explain")
	}
	d := explainer.Explain(context.Background(), selfVacuumRequest())
	if d.Trusted == nil || d.Trusted.Verdict != VerdictExecute {
		t.Fatalf("explain: %+v", d)
	}
}

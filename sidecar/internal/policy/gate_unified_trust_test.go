package policy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Roadmap 1.2 "one trust system": every self-initiated action class is
// governed by the earned ledger, not only incident families. A limiter
// that implements AutonomyScope says which requests it governs. For a
// governed self-initiated request the trust ramp no longer grants (or
// blocks) anything at authorization time: the ledger level decides, the
// ramp is a floor on promotion proposals. Rollbacks, operator approvals
// and owner-declared actions are never governed. The operator's trust
// level, execution mode, tier flags and windows stay the outer bound.

// scopedLimiter governs every request whose action type it lists.
type scopedLimiter struct {
	fakeLimiter
	governed map[string]bool
}

func (s *scopedLimiter) Governs(req ActionRequest) bool {
	return req.Contract != nil && s.governed[req.Contract.ActionType]
}

func newScopedFixture(level int) (*autonomyFixture, *scopedLimiter) {
	f := newAutonomyFixture(level)
	scoped := &scopedLimiter{governed: map[string]bool{
		"vacuum_table": true, "create_index_concurrently": true,
		"drop_unused_index": true,
	}}
	scoped.limit = AutonomyLimit{Level: level, Granted: level, Family: "hygiene",
		Class: "vacuum"}
	return f, scoped
}

func (f *autonomyFixture) scopedGate(limiter AutonomyLimiter) Gate {
	cfg := GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return f.runtime, nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy:      func(context.Context, ActionRequest) (Document, error) { return f.doc, nil },
		Usage: func(context.Context, ActionRequest) (LimitUsage, error) {
			return LimitUsage{}, nil
		},
		RecordDecision: func(_ context.Context, _ ActionRequest, d Decision) (string, error) {
			f.records = append(f.records, d)
			return "decision-1", nil
		},
		Now:      func() time.Time { return f.now },
		Autonomy: limiter,
	}
	return NewGate(cfg)
}

func selfVacuumRequest() ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: "vacuum_table", RiskTier: RiskSafe,
			RollbackClass: RollbackNoRollbackNeeded},
		SQL: `VACUUM public.orders`, TargetObjs: []string{"public.orders"},
		Feature: "vacuum",
	}
}

func selfIndexRequest() ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: "create_index_concurrently",
			RiskTier: RiskModerate, RollbackClass: RollbackReversible},
		SQL:        `CREATE INDEX CONCURRENTLY idx_o ON public.orders (status)`,
		TargetObjs: []string{"public.orders"}, Feature: "index",
	}
}

// The ramp is elapsed (60 days) and trust is autonomous, yet a governed
// class at L1 only gets a manual script: time alone never grants.
func TestUnifiedTrustRampAloneNeverGrants(t *testing.T) {
	f, limiter := newScopedFixture(1)
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	assertDecision(t, d, VerdictObserveOnly, ReasonAutonomyLevel)
	if limiter.calls != 1 {
		t.Fatalf("ledger consulted %d times, want 1", limiter.calls)
	}
	if limiter.seen[0].Contract.ActionType != "vacuum_table" {
		t.Fatalf("ledger saw %+v", limiter.seen[0])
	}
}

// An earned (or grandfathered) L3 executes even though the ramp has not
// elapsed: the ramp is a promotion floor, not an authorization check.
func TestUnifiedTrustEarnedL3ExecutesBeforeTheRamp(t *testing.T) {
	f, limiter := newScopedFixture(3)
	f.runtime.RampStart = f.now.Add(-time.Hour)
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	assertDecision(t, d, VerdictExecute, ReasonAutonomyL3)
	if d.OffWindowOK {
		t.Fatal("an L3 execution must not carry an off-window override")
	}
}

// A request the scope does not govern keeps the ramp: it is neither
// granted nor withheld by the ledger.
func TestUnifiedTrustUngovernedRequestKeepsTheRamp(t *testing.T) {
	f, limiter := newScopedFixture(3)
	f.runtime.RampStart = f.now.Add(-time.Hour)
	req := selfVacuumRequest()
	req.Contract.ActionType = "alter_table"
	req.Contract.RiskTier = RiskSafe
	d := f.scopedGate(limiter).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictBlocked, ReasonTrustRampNotSatisfied)
	if limiter.calls != 0 {
		t.Fatal("the ledger was consulted for a request it does not govern")
	}
}

// A rollback undoes pg_sage's own change: the ledger never withholds it
// (an index_create class at L1 must not block the re-create of a dropped
// index). The ramp and tiers still apply as before.
func TestUnifiedTrustNeverGovernsRollbacks(t *testing.T) {
	f, limiter := newScopedFixture(0)
	req := selfIndexRequest()
	req.Rollback = true
	d := f.scopedGate(limiter).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictExecute, ReasonAuthorized)
	if limiter.calls != 0 {
		t.Fatal("the ledger was consulted for a rollback")
	}
	f.runtime.RampStart = f.now.Add(-time.Hour)
	d = f.scopedGate(limiter).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictBlocked, ReasonTrustRampNotSatisfied)
}

func TestUnifiedTrustNeverGovernsOwnerDeclaredOrOperatorRequests(t *testing.T) {
	f, limiter := newScopedFixture(0)
	owner := selfVacuumRequest()
	owner.OwnerDeclared = true
	assertDecision(t, f.scopedGate(limiter).Authorize(context.Background(), owner),
		VerdictExecute, ReasonAuthorized)
	op := selfVacuumRequest()
	op.OperatorApproved = true
	assertDecision(t, f.scopedGate(limiter).Authorize(context.Background(), op),
		VerdictExecute, ReasonOperatorApproved)
	if limiter.calls != 0 {
		t.Fatalf("ledger consulted %d times for owner/operator requests", limiter.calls)
	}
}

// An owner declaration does not exempt an incident-family request: the
// family is pg_sage's own remediation.
func TestUnifiedTrustOwnerDeclarationDoesNotExemptIncidentFamilies(t *testing.T) {
	f, limiter := newScopedFixture(1)
	req := familyRequest(RiskSafe, RollbackReversible)
	req.OwnerDeclared = true
	d := f.scopedGate(limiter).Authorize(context.Background(), req)
	if d.Verdict != VerdictObserveOnly || limiter.calls != 1 {
		t.Fatalf("decision %+v after %d ledger calls", d, limiter.calls)
	}
}

// A limiter without AutonomyScope keeps the M7 contract: only incident
// families are governed.
func TestUnifiedTrustLimiterWithoutScopeGovernsOnlyFamilies(t *testing.T) {
	f := newAutonomyFixture(1)
	d := f.scopedGate(f.limiter).Authorize(context.Background(), selfVacuumRequest())
	assertDecision(t, d, VerdictExecute, ReasonAuthorized)
	if f.limiter.calls != 0 {
		t.Fatal("an unscoped limiter was consulted for a request without a family")
	}
}

// The trust level stays the ceiling: advisory never auto-executes a
// MODERATE class, whatever the ledger granted.
func TestUnifiedTrustAdvisoryCeilingHandsOffModerate(t *testing.T) {
	f, limiter := newScopedFixture(3)
	f.runtime.TrustLevel = TrustAdvisory
	d := f.scopedGate(limiter).Authorize(context.Background(), selfIndexRequest())
	assertDecision(t, d, VerdictQueueApproval, ReasonAutonomyHandoff)
}

func TestUnifiedTrustObservationCeilingWins(t *testing.T) {
	f, limiter := newScopedFixture(3)
	f.runtime.TrustLevel = TrustObservation
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	if d.Verdict != VerdictObserveOnly || d.Reason != ReasonObserveOnly {
		t.Fatalf("observation trust decision = %+v", d)
	}
	if limiter.calls != 0 {
		t.Fatal("the ledger was consulted under observation trust")
	}
}

func TestUnifiedTrustTierFlagOffBlocksEvenAtL3(t *testing.T) {
	f, limiter := newScopedFixture(3)
	f.runtime.Tier3Safe = false
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	if d.Verdict == VerdictExecute {
		t.Fatalf("tier3_safe=false executed at L3: %+v", d)
	}
}

// Self-initiated SAFE classes never needed a maintenance window: an L3
// vacuum outside the window executes. MODERATE classes keep needing one.
func TestUnifiedTrustL3WindowFollowsTheTier(t *testing.T) {
	f, limiter := newScopedFixture(3)
	f.doc.MaintenanceWindows = []string{"weekdays 09:00-17:00"} // closed: Sunday 02:00
	f.runtime.InConfiguredWindow = false
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	assertDecision(t, d, VerdictExecute, ReasonAutonomyL3)
	d = f.scopedGate(limiter).Authorize(context.Background(), selfIndexRequest())
	if d.Verdict == VerdictExecute {
		t.Fatalf("a MODERATE class executed outside the window: %+v", d)
	}
}

// Incident families keep the strict L3 window (M7 behaviour unchanged).
func TestUnifiedTrustIncidentFamilyL3StillNeedsTheWindow(t *testing.T) {
	f, limiter := newScopedFixture(3)
	f.doc.MaintenanceWindows = []string{"weekdays 09:00-17:00"} // closed: Sunday 02:00
	f.doc.DeadlineOverrides = nil
	req := familyRequest(RiskSafe, RollbackReversible)
	d := f.scopedGate(limiter).Authorize(context.Background(), req)
	if d.Verdict == VerdictExecute {
		t.Fatalf("an incident-family L3 executed outside the window: %+v", d)
	}
}

func TestUnifiedTrustL3NeedsExactlyOneTarget(t *testing.T) {
	f, limiter := newScopedFixture(3)
	for _, targets := range [][]string{nil, {"public.a", "public.b"}} {
		req := selfVacuumRequest()
		req.TargetObjs = targets
		d := f.scopedGate(limiter).Authorize(context.Background(), req)
		assertDecision(t, d, VerdictQueueApproval, ReasonAutonomyHandoff)
	}
}

func TestUnifiedTrustLedgerErrorFailsClosed(t *testing.T) {
	f, limiter := newScopedFixture(3)
	limiter.err = errors.New("ledger unreachable")
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	if d.Verdict != VerdictBlocked || d.Reason != ReasonAutonomyUnavailable ||
		d.Detail != "ledger unreachable" {
		t.Fatalf("decision on ledger error = %+v", d)
	}
}

// The ledger can only restrict: an irreversible class granted L3 by a
// wrong ledger answer never executes.
func TestUnifiedTrustIrreversibleNeverExceedsL1(t *testing.T) {
	f, limiter := newScopedFixture(3)
	req := selfIndexRequest()
	req.Contract.RollbackClass = RollbackNotReversible
	d := f.scopedGate(limiter).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictObserveOnly, ReasonAutonomyLevel)
}

func TestUnifiedTrustNoteNamesTheFamily(t *testing.T) {
	f, limiter := newScopedFixture(1)
	d := f.scopedGate(limiter).Authorize(context.Background(), selfVacuumRequest())
	want := "autonomy L1 (granted L1, hygiene/vacuum)"
	if len(d.Detail) < len(want) || d.Detail[:len(want)] != want {
		t.Fatalf("detail = %q, want prefix %q", d.Detail, want)
	}
}

// RampAge is exported for the ledger's promotion floor and must match
// the gate's rule: zero takes the spec, a configured ramp never drops
// below an hour, an irreversible action never below the spec.
func TestRampAgeExportedMatchesTheGateRule(t *testing.T) {
	cases := []struct {
		configured time.Duration
		rollback   RollbackClass
		want       time.Duration
	}{
		{0, RollbackReversible, SpecModerateRampAge},
		{-time.Hour, RollbackReversible, SpecModerateRampAge},
		{2 * time.Hour, RollbackReversible, 2 * time.Hour},
		{time.Minute, RollbackReversible, MinRampAge},
		{2 * time.Hour, RollbackNotReversible, SpecModerateRampAge},
		{2 * time.Hour, RollbackNoRollbackNeeded, 2 * time.Hour},
	}
	for _, c := range cases {
		if got := RampAge(c.configured, SpecModerateRampAge, c.rollback); got != c.want {
			t.Errorf("RampAge(%v, %s) = %v, want %v", c.configured, c.rollback, got, c.want)
		}
	}
}

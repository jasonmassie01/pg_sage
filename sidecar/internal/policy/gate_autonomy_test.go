package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sage SRE M7 (AI-SRE-SPEC §7.3, CHECK-40): the standing gate consults
// the earned-autonomy ledger for self-initiated actions of an incident
// family. L0/L1 hand back a manual script (observe only), L2 hands off
// to one-click approval, L3 may auto-execute a reversible action bounded
// to one object inside the maintenance window. The operator's trust,
// execution mode, tiers and windows stay the outer bound: the ledger can
// only restrict a verdict, never widen it.

type fakeLimiter struct {
	mu    sync.Mutex
	limit AutonomyLimit
	err   error
	calls int
	seen  []ActionRequest
}

func (f *fakeLimiter) Limit(_ context.Context, req ActionRequest) (AutonomyLimit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = append(f.seen, req)
	return f.limit, f.err
}

type autonomyFixture struct {
	runtime RuntimeState
	doc     Document
	now     time.Time
	limiter *fakeLimiter
	records []Decision
}

func newAutonomyFixture(level int) *autonomyFixture {
	now := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC) // Sunday 02:00 UTC
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	return &autonomyFixture{now: now, doc: doc,
		limiter: &fakeLimiter{limit: AutonomyLimit{Level: level, Granted: level}},
		runtime: RuntimeState{ExecutorEnabled: true, TrustLevel: TrustAutonomous,
			ExecutionMode: ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
			RampStart: now.Add(-60 * 24 * time.Hour), InConfiguredWindow: true}}
}

func (f *autonomyFixture) gate(withLimiter bool) Gate {
	cfg := GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return f.runtime, nil
		},
		ValidateSQL: func(sql string) error {
			if !strings.Contains(strings.ToUpper(sql), "CONCURRENTLY") {
				return fmt.Errorf("unsafe SQL")
			}
			return nil
		},
		Policy: func(context.Context, ActionRequest) (Document, error) { return f.doc, nil },
		Usage: func(context.Context, ActionRequest) (LimitUsage, error) {
			return LimitUsage{}, nil
		},
		RecordDecision: func(_ context.Context, _ ActionRequest, d Decision) (string, error) {
			f.records = append(f.records, d)
			return "decision-1", nil
		},
		Now: func() time.Time { return f.now },
	}
	if withLimiter {
		cfg.Autonomy = f.limiter
	}
	return NewGate(cfg)
}

func familyRequest(tier RiskTier, rollback RollbackClass) ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: "create_index_concurrently",
			RiskTier: tier, RollbackClass: rollback},
		SQL:            `CREATE INDEX CONCURRENTLY idx_orders ON public.orders (status)`,
		TargetObjs:     []string{"public.orders"},
		Feature:        "index",
		IncidentFamily: "plan_regression",
	}
}

func (f *autonomyFixture) authorize(t *testing.T, req ActionRequest) Decision {
	t.Helper()
	return f.gate(true).Authorize(context.Background(), req)
}

func TestAutonomyNotConfiguredKeepsTheExistingGate(t *testing.T) {
	f := newAutonomyFixture(0)
	d := f.gate(false).Authorize(context.Background(),
		familyRequest(RiskSafe, RollbackReversible))
	assertDecision(t, d, VerdictExecute, ReasonAuthorized)
}

func TestAutonomyIgnoresRequestsWithoutAFamily(t *testing.T) {
	f := newAutonomyFixture(0)
	req := familyRequest(RiskSafe, RollbackReversible)
	req.IncidentFamily = ""
	assertDecision(t, f.authorize(t, req), VerdictExecute, ReasonAuthorized)
	if f.limiter.calls != 0 {
		t.Fatal("the ledger was consulted for a request without an incident family")
	}
}

// An operator's approval is the human step; the ledger restricts only
// what pg_sage does on its own.
func TestAutonomyDoesNotRestrictOperatorApprovals(t *testing.T) {
	f := newAutonomyFixture(0)
	req := familyRequest(RiskSafe, RollbackReversible)
	req.OperatorApproved = true
	assertDecision(t, f.authorize(t, req), VerdictExecute, ReasonOperatorApproved)
	if f.limiter.calls != 0 {
		t.Fatal("the ledger was consulted for an operator-approved request")
	}
}

func TestAutonomyIgnoresReadOnlyDiagnostics(t *testing.T) {
	f := newAutonomyFixture(0)
	req := familyRequest(RiskReadOnly, RollbackNotApplicable)
	assertDecision(t, f.authorize(t, req), VerdictExecute, ReasonAuthorized)
	if f.limiter.calls != 0 {
		t.Fatal("the ledger was consulted for a read-only diagnostic")
	}
}

func TestAutonomyLevelsMapToVerdicts(t *testing.T) {
	cases := []struct {
		level   int
		verdict Verdict
		reason  Reason
	}{
		{0, VerdictObserveOnly, ReasonAutonomyLevel},
		{1, VerdictObserveOnly, ReasonAutonomyLevel},
		{2, VerdictQueueApproval, ReasonAutonomyHandoff},
		{3, VerdictExecute, ReasonAutonomyL3},
	}
	for _, c := range cases {
		f := newAutonomyFixture(c.level)
		d := f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
		assertDecision(t, d, c.verdict, c.reason)
		if len(f.records) != 1 || f.records[0].Verdict != c.verdict ||
			f.records[0].Reason != c.reason {
			t.Fatalf("L%d recorded %+v, want the restricted verdict", c.level, f.records)
		}
		if c.level < 3 && !strings.Contains(d.Detail, string(ReasonAuthorized)) {
			t.Errorf("L%d detail %q does not keep the verdict it restricted", c.level,
				d.Detail)
		}
	}
}

func TestAutonomyL3RequiresTheWindow(t *testing.T) {
	f := newAutonomyFixture(3)
	f.doc = StaffedProfile() // weekdays 01:00-05:00; the clock is Sunday 02:00
	d := f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
	assertDecision(t, d, VerdictQueueApproval, ReasonAutonomyHandoff)
	if !strings.Contains(d.Detail, "outside_window") {
		t.Fatalf("detail = %q", d.Detail)
	}
	f = newAutonomyFixture(3)
	f.runtime.WindowConfigured, f.runtime.InConfiguredWindow = true, false
	assertDecision(t, f.authorize(t, familyRequest(RiskSafe, RollbackReversible)),
		VerdictQueueApproval, ReasonAutonomyHandoff)
	// A standing policy must declare a window: one without any does not
	// validate, and the gate fails closed before the ledger is consulted.
	f = newAutonomyFixture(3)
	f.doc.MaintenanceWindows = nil
	assertDecision(t, f.authorize(t, familyRequest(RiskSafe, RollbackReversible)),
		VerdictBlocked, ReasonPolicyUnavailable)
	if f.limiter.calls != 0 {
		t.Fatal("the ledger was consulted for an invalid policy")
	}
}

// A deadline override lets the existing gate run outside the window; at
// L3 it is still a handoff (running without a window is L4, reserved).
func TestAutonomyL3IgnoresDeadlineOverride(t *testing.T) {
	f := newAutonomyFixture(3)
	f.doc = UnattendedProfile()
	f.doc.MaintenanceWindows = []string{"weekdays 01:00-05:00"}
	req := familyRequest(RiskModerate, RollbackReversible)
	req.Deadline = &DeadlineContext{Kind: DeadlineXID, Urgency: UrgencyCritical,
		HardAt: f.now.Add(time.Hour)}
	base := f.gate(false).Authorize(context.Background(), req)
	assertDecision(t, base, VerdictExecute, ReasonDeadlineOverride)
	assertDecision(t, f.authorize(t, req), VerdictQueueApproval, ReasonAutonomyHandoff)
}

// Post-test audit (mutation P6 survived): an L3 execution is an
// in-window execution, so it never carries the deadline override's
// off-window flag into the decision evidence.
func TestAutonomyL3ClearsTheOffWindowFlag(t *testing.T) {
	f := newAutonomyFixture(3)
	f.runtime.InConfiguredWindow = false // moderate tier: the override path decides
	req := familyRequest(RiskModerate, RollbackReversible)
	req.Deadline = &DeadlineContext{Kind: DeadlineXID, Urgency: UrgencyCritical,
		HardAt: f.now.Add(time.Hour)}
	base := f.gate(false).Authorize(context.Background(), req)
	assertDecision(t, base, VerdictExecute, ReasonDeadlineOverride)
	if !base.OffWindowOK {
		t.Fatal("fixture: the base decision is not an off-window override")
	}
	d := f.authorize(t, req)
	assertDecision(t, d, VerdictExecute, ReasonAutonomyL3)
	if d.OffWindowOK {
		t.Fatal("an L3 execution kept the off-window flag")
	}
}

func TestAutonomyL3RequiresExactlyOneTarget(t *testing.T) {
	for _, targets := range [][]string{nil, {"public.a", "public.b"}} {
		f := newAutonomyFixture(3)
		req := familyRequest(RiskSafe, RollbackReversible)
		req.TargetObjs = targets
		d := f.authorize(t, req)
		assertDecision(t, d, VerdictQueueApproval, ReasonAutonomyHandoff)
		if !strings.Contains(d.Detail, "unbounded") {
			t.Fatalf("%v: detail = %q", targets, d.Detail)
		}
	}
}

// The gate caps by the contract's own reversibility even when the
// ledger (wrongly) reports a higher level.
func TestAutonomyGateCapsByReversibility(t *testing.T) {
	cases := map[RollbackClass]Verdict{
		RollbackNotReversible:                 VerdictObserveOnly,
		RollbackForwardFixOnly:                VerdictObserveOnly,
		RollbackApplication:                   VerdictObserveOnly,
		RollbackNotApplicable:                 VerdictObserveOnly,
		"":                                    VerdictObserveOnly,
		RollbackClass("mitigation_only"):      VerdictQueueApproval,
		RollbackNoRollbackNeeded:              VerdictExecute,
		RollbackReversible:                    VerdictExecute,
		RollbackClass("reversible-ish-maybe"): VerdictObserveOnly,
	}
	for rollback, want := range cases {
		f := newAutonomyFixture(3)
		f.doc.RefusalSet = nil // the refusal set is tested elsewhere
		d := f.authorize(t, familyRequest(RiskSafe, rollback))
		if d.Verdict != want {
			t.Errorf("rollback %q at L3: verdict %s (%s), want %s", rollback, d.Verdict,
				d.Reason, want)
		}
	}
}

func TestAutonomyReservedAndBogusLevels(t *testing.T) {
	for level, want := range map[int]Verdict{4: VerdictExecute, 99: VerdictExecute,
		-1: VerdictObserveOnly, -100: VerdictObserveOnly} {
		f := newAutonomyFixture(level)
		d := f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
		if d.Verdict != want {
			t.Errorf("level %d: %s (%s), want %s", level, d.Verdict, d.Reason, want)
		}
		if level >= 4 && d.Reason != ReasonAutonomyL3 {
			t.Errorf("level %d acted as %s; above L3 is treated as L3", level, d.Reason)
		}
	}
}

func TestAutonomyDowngradedNeverExecutes(t *testing.T) {
	f := newAutonomyFixture(3)
	f.limiter.limit.Downgraded = true
	f.limiter.limit.Reasons = []string{"error_budget_fast_burn"}
	d := f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
	assertDecision(t, d, VerdictObserveOnly, ReasonAutonomyDowngraded)
	if !strings.Contains(d.Detail, "error_budget_fast_burn") {
		t.Fatalf("detail = %q", d.Detail)
	}
}

func TestAutonomyLedgerFailureFailsClosed(t *testing.T) {
	f := newAutonomyFixture(3)
	f.limiter.err = errors.New("ledger unreachable")
	d := f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
	assertDecision(t, d, VerdictBlocked, ReasonAutonomyUnavailable)
	if !strings.Contains(d.Detail, "ledger unreachable") {
		t.Fatalf("detail = %q", d.Detail)
	}
}

// The operator's settings remain the outer bound at L3.
func TestAutonomyNeverWidensTheOperatorBound(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*autonomyFixture)
		verdict Verdict
		reason  Reason
	}{
		"approval mode": {func(f *autonomyFixture) { f.runtime.ExecutionMode = ExecutionApproval },
			VerdictQueueApproval, ReasonAutonomyHandoff},
		"manual mode": {func(f *autonomyFixture) { f.runtime.ExecutionMode = ExecutionManual },
			VerdictObserveOnly, ReasonObserveOnly},
		"observation": {func(f *autonomyFixture) { f.runtime.TrustLevel = TrustObservation },
			VerdictObserveOnly, ReasonObserveOnly},
		"ramp": {func(f *autonomyFixture) { f.runtime.Tier3Safe = false },
			VerdictBlocked, ReasonTrustRampNotSatisfied},
		"emergency stop": {func(f *autonomyFixture) { f.runtime.EmergencyStop = true },
			VerdictBlocked, ReasonEmergencyStop},
		"replica": {func(f *autonomyFixture) { f.runtime.IsReplica = true },
			VerdictBlocked, ReasonReplicaMutation},
		"class not allowed": {func(f *autonomyFixture) {
			f.doc.AllowedChangeClasses = []ChangeClass{ChangeAnalyze}
		}, VerdictBlocked, ReasonChangeClassNotAllowed},
	}
	for name, c := range cases {
		f := newAutonomyFixture(3)
		c.mutate(f)
		d := f.authorize(t, familyRequest(RiskSafe, RollbackReversible))
		if d.Verdict != c.verdict || d.Reason != c.reason {
			t.Errorf("%s: %s/%s, want %s/%s", name, d.Verdict, d.Reason, c.verdict, c.reason)
		}
	}
}

// At L1 an approval-required class is a manual script, not a queued
// approval: the one-click handoff is L2 behavior.
func TestAutonomyL1WithholdsTheHandoff(t *testing.T) {
	f := newAutonomyFixture(1)
	f.runtime.ExecutionMode = ExecutionApproval
	assertDecision(t, f.authorize(t, familyRequest(RiskSafe, RollbackReversible)),
		VerdictObserveOnly, ReasonAutonomyLevel)
}

func TestAutonomyExplainMatchesAuthorize(t *testing.T) {
	for _, level := range []int{0, 1, 2, 3} {
		f := newAutonomyFixture(level)
		gate := f.gate(true)
		explained := mustExplainer(t, gate).Explain(context.Background(),
			familyRequest(RiskSafe, RollbackReversible))
		authorized := gate.Authorize(context.Background(),
			familyRequest(RiskSafe, RollbackReversible))
		if explained.Verdict != authorized.Verdict || explained.Reason != authorized.Reason {
			t.Fatalf("L%d: explain %s/%s, authorize %s/%s", level, explained.Verdict,
				explained.Reason, authorized.Verdict, authorized.Reason)
		}
	}
}

// The no-premature-autonomy property, exhaustively: over every
// combination of ledger level, downgrade, reversibility, trust level,
// execution mode, risk tier, window and target count, the gate executes
// a self-initiated family action only when the level is at least L3,
// nothing is downgraded, the contract is reversible, exactly one object
// is targeted, the operator allows autonomous execution and the
// maintenance window is open.
func TestAutonomyGateNeverExecutesPrematurely(t *testing.T) {
	levels := []int{-1, 0, 1, 2, 3, 4, 9}
	rollbacks := []RollbackClass{RollbackReversible, RollbackNoRollbackNeeded,
		RollbackClass("mitigation_only"), RollbackNotReversible, RollbackForwardFixOnly,
		RollbackApplication, RollbackNotApplicable, ""}
	trusts := []string{TrustObservation, TrustAdvisory, TrustAutonomous}
	modes := []string{ExecutionAuto, ExecutionApproval, ExecutionManual}
	tiers := []RiskTier{RiskSafe, RiskModerate, RiskHigh}
	targetSets := [][]string{nil, {"public.orders"}, {"public.orders", "public.items"}}
	executed, total := 0, 0
	for _, level := range levels {
		for _, downgraded := range []bool{false, true} {
			for _, rollback := range rollbacks {
				for _, trust := range trusts {
					for _, mode := range modes {
						for _, tier := range tiers {
							for _, inWindow := range []bool{false, true} {
								for _, targets := range targetSets {
									total++
									f := newAutonomyFixture(level)
									f.limiter.limit.Downgraded = downgraded
									f.runtime.TrustLevel, f.runtime.ExecutionMode = trust, mode
									if !inWindow {
										f.doc.MaintenanceWindows = []string{"weekdays 01:00-05:00"}
									}
									req := familyRequest(tier, rollback)
									req.TargetObjs = targets
									d := f.authorize(t, req)
									// The operator's own bound: safe actions run
									// under advisory or autonomous trust, moderate
									// ones only under autonomous, high never.
									operatorAllows := mode == ExecutionAuto &&
										((tier == RiskSafe && trust != TrustObservation) ||
											(tier == RiskModerate && trust == TrustAutonomous))
									want := level >= 3 && !downgraded &&
										(rollback == RollbackReversible ||
											rollback == RollbackNoRollbackNeeded) &&
										operatorAllows && inWindow && len(targets) == 1
									if (d.Verdict == VerdictExecute) != want {
										t.Fatalf("level=%d down=%v rollback=%q trust=%s mode=%s "+
											"tier=%s window=%v targets=%d: %s/%s (%s)", level,
											downgraded, rollback, trust, mode, tier, inWindow,
											len(targets), d.Verdict, d.Reason, d.Detail)
									}
									if d.Verdict == VerdictExecute {
										executed++
										if d.Reason != ReasonAutonomyL3 {
											t.Fatalf("executed with reason %s", d.Reason)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if executed == 0 || total != 18144 {
		t.Fatalf("matrix: %d combinations, %d executed", total, executed)
	}
}

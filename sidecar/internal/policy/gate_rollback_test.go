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

// policy-05: a revert of an executed or operator-approved change is a
// mitigation. Windows, budgets, per-class autonomy and the change-class
// allowlist do not bind it; the hard stops, the provider list, trust
// observation, the rollback contract's approval guardrail and the lock
// ceiling do, and every decision is recorded.

type rollbackRecorder struct {
	mu        sync.Mutex
	decisions []Decision
	requests  []ActionRequest
}

func (r *rollbackRecorder) record(
	_ context.Context, req ActionRequest, d Decision,
) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, d)
	r.requests = append(r.requests, req)
	return fmt.Sprintf("evidence-%d", len(r.decisions)), nil
}

func (r *rollbackRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.decisions)
}

type rollbackFixture struct {
	runtime   RuntimeState
	doc       Document
	policyErr error
	usageHits int
}

// rollbackNow is 14:00 UTC on a Wednesday: outside "weekdays 01:00-05:00".
var rollbackNow = time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)

func newRollbackFixture() *rollbackFixture {
	doc := StaffedProfile()
	doc.MaintenanceWindows = []string{"weekdays 01:00-05:00"}
	doc.LockDurationCeilingMS = 2500
	return &rollbackFixture{doc: doc, runtime: RuntimeState{ExecutorEnabled: true,
		TrustLevel: TrustAdvisory, ExecutionMode: ExecutionApproval,
		WindowConfigured: true}}
}

func (f *rollbackFixture) gate(rec *rollbackRecorder) Gate {
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return f.runtime, nil
		},
		ValidateSQL: func(sql string) error {
			if strings.TrimSpace(sql) == "" {
				return errors.New("empty statement")
			}
			return nil
		},
		Policy: func(context.Context, ActionRequest) (Document, error) {
			return f.doc, f.policyErr
		},
		Usage: func(context.Context, ActionRequest) (LimitUsage, error) {
			f.usageHits++
			return LimitUsage{SelfInitiatedChangesInWindow: 1 << 20}, nil
		},
		RecordDecision: rec.record,
		Now:            func() time.Time { return rollbackNow },
	})
}

// revertRequest is the revert of a created index: DROP INDEX CONCURRENTLY.
func revertRequest(operatorApproved bool) ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: "revert_created_index", RiskTier: RiskModerate,
			RollbackClass: RollbackReversible},
		SQL:        "DROP INDEX CONCURRENTLY IF EXISTS public.orders_status_idx",
		TargetObjs: []string{"public.orders"}, Feature: string(ChangeIndex),
		Rollback: true, OperatorApproved: operatorApproved,
	}
}

func TestRollbackOutsideEveryWindowExecutes(t *testing.T) {
	for _, approved := range []bool{false, true} {
		rec := &rollbackRecorder{}
		f := newRollbackFixture()
		d := f.gate(rec).Authorize(context.Background(), revertRequest(approved))
		assertDecision(t, d, VerdictExecute, ReasonRollbackMitigation)
		if d.LockCeilingMS != 2500 {
			t.Fatalf("approved=%v: lock ceiling = %d, want the policy's 2500", approved,
				d.LockCeilingMS)
		}
		if rec.count() != 1 || rec.decisions[0].Verdict != VerdictExecute {
			t.Fatalf("approved=%v: recorded %+v, want one execute", approved, rec.decisions)
		}
		if f.usageHits != 0 {
			t.Fatalf("approved=%v: a revert read budget usage %d times", approved, f.usageHits)
		}
	}
}

// Without the rollback branch a self-initiated revert fell to the forward
// path: the 14:00 window or the advisory tier withheld it.
func TestForwardRequestStillBoundByWindow(t *testing.T) {
	rec := &rollbackRecorder{}
	forward := revertRequest(true)
	forward.Rollback = false
	d := newRollbackFixture().gate(rec).Authorize(context.Background(), forward)
	assertDecision(t, d, VerdictBlocked, ReasonOutsideMaintenanceWindow)
}

func TestRollbackPassesTierRampAndChangeClass(t *testing.T) {
	f := newRollbackFixture()
	f.runtime.TrustLevel = TrustAutonomous
	f.runtime.ExecutionMode = ExecutionManual // observeOnly for forward work
	f.doc.AllowedChangeClasses = []ChangeClass{ChangeVacuum}
	rec := &rollbackRecorder{}
	d := f.gate(rec).Authorize(context.Background(), revertRequest(false))
	assertDecision(t, d, VerdictExecute, ReasonRollbackMitigation)
	if d.BudgetKind != BudgetBypass || !strings.Contains(d.Detail, "budget bypass") {
		t.Fatalf("automatic revert budget = %q detail %q, want bypass", d.BudgetKind, d.Detail)
	}
}

func TestRollbackHardStopsBindAndAreRecorded(t *testing.T) {
	cases := map[string]struct {
		mutate func(*RuntimeState)
		reason Reason
	}{
		"replica":   {func(r *RuntimeState) { r.IsReplica = true }, ReasonReplicaMutation},
		"disabled":  {func(r *RuntimeState) { r.ExecutorEnabled = false }, ReasonExecutorDisabled},
		"emergency": {func(r *RuntimeState) { r.EmergencyStop = true }, ReasonEmergencyStop},
	}
	for name, c := range cases {
		for _, approved := range []bool{false, true} {
			f := newRollbackFixture()
			c.mutate(&f.runtime)
			rec := &rollbackRecorder{}
			d := f.gate(rec).Authorize(context.Background(), revertRequest(approved))
			assertDecision(t, d, VerdictBlocked, c.reason)
			if rec.count() != 1 || rec.decisions[0].Reason != c.reason ||
				!rec.requests[0].Rollback {
				t.Fatalf("%s approved=%v: recorded %+v", name, approved, rec.decisions)
			}
		}
	}
}

func TestRollbackProviderAndTrustBind(t *testing.T) {
	f := newRollbackFixture()
	f.runtime.Provider = "cloud-sql"
	req := revertRequest(true)
	req.Contract.ProviderSupport = []string{"self-managed"}
	assertDecision(t, f.gate(&rollbackRecorder{}).Authorize(context.Background(), req),
		VerdictBlocked, ReasonProviderUnsupported)

	f = newRollbackFixture()
	f.runtime.TrustLevel = TrustObservation
	for _, approved := range []bool{false, true} {
		d := f.gate(&rollbackRecorder{}).Authorize(context.Background(),
			revertRequest(approved))
		assertDecision(t, d, VerdictObserveOnly, ReasonObserveOnly)
	}
	f.runtime.TrustLevel = "superuser"
	assertDecision(t, f.gate(&rollbackRecorder{}).Authorize(context.Background(),
		revertRequest(true)), VerdictBlocked, ReasonUnknownTrustLevel)
}

func TestApprovalRequiredRollbackContractQueuesUnlessApproved(t *testing.T) {
	approvalReq := func(approved bool) ActionRequest {
		req := revertRequest(approved)
		req.Contract = &ActionContract{ActionType: "retire_query_hint", RiskTier: RiskSafe,
			Guardrails: []Guardrail{GuardrailApprovalRequired}}
		req.SQL = "DELETE FROM hint_plan.hints WHERE query_id = 42"
		req.Feature = string(ChangeQueryHint)
		return req
	}
	rec := &rollbackRecorder{}
	gate := newRollbackFixture().gate(rec)
	assertDecision(t, gate.Authorize(context.Background(), approvalReq(false)),
		VerdictQueueApproval, ReasonApprovalRequired)
	assertDecision(t, gate.Authorize(context.Background(), approvalReq(true)),
		VerdictExecute, ReasonRollbackMitigation)
	if rec.count() != 2 {
		t.Fatalf("recorded %d decisions, want 2", rec.count())
	}
}

func TestAutomaticRevertHonoursApprovalClassesAndRefusals(t *testing.T) {
	f := newRollbackFixture()
	f.doc.ApprovalRequiredClasses = []ChangeClass{ChangeIndex}
	gate := f.gate(&rollbackRecorder{})
	assertDecision(t, gate.Authorize(context.Background(), revertRequest(false)),
		VerdictQueueApproval, ReasonApprovalRequired)
	assertDecision(t, gate.Authorize(context.Background(), revertRequest(true)),
		VerdictExecute, ReasonRollbackMitigation)

	f = newRollbackFixture()
	f.doc.RefusalSet = []string{RefusalUnrollbackable}
	if err := ValidateDocument(f.doc); err != nil {
		t.Fatalf("fixture document: %v", err)
	}
	refused := revertRequest(false)
	refused.Contract.RollbackClass = RollbackForwardFixOnly
	d := f.gate(&rollbackRecorder{}).Authorize(context.Background(), refused)
	assertDecision(t, d, VerdictQueueApproval, ReasonRefusedByPolicy)
	refused.OperatorApproved = true
	assertDecision(t, f.gate(&rollbackRecorder{}).Authorize(context.Background(), refused),
		VerdictExecute, ReasonRollbackMitigation)
}

func TestRollbackWithoutContractIsParkedAndRecorded(t *testing.T) {
	for name, contract := range map[string]*ActionContract{
		"nil": nil, "empty action type": {RiskTier: RiskSafe},
	} {
		rec := &rollbackRecorder{}
		req := revertRequest(true)
		req.Contract = contract
		d := newRollbackFixture().gate(rec).Authorize(context.Background(), req)
		assertDecision(t, d, VerdictPark, ReasonNoTypedContract)
		if rec.count() != 1 || rec.decisions[0].Reason != ReasonNoTypedContract {
			t.Fatalf("%s: recorded %+v, want one park", name, rec.decisions)
		}
		if contract == nil && rec.decisions[0].RiskTier != RiskHigh {
			t.Fatalf("no contract recorded at tier %q, want high", rec.decisions[0].RiskTier)
		}
	}
}

func TestRollbackWithEmptySQLIsParked(t *testing.T) {
	req := revertRequest(false)
	req.SQL = "  "
	d := newRollbackFixture().gate(&rollbackRecorder{}).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictPark, ReasonNoTypedContract)
}

func TestRollbackUnknownRiskTierIsBlocked(t *testing.T) {
	req := revertRequest(true)
	req.Contract.RiskTier = "extreme"
	d := newRollbackFixture().gate(&rollbackRecorder{}).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictBlocked, ReasonUnknownRiskTier)
}

func TestRollbackPolicyReadErrorRunsUnderSafetyTimeout(t *testing.T) {
	cases := map[string]func(*rollbackFixture){
		"store unavailable": func(f *rollbackFixture) {
			f.policyErr = errors.New("connection refused reading sage.policy")
		},
		"invalid document": func(f *rollbackFixture) { f.doc.SerializeMode = "bogus" },
	}
	for name, mutate := range cases {
		f := newRollbackFixture()
		mutate(f)
		rec := &rollbackRecorder{}
		d := f.gate(rec).Authorize(context.Background(), revertRequest(false))
		assertDecision(t, d, VerdictExecute, ReasonRollbackMitigation)
		if d.LockCeilingMS != 0 {
			t.Fatalf("%s: ceiling %d, want none (safety.lock_timeout_ms)", name,
				d.LockCeilingMS)
		}
		if d.PolicyReadError == "" || !strings.Contains(d.Detail, d.PolicyReadError) ||
			!strings.Contains(d.Detail, "safety.lock_timeout_ms") {
			t.Fatalf("%s: read error %q detail %q", name, d.PolicyReadError, d.Detail)
		}
		if rec.count() != 1 || rec.decisions[0].PolicyReadError != d.PolicyReadError {
			t.Fatalf("%s: recorded %+v", name, rec.decisions)
		}
	}
}

// The soft-drop restore keeps its own decision (executor-01): it passes
// observation trust, which a generic revert does not.
func TestSelfInitiatedDropRestoreKeepsItsOwnDecision(t *testing.T) {
	f := newRollbackFixture()
	f.runtime.TrustLevel = TrustObservation
	f.doc.MaintenanceWindows = []string{"always"}
	req := ActionRequest{
		Contract: &ActionContract{ActionType: ActionRestoreDroppedIndex, RiskTier: RiskSafe},
		SQL:      "CREATE INDEX CONCURRENTLY i ON public.t (a)", Feature: string(ChangeIndex),
		TargetObjs: []string{"public.t"}, Rollback: true,
	}
	assertDecision(t, f.gate(&rollbackRecorder{}).Authorize(context.Background(), req),
		VerdictExecute, ReasonDropRestore)
	req.OperatorApproved = true
	assertDecision(t, f.gate(&rollbackRecorder{}).Authorize(context.Background(), req),
		VerdictObserveOnly, ReasonObserveOnly)
}

func TestRollbackMitigationIsNarrow(t *testing.T) {
	restore := ActionRequest{Rollback: true,
		Contract: &ActionContract{ActionType: ActionRestoreDroppedIndex}}
	cases := []struct {
		name string
		req  ActionRequest
		want bool
	}{
		{"operator revert", revertRequest(true), true},
		{"automatic revert", revertRequest(false), true},
		{"not a rollback", ActionRequest{OperatorApproved: true}, false},
		{"zero request", ActionRequest{}, false},
		{"self-initiated drop restore", restore, false},
	}
	approvedRestore := restore
	approvedRestore.OperatorApproved = true
	cases = append(cases, struct {
		name string
		req  ActionRequest
		want bool
	}{"approved drop restore", approvedRestore, true})
	for _, c := range cases {
		if got := rollbackMitigation(c.req); got != c.want {
			t.Fatalf("%s: rollbackMitigation = %v, want %v", c.name, got, c.want)
		}
	}
}

// Concurrent reverts each get their own recorded decision.
func TestConcurrentRollbacksAreEachRecorded(t *testing.T) {
	rec := &rollbackRecorder{}
	gate := newRollbackFixture().gate(rec)
	var wg sync.WaitGroup
	verdicts := make(chan Verdict, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(approved bool) {
			defer wg.Done()
			verdicts <- gate.Authorize(context.Background(), revertRequest(approved)).Verdict
		}(i%2 == 0)
	}
	wg.Wait()
	close(verdicts)
	for v := range verdicts {
		if v != VerdictExecute {
			t.Fatalf("concurrent revert verdict %q, want execute", v)
		}
	}
	if rec.count() != 8 {
		t.Fatalf("recorded %d decisions, want 8", rec.count())
	}
}

// Explain answers as Authorize would and records nothing.
func TestExplainRollbackRecordsNothing(t *testing.T) {
	rec := &rollbackRecorder{}
	explainer := newRollbackFixture().gate(rec).(Explainer)
	d := explainer.Explain(context.Background(), revertRequest(true))
	if d.Verdict != VerdictExecute || d.Reason != ReasonRollbackMitigation {
		t.Fatalf("explain = %+v", d)
	}
	if rec.count() != 0 {
		t.Fatalf("explain recorded %d decisions", rec.count())
	}
}

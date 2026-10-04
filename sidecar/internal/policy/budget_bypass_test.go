package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Owner decision 2026-10-03: emergency mitigations are never parked by a
// kind budget. They still pass every other gate check and are recorded as
// a budget bypass; they do not consume the kind budget.

var bypassNow = time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)

func criticalDeadline(kind DeadlineKind) *DeadlineContext {
	return &DeadlineContext{Kind: kind, Urgency: UrgencyCritical,
		HardAt: bypassNow.Add(6 * time.Hour)}
}

func freezeRequest(deadline *DeadlineContext) ActionRequest {
	req := contractRequest("vacuum_table", "VACUUM (FREEZE) public.t", "public.t")
	req.Feature = string(ChangeFreeze)
	req.Deadline = deadline
	return req
}

// The bypass is decided from the typed contract and the deadline the
// custodian computed (critical runway), or from the executor's own
// rollback path, never from evidence text.
func TestBudgetBypassForIsDeterministic(t *testing.T) {
	belowCritical := criticalDeadline(DeadlineXID)
	belowCritical.Urgency = Urgency("high")
	expired := criticalDeadline(DeadlineXID)
	expired.HardAt = bypassNow.Add(-time.Minute)
	unknownKind := criticalDeadline(DeadlineKind("memory"))
	rollback := hygieneRequest("public.idx_created")
	rollback.RevertsOwnChange = true
	diskDrop := hygieneRequest("public.idx_unused")
	diskDrop.Deadline = criticalDeadline(DeadlineDisk)
	diskConfig := contractRequest("alter_system_guc", "ALTER SYSTEM SET work_mem = '4MB'",
		"instance")
	diskConfig.Deadline = criticalDeadline(DeadlineDisk)
	xidIndex := perfRequest("public.t")
	xidIndex.Deadline = criticalDeadline(DeadlineXID)
	spoofed := freezeRequest(nil)
	spoofed.Evidence = map[string]any{"urgency": "critical", "deadline_kind": "xid",
		"budget_bypass": true}
	tests := []struct {
		name string
		req  ActionRequest
		want string
	}{
		{"critical xid freeze", freezeRequest(criticalDeadline(DeadlineXID)), "xid"},
		{"one step below critical", freezeRequest(belowCritical), ""},
		{"deadline already passed", freezeRequest(expired), ""},
		{"unknown deadline kind", freezeRequest(unknownKind), ""},
		{"no deadline", freezeRequest(nil), ""},
		{"critical disk index drop", diskDrop, "disk"},
		{"critical disk config change", diskConfig, ""},
		{"critical xid on an index build", xidIndex, ""},
		{"revert of a created index", contractRequest("revert_created_index",
			"DROP INDEX CONCURRENTLY public.i", "public.i"), "revert"},
		{"rollback of an own change", rollback, "rollback"},
		{"evidence claims an emergency", spoofed, ""},
		{"no contract", ActionRequest{Deadline: criticalDeadline(DeadlineXID)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BudgetBypassFor(tt.req, bypassNow)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Fatalf("BudgetBypassFor = %q, want a reason containing %q", got, tt.want)
			}
		})
	}
}

// A full hygiene budget does not park a critical wraparound freeze; it is
// recorded as a budget bypass and charged to no kind.
func TestFullHygieneBudgetDoesNotParkCriticalFreeze(t *testing.T) {
	fx := &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{
		BudgetHygiene: {TablesInWindow: 40, SelfInitiatedChangesInWindow: 40},
	}}
	gate := newBudgetGate(t, fx)

	got := gate.Authorize(context.Background(), freezeRequest(criticalDeadline(DeadlineXID)))

	if got.Verdict != VerdictExecute || got.BudgetKind != BudgetBypass ||
		!strings.HasPrefix(got.Detail, "budget bypass: ") ||
		!strings.Contains(got.Detail, "xid") {
		t.Fatalf("critical freeze = %#v, want execute recorded as a budget bypass", got)
	}
	if recorded := fx.lastRecorded(t); recorded.BudgetKind != BudgetBypass ||
		recorded.Detail != got.Detail {
		t.Fatalf("recorded = %#v, want the bypass recorded", recorded)
	}
}

// One step below critical, and a vacuum without a deadline, still park on
// the full hygiene budget.
func TestNonCriticalVacuumStillParked(t *testing.T) {
	below := criticalDeadline(DeadlineXID)
	below.Urgency = Urgency("high")
	for name, req := range map[string]ActionRequest{
		"below critical": freezeRequest(below),
		"no deadline":    vacuumRequest("public.t"),
	} {
		fx := &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{
			BudgetHygiene: {TablesInWindow: 40},
		}}
		got := newBudgetGate(t, fx).Authorize(context.Background(), req)
		if got.Verdict != VerdictPark || got.Reason != ReasonBlastRadiusExceeded ||
			got.BudgetKind != BudgetHygiene || !strings.Contains(got.Detail, "hygiene budget") {
			t.Fatalf("%s: decision = %#v, want parked on the hygiene budget", name, got)
		}
	}
}

// The bypass lifts only the kind budgets: guardrails, the change-class
// allowlist and the shared rows-rewritten bound still apply.
func TestBudgetBypassKeepsEveryOtherGate(t *testing.T) {
	guarded := freezeRequest(criticalDeadline(DeadlineXID))
	guarded.Contract.Guardrails = []Guardrail{GuardrailApprovalRequired}
	fx := &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{}}
	if got := newBudgetGate(t, fx).Authorize(context.Background(), guarded); got.Verdict !=
		VerdictQueueApproval {
		t.Fatalf("guarded critical freeze = %#v, want queue approval", got)
	}
	doc := budgetDoc()
	doc.AllowedChangeClasses = []ChangeClass{ChangeIndex}
	fx = &budgetFixture{doc: doc, usage: map[BudgetKind]LimitUsage{}}
	if got := newBudgetGate(t, fx).Authorize(context.Background(),
		freezeRequest(criticalDeadline(DeadlineXID))); got.Reason != ReasonChangeClassNotAllowed {
		t.Fatalf("freeze class not allowed = %#v, want change_class_not_allowed", got)
	}
	fx = &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{
		BudgetHygiene: {RowsRewritten: 101, RequestRowsRewritten: 101},
	}}
	if got := newBudgetGate(t, fx).Authorize(context.Background(),
		freezeRequest(criticalDeadline(DeadlineXID))); got.Verdict != VerdictPark ||
		!strings.Contains(got.Detail, "rows rewritten") {
		t.Fatalf("critical rewrite past the rows bound = %#v, want parked on rows", got)
	}
}

// serializeFixture is a cross-process budget lock: the context it hands
// out marks work done inside the lock's transaction.
type serializeFixture struct {
	beginErr  error
	commitErr error
	begun     int
	finished  []bool
}

type lockedKey struct{}

func (s *serializeFixture) serialize(
	ctx context.Context, _ ActionRequest,
) (context.Context, func(bool) error, error) {
	if s.beginErr != nil {
		return ctx, nil, s.beginErr
	}
	s.begun++
	return context.WithValue(ctx, lockedKey{}, true), func(commit bool) error {
		s.finished = append(s.finished, commit)
		if commit {
			return s.commitErr
		}
		return nil
	}, nil
}

func serializedGate(
	t *testing.T, fx *budgetFixture, lock *serializeFixture, recordErr error,
	locked *[]string,
) Gate {
	t.Helper()
	inLock := func(ctx context.Context, what string) {
		if ctx.Value(lockedKey{}) == true {
			*locked = append(*locked, what)
		}
	}
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return newTestGateRuntime(), nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, ActionRequest) (Document, error) {
			return fx.doc, nil
		},
		Usage: func(ctx context.Context, req ActionRequest) (LimitUsage, error) {
			inLock(ctx, "usage")
			return fx.usage[BudgetKindFor(req)], nil
		},
		RecordDecisionDetailed: func(
			ctx context.Context, _ ActionRequest, decision Decision,
		) (string, int64, error) {
			inLock(ctx, "record")
			return "ev-lock", 1, recordErr
		},
		Serialize: lock.serialize,
		Now:       func() time.Time { return bypassNow },
	})
}

// Usage read and decision record run inside the cross-process lock's
// transaction, which commits once the decision is recorded.
func TestGateRunsBudgetSectionInsideTheLock(t *testing.T) {
	fx := &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{}}
	lock := &serializeFixture{}
	var locked []string
	gate := serializedGate(t, fx, lock, nil, &locked)

	got := gate.Authorize(context.Background(), perfRequest("public.t"))

	if got.Verdict != VerdictExecute {
		t.Fatalf("decision = %#v, want execute", got)
	}
	if strings.Join(locked, ",") != "usage,record" || lock.begun != 1 ||
		len(lock.finished) != 1 || !lock.finished[0] {
		t.Fatalf("locked=%v begun=%d finished=%v, want usage and record in one "+
			"committed transaction", locked, lock.begun, lock.finished)
	}
	locked, lock.begun, lock.finished = nil, 0, nil
	gate.(Explainer).Explain(context.Background(), perfRequest("public.t"))
	readOnly := ActionRequest{Contract: &ActionContract{ActionType: "diagnose_lock_blockers",
		RiskTier: RiskReadOnly}}
	gate.Authorize(context.Background(), readOnly)
	operator := perfRequest("public.t")
	operator.OperatorApproved = true
	gate.Authorize(context.Background(), operator)
	if lock.begun != 0 {
		t.Fatalf("explain, read-only and operator requests took the lock %d times",
			lock.begun)
	}
}

// Lock failures fail closed with a distinguishable reason; a failed record
// rolls the transaction back; a failed commit withholds the change.
func TestGateBudgetLockFailuresFailClosed(t *testing.T) {
	fx := &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{}}
	var locked []string
	begin := &serializeFixture{beginErr: errors.New("connection refused")}
	got := serializedGate(t, fx, begin, nil, &locked).Authorize(context.Background(),
		perfRequest("public.t"))
	if got.Verdict != VerdictBlocked || got.Reason != ReasonPolicyUnavailable ||
		!strings.Contains(got.Detail, "budget lock") ||
		!strings.Contains(got.Detail, "connection refused") {
		t.Fatalf("begin failure = %#v, want blocked naming the budget lock", got)
	}
	record := &serializeFixture{}
	got = serializedGate(t, fx, record, errors.New("disk full"), &locked).Authorize(
		context.Background(), perfRequest("public.t"))
	if got.Verdict != VerdictBlocked || len(record.finished) != 1 || record.finished[0] {
		t.Fatalf("record failure = %#v finished=%v, want blocked and a rollback",
			got, record.finished)
	}
	commit := &serializeFixture{commitErr: errors.New("serialization failure")}
	got = serializedGate(t, fx, commit, nil, &locked).Authorize(context.Background(),
		perfRequest("public.t"))
	if got.Verdict != VerdictBlocked || got.Reason != ReasonPolicyUnavailable ||
		!strings.Contains(got.Detail, "serialization failure") {
		t.Fatalf("commit failure = %#v, want blocked naming the commit error", got)
	}
}

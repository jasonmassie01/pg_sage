package policy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// One self-initiated change per object (lifeos 2026-10-04): a second
// change to a GUC or a table while the first change's verification is in
// flight makes both verdicts meaningless. The gate parks the second change
// with ReasonAwaitingVerification until the first verdict (or its hard
// deadline); emergencies and rollbacks are exempt; an operator approval
// overrides and is recorded.

type fakeTracker struct {
	mu      sync.Mutex
	pending []PendingVerification
	err     error
	calls   int
	seen    []ActionRequest
}

func (f *fakeTracker) PendingVerifications(_ context.Context, req ActionRequest) (
	[]PendingVerification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = append(f.seen, req)
	return f.pending, f.err
}

// waitNow is the gate fixture's clock.
var waitNow = time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)

func gucInFlight(id int64) PendingVerification {
	return PendingVerification{ActionID: id, Object: "guc:work_mem",
		Until: waitNow.Add(15 * time.Minute), HardDeadline: waitNow.Add(72 * time.Hour)}
}

func newWaitGate(t *testing.T, fixture gateFixture, tracker VerificationTracker) Gate {
	t.Helper()
	gate := newTestGate(t, fixture)
	// The fixture's fake validator admits only CONCURRENTLY statements;
	// these requests are settings and VACUUM, which the real one admits.
	gate.(*authorizationGate).config.ValidateSQL = func(string) error { return nil }
	gate.(*authorizationGate).config.Verification = tracker
	return gate
}

func gucRequest(value string) ActionRequest {
	return ActionRequest{
		Contract: &ActionContract{ActionType: "alter_system_guc", RiskTier: RiskSafe,
			RollbackClass: RollbackReversible},
		SQL:        "ALTER SYSTEM SET work_mem = '" + value + "'",
		TargetObjs: []string{"instance"}, Feature: "config_guc",
	}
}

func TestWaitParksASecondChangeToTheSameGUC(t *testing.T) {
	tracker := &fakeTracker{pending: []PendingVerification{gucInFlight(6407)}}
	d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(),
		gucRequest("10MB"))
	assertDecision(t, d, VerdictPark, ReasonAwaitingVerification)
	want := "awaiting verification of action 6407 (until 2026-07-26T02:15:00Z)"
	if !strings.HasPrefix(d.Detail, want) || !strings.Contains(d.Detail, "guc:work_mem") {
		t.Fatalf("detail %q, want prefix %q naming the object", d.Detail, want)
	}
	if d.VerificationWait == nil || len(d.VerificationWait.Pending) != 1 ||
		d.VerificationWait.Pending[0].ActionID != 6407 || d.VerificationWait.Overridden {
		t.Fatalf("wait %+v, want action 6407 pending and not overridden", d.VerificationWait)
	}
	if d.RiskTier != RiskSafe || tracker.calls != 1 {
		t.Fatalf("risk %q, tracker calls %d", d.RiskTier, tracker.calls)
	}
	if tracker.seen[0].SQL != gucRequest("10MB").SQL {
		t.Fatalf("tracker saw %q, want the request", tracker.seen[0].SQL)
	}
}

func TestWaitDetailNamesEveryPendingChange(t *testing.T) {
	held := PendingVerification{DecisionID: 99, Object: "table:public.memories",
		Until: waitNow.Add(10 * time.Minute), HardDeadline: waitNow.Add(10 * time.Minute)}
	index := PendingVerification{ActionID: 6410, Object: "table:public.memories",
		Until: waitNow.Add(time.Hour), HardDeadline: waitNow.Add(73 * time.Hour)}
	detail := WaitDetail([]PendingVerification{index, held})
	for _, want := range []string{
		"awaiting verification of action 6410 (until 2026-07-26T03:00:00Z)",
		"table:public.memories",
		"the change authorized by decision 99 (until 2026-07-26T02:10:00Z)",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q lacks %q", detail, want)
		}
	}
	if WaitDetail(nil) != "" {
		t.Fatal("no pending change must give an empty detail")
	}
}

func TestWaitWithoutPendingDecidesAsWithoutTracker(t *testing.T) {
	plain := newWaitGate(t, gateFixture{}, nil).Authorize(context.Background(),
		gucRequest("9MB"))
	tracked := newWaitGate(t, gateFixture{}, &fakeTracker{}).Authorize(context.Background(),
		gucRequest("9MB"))
	if plain.Verdict != VerdictExecute || tracked.Verdict != plain.Verdict ||
		tracked.Reason != plain.Reason || tracked.Detail != plain.Detail ||
		tracked.VerificationWait != nil {
		t.Fatalf("without tracker %+v, with nothing pending %+v", plain, tracked)
	}
}

func TestWaitNilTrackerNeverParks(t *testing.T) {
	d := newWaitGate(t, gateFixture{}, nil).Authorize(context.Background(), gucRequest("10MB"))
	assertDecision(t, d, VerdictExecute, ReasonAuthorized)
}

func TestWaitExemptsRollbacksAndReverts(t *testing.T) {
	tracker := &fakeTracker{pending: []PendingVerification{gucInFlight(6407)}}
	gate := newWaitGate(t, gateFixture{}, tracker)
	rollback := gucRequest("8MB")
	rollback.Rollback = true // the monitor undoing action 6407 itself
	if d := gate.Authorize(context.Background(), rollback); d.Verdict != VerdictExecute {
		t.Fatalf("rollback of the in-flight action = %+v, want execute", d)
	}
	revert := validIndexRequest()
	revert.Contract = &ActionContract{ActionType: "revert_created_index", RiskTier: RiskSafe}
	revert.SQL = "DROP INDEX CONCURRENTLY public.idx_orders"
	if d := gate.Authorize(context.Background(), revert); d.Verdict != VerdictExecute ||
		d.Reason == ReasonAwaitingVerification {
		t.Fatalf("revert of an index pg_sage created = %+v, want execute", d)
	}
	if tracker.calls != 0 {
		t.Fatalf("tracker consulted %d times for exempt requests", tracker.calls)
	}
}

func waitFreezeRequest(critical bool, kind DeadlineKind) ActionRequest {
	req := ActionRequest{
		Contract: &ActionContract{ActionType: "vacuum_table", RiskTier: RiskSafe,
			RollbackClass: RollbackNoRollbackNeeded},
		SQL: "VACUUM (FREEZE) public.orders", TargetObjs: []string{"public.orders"},
		Feature: "freeze",
	}
	if critical {
		req.Deadline = &DeadlineContext{Kind: kind, Urgency: UrgencyCritical,
			HardAt: waitNow.Add(6 * time.Hour)}
	}
	return req
}

func TestWaitExemptsEmergencyBudgetBypass(t *testing.T) {
	inFlight := PendingVerification{ActionID: 6410, Object: "table:public.orders",
		Until: waitNow.Add(time.Hour), HardDeadline: waitNow.Add(73 * time.Hour)}
	for _, kind := range []DeadlineKind{DeadlineXID, DeadlineDisk} {
		tracker := &fakeTracker{pending: []PendingVerification{inFlight}}
		d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(),
			waitFreezeRequest(true, kind))
		if d.Verdict != VerdictExecute || d.VerificationWait != nil {
			t.Fatalf("critical %s freeze = %+v, want execute without a wait", kind, d)
		}
	}
	tracker := &fakeTracker{pending: []PendingVerification{inFlight}}
	d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(),
		waitFreezeRequest(false, ""))
	assertDecision(t, d, VerdictPark, ReasonAwaitingVerification)
}

func TestWaitExemptsAnExpiredDeadline(t *testing.T) {
	// A critical deadline already past is no emergency the gate trusts.
	req := waitFreezeRequest(true, DeadlineXID)
	req.Deadline.HardAt = waitNow.Add(-time.Minute)
	tracker := &fakeTracker{pending: []PendingVerification{{ActionID: 1,
		Object: "table:public.orders", Until: waitNow.Add(time.Hour),
		HardDeadline: waitNow.Add(2 * time.Hour)}}}
	d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictPark, ReasonAwaitingVerification)
}

func TestWaitExemptsOwnerDeclaredAndReadOnly(t *testing.T) {
	tracker := &fakeTracker{pending: []PendingVerification{gucInFlight(1)}}
	gate := newWaitGate(t, gateFixture{}, tracker)
	owner := validIndexRequest()
	owner.OwnerDeclared = true
	if d := gate.Authorize(context.Background(), owner); d.Reason == ReasonAwaitingVerification {
		t.Fatalf("owner-declared request parked: %+v", d)
	}
	read := validIndexRequest()
	read.Contract = &ActionContract{ActionType: "diagnose_lock_blockers", RiskTier: RiskReadOnly}
	read.SQL = "SELECT 1 /* CONCURRENTLY */"
	if d := gate.Authorize(context.Background(), read); d.Reason == ReasonAwaitingVerification {
		t.Fatalf("read-only diagnostic parked: %+v", d)
	}
	if tracker.calls != 0 {
		t.Fatalf("tracker consulted %d times", tracker.calls)
	}
}

// A request the gate already withholds is not looked up: the wait can only
// restrict an execute (or annotate an approval).
func TestWaitIsNotConsultedForWithheldVerdicts(t *testing.T) {
	tracker := &fakeTracker{pending: []PendingVerification{gucInFlight(1)}}
	gate := newWaitGate(t, gateFixture{runtime: RuntimeState{ExecutorEnabled: true,
		EmergencyStop: true}, runtimeSet: true}, tracker)
	d := gate.Authorize(context.Background(), gucRequest("10MB"))
	assertDecision(t, d, VerdictBlocked, ReasonEmergencyStop)
	if tracker.calls != 0 {
		t.Fatalf("tracker consulted %d times after a hard stop", tracker.calls)
	}
}

// An approval a person decides is still queued; the wait is in its detail
// so the card and the decision log show it.
func TestWaitAnnotatesAQueuedApproval(t *testing.T) {
	tracker := &fakeTracker{pending: []PendingVerification{gucInFlight(6407)}}
	runtime := RuntimeState{ExecutorEnabled: true, TrustLevel: TrustAdvisory,
		ExecutionMode: ExecutionApproval}
	d := newWaitGate(t, gateFixture{runtime: runtime, runtimeSet: true}, tracker).
		Authorize(context.Background(), gucRequest("10MB"))
	assertDecision(t, d, VerdictQueueApproval, ReasonApprovalRequired)
	if !strings.Contains(d.Detail, "awaiting verification of action 6407") ||
		d.VerificationWait == nil || d.VerificationWait.Overridden {
		t.Fatalf("queued approval %+v, want the wait noted", d)
	}
}

func TestWaitOperatorApprovalOverridesAndIsRecorded(t *testing.T) {
	tracker := &fakeTracker{pending: []PendingVerification{gucInFlight(6407)}}
	req := gucRequest("10MB")
	req.OperatorApproved = true
	d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(), req)
	assertDecision(t, d, VerdictExecute, ReasonOperatorApproved)
	if !strings.Contains(d.Detail, "overrides pending verification of action 6407") {
		t.Fatalf("detail %q, want the override named", d.Detail)
	}
	if d.VerificationWait == nil || !d.VerificationWait.Overridden ||
		len(d.VerificationWait.Pending) != 1 {
		t.Fatalf("wait %+v, want an override of action 6407", d.VerificationWait)
	}
	if OverrideDetail(nil) != "" {
		t.Fatal("no pending change must give an empty override detail")
	}
}

func TestWaitHardDeadlineReleasesAndIsRecorded(t *testing.T) {
	expired := gucInFlight(6407)
	expired.Until, expired.HardDeadline = waitNow.Add(-time.Minute), waitNow.Add(-time.Minute)
	tracker := &fakeTracker{pending: []PendingVerification{expired}}
	d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(),
		gucRequest("10MB"))
	assertDecision(t, d, VerdictExecute, ReasonAuthorized)
	if d.VerificationWait == nil || len(d.VerificationWait.Released) != 1 ||
		len(d.VerificationWait.Pending) != 0 || d.VerificationWait.Released[0].ActionID != 6407 {
		t.Fatalf("wait %+v, want action 6407 released", d.VerificationWait)
	}
	if !strings.Contains(d.Detail, "action 6407") || !strings.Contains(d.Detail,
		"hard deadline") {
		t.Fatalf("detail %q, want the release recorded", d.Detail)
	}
}

// The hard deadline is inclusive: at the deadline the wait is over.
func TestWaitHardDeadlineBoundary(t *testing.T) {
	at := gucInFlight(1)
	at.HardDeadline = waitNow
	if !at.Expired(waitNow) || at.Expired(waitNow.Add(-time.Nanosecond)) {
		t.Fatal("a wait expires exactly at its hard deadline")
	}
	none := PendingVerification{ActionID: 2}
	if none.Expired(waitNow) {
		t.Fatal("a wait without a hard deadline must not be treated as expired")
	}
	tracker := &fakeTracker{pending: []PendingVerification{at}}
	d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(),
		gucRequest("10MB"))
	assertDecision(t, d, VerdictExecute, ReasonAuthorized)
	tracker.pending[0].HardDeadline = waitNow.Add(time.Second)
	d = newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(),
		gucRequest("10MB"))
	assertDecision(t, d, VerdictPark, ReasonAwaitingVerification)
}

// One expired and one live verification: the live one still parks, and
// the release of the other is kept with it.
func TestWaitMixedExpiredAndLiveStillParks(t *testing.T) {
	expired := gucInFlight(6400)
	expired.HardDeadline = waitNow.Add(-time.Hour)
	tracker := &fakeTracker{pending: []PendingVerification{expired, gucInFlight(6407)}}
	d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(),
		gucRequest("10MB"))
	assertDecision(t, d, VerdictPark, ReasonAwaitingVerification)
	if strings.Contains(d.Detail, "action 6400 (") ||
		!strings.Contains(d.Detail, "action 6407") {
		t.Fatalf("detail %q must name only the live verification", d.Detail)
	}
	if len(d.VerificationWait.Released) != 1 || len(d.VerificationWait.Pending) != 1 {
		t.Fatalf("wait %+v", d.VerificationWait)
	}
}

func TestWaitFailsClosedWhenInFlightStateIsUnreadable(t *testing.T) {
	tracker := &fakeTracker{err: errors.New("relation sage.action_outcome does not exist")}
	d := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(),
		gucRequest("10MB"))
	assertDecision(t, d, VerdictBlocked, ReasonPolicyUnavailable)
	if !strings.Contains(d.Detail, "in-flight verifications") ||
		!strings.Contains(d.Detail, "sage.action_outcome") {
		t.Fatalf("detail %q", d.Detail)
	}
	// A person's approval is not refused for it; the gap is recorded.
	req := gucRequest("10MB")
	req.OperatorApproved = true
	op := newWaitGate(t, gateFixture{}, tracker).Authorize(context.Background(), req)
	assertDecision(t, op, VerdictExecute, ReasonOperatorApproved)
	if !strings.Contains(op.Detail, "in-flight verifications") {
		t.Fatalf("operator detail %q, want the unread state noted", op.Detail)
	}
}

func TestWaitExplainShowsThePark(t *testing.T) {
	tracker := &fakeTracker{pending: []PendingVerification{gucInFlight(6407)}}
	gate := newWaitGate(t, gateFixture{}, tracker)
	d := gate.(Explainer).Explain(context.Background(), gucRequest("10MB"))
	if d.Verdict != VerdictPark || d.Reason != ReasonAwaitingVerification {
		t.Fatalf("explain %+v, want the park", d)
	}
}

// Concurrent authorizations each consult the tracker; the gate is safe to
// share (state lives in the tracker and the ledger).
func TestWaitConcurrentAuthorizationsAllPark(t *testing.T) {
	tracker := &fakeTracker{pending: []PendingVerification{gucInFlight(6407)}}
	gate := newWaitGate(t, gateFixture{}, tracker)
	var wg sync.WaitGroup
	results := make(chan Decision, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- gate.Authorize(context.Background(), gucRequest("10MB"))
		}()
	}
	wg.Wait()
	close(results)
	for d := range results {
		if d.Verdict != VerdictPark || d.Reason != ReasonAwaitingVerification {
			t.Fatalf("concurrent decision %+v, want parked", d)
		}
	}
	if tracker.calls != 8 {
		t.Fatalf("tracker calls %d, want 8", tracker.calls)
	}
}

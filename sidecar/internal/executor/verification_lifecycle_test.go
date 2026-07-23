package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

func TestVerifiedIndexLifecycleFailsClosedWithoutVerifier(t *testing.T) {
	actions := &fakeVerifiedIndexActions{}
	lifecycle := newVerifiedIndexLifecycle(nil, actions)

	err := lifecycle.Apply(context.Background(), testVerifiedIndexAction())

	assertDeniedBeforeApply(t, err, actions)
}

func TestVerifiedIndexLifecycleFailsClosedWhenAdmissionUnavailable(t *testing.T) {
	verifier := &fakeIndexVerifier{admitErr: errors.New("collector unavailable")}
	actions := &fakeVerifiedIndexActions{}
	lifecycle := newVerifiedIndexLifecycle(verifier, actions)

	err := lifecycle.Apply(context.Background(), testVerifiedIndexAction())

	assertDeniedBeforeApply(t, err, actions)
	if verifier.admitCalls != 1 {
		t.Fatalf("admission calls = %d, want 1", verifier.admitCalls)
	}
}

func TestVerifiedIndexLifecycleRequiresLowLoadBeforeApply(t *testing.T) {
	verifier := &fakeIndexVerifier{
		admission: verify.Admission{OK: false, Reason: "cpu_above_ceiling"},
	}
	actions := &fakeVerifiedIndexActions{}
	lifecycle := newVerifiedIndexLifecycle(verifier, actions)

	err := lifecycle.Apply(context.Background(), testVerifiedIndexAction())

	assertDeniedBeforeApply(t, err, actions)
	if !strings.Contains(err.Error(), "cpu_above_ceiling") {
		t.Fatalf("error %q does not identify the load gate", err)
	}
}

func TestVerifiedIndexLifecycleAdmitsBeforeMutation(t *testing.T) {
	events := &eventLog{}
	verifier := &fakeIndexVerifier{
		admission:    verify.Admission{OK: true},
		watchVerdict: verify.Verdict{Retain: true, Status: "retain"},
		events:       events,
	}
	actions := &fakeVerifiedIndexActions{actionID: 41, events: events}
	lifecycle := newVerifiedIndexLifecycle(verifier, actions)

	err := lifecycle.Apply(context.Background(), testVerifiedIndexAction())

	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertEvents(t, events.values, "admit", "apply", "watch", "retain")
	assertWatchRequest(t, verifier.watchRequest, 41)
}

func TestVerifiedIndexLifecycleFinalizesVerifierVerdict(t *testing.T) {
	tests := []struct {
		name    string
		verdict verify.Verdict
		want    string
	}{
		{name: "retain", verdict: retainVerdict(), want: "retain"},
		{name: "no gain", verdict: revertVerdict("no_gain"), want: "revert"},
		{name: "write impact", verdict: revertVerdict("write_impact"), want: "revert"},
		{name: "invalid", verdict: revertVerdict("invalid_index"), want: "revert"},
		{name: "hard max samples", verdict: revertVerdict("insufficient_samples"), want: "revert"},
		{name: "more samples", verdict: pendingVerdict(), want: "pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testVerifiedIndexVerdict(t, tt.verdict, tt.want)
		})
	}
}

func TestVerifiedIndexLifecycleResumesDurablePendingWatch(t *testing.T) {
	verifier := &fakeIndexVerifier{
		admission:    verify.Admission{OK: true},
		watchVerdict: pendingVerdict(),
	}
	actions := &fakeVerifiedIndexActions{actionID: 73}
	firstProcess := newVerifiedIndexLifecycle(verifier, actions)

	if err := firstProcess.Apply(context.Background(), testVerifiedIndexAction()); err != nil {
		t.Fatalf("initial Apply() error = %v", err)
	}
	assertFinalization(t, actions, "pending")

	verifier.resumeResults = []resumedIndexVerification{{
		ActionID:    73,
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_customer",
		Verdict:     revertVerdict("insufficient_samples"),
	}}
	restartedProcess := newVerifiedIndexLifecycle(verifier, actions)
	if err := restartedProcess.ResumeDue(context.Background()); err != nil {
		t.Fatalf("ResumeDue() error = %v", err)
	}
	assertFinalization(t, actions, "revert")
}

func testVerifiedIndexVerdict(t *testing.T, verdict verify.Verdict, want string) {
	t.Helper()
	verifier := &fakeIndexVerifier{
		admission:    verify.Admission{OK: true},
		watchVerdict: verdict,
	}
	actions := &fakeVerifiedIndexActions{actionID: 52}
	lifecycle := newVerifiedIndexLifecycle(verifier, actions)

	if err := lifecycle.Apply(context.Background(), testVerifiedIndexAction()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertFinalization(t, actions, want)
	if want == "revert" && actions.revertReason != verdict.Reason {
		t.Fatalf("revert reason = %q, want %q", actions.revertReason, verdict.Reason)
	}
}

func testVerifiedIndexAction() verifiedIndexAction {
	return verifiedIndexAction{
		SQL:         "CREATE INDEX CONCURRENTLY idx_orders_customer ON orders(customer_id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_customer",
		WatchID:     "index-action-52",
		Table:       "orders",
		IndexName:   "idx_orders_customer",
		QueryIDs:    []int64{101, 202},
		Criterion: verify.Criterion{
			Kind:       "index_benefit",
			MinGainPct: 10,
			Window:     15 * time.Minute,
			HardMax:    time.Hour,
		},
	}
}

func retainVerdict() verify.Verdict {
	return verify.Verdict{Retain: true, Status: "retain", Reason: "gain_confirmed"}
}

func revertVerdict(reason string) verify.Verdict {
	return verify.Verdict{Revert: true, Status: "revert", Reason: reason}
}

func pendingVerdict() verify.Verdict {
	return verify.Verdict{
		Status:           "extended",
		Reason:           "insufficient_samples",
		NextEvaluationAt: time.Now().Add(15 * time.Minute),
	}
}

func assertDeniedBeforeApply(t *testing.T, err error, actions *fakeVerifiedIndexActions) {
	t.Helper()
	if err == nil {
		t.Fatal("Apply() error = nil, want fail-closed denial")
	}
	if actions.applyCalls != 0 {
		t.Fatalf("Apply mutation calls = %d, want 0", actions.applyCalls)
	}
}

func assertWatchRequest(t *testing.T, request verify.WatchRequest, actionID int64) {
	t.Helper()
	if request.ActionID != actionID || request.ID == "" {
		t.Fatalf("watch identity = (%q, %d), want non-empty ID and action %d",
			request.ID, request.ActionID, actionID)
	}
	if request.IndexName != "idx_orders_customer" || request.Table != "orders" {
		t.Fatalf("watch target = %s.%s, want orders.idx_orders_customer",
			request.Table, request.IndexName)
	}
	if request.ExecutedAt.IsZero() {
		t.Fatal("watch request did not record execution time")
	}
	if len(request.Criterion.TargetIDs) != 2 {
		t.Fatalf("target query IDs = %v, want [101 202]", request.Criterion.TargetIDs)
	}
}

func assertFinalization(t *testing.T, actions *fakeVerifiedIndexActions, want string) {
	t.Helper()
	switch want {
	case "retain":
		if actions.retainCalls != 1 || actions.revertCalls != 0 {
			t.Fatalf("retain/revert calls = %d/%d, want 1/0",
				actions.retainCalls, actions.revertCalls)
		}
	case "revert":
		if actions.retainCalls != 0 || actions.revertCalls != 1 {
			t.Fatalf("retain/revert calls = %d/%d, want 0/1",
				actions.retainCalls, actions.revertCalls)
		}
	case "pending":
		if actions.retainCalls != 0 || actions.revertCalls != 0 {
			t.Fatalf("pending watch finalized with retain/revert calls %d/%d",
				actions.retainCalls, actions.revertCalls)
		}
	}
}

func assertEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

type eventLog struct {
	values []string
}

func (l *eventLog) add(value string) {
	if l != nil {
		l.values = append(l.values, value)
	}
}

type fakeIndexVerifier struct {
	admission     verify.Admission
	admitErr      error
	admitCalls    int
	watchVerdict  verify.Verdict
	watchErr      error
	watchRequest  verify.WatchRequest
	resumeResults []resumedIndexVerification
	resumeErr     error
	events        *eventLog
}

func (f *fakeIndexVerifier) OKToApplyNow(context.Context) (verify.Admission, error) {
	f.events.add("admit")
	f.admitCalls++
	return f.admission, f.admitErr
}

func (f *fakeIndexVerifier) Watch(
	_ context.Context, request verify.WatchRequest,
) (verify.Verdict, error) {
	f.events.add("watch")
	f.watchRequest = request
	return f.watchVerdict, f.watchErr
}

func (f *fakeIndexVerifier) ResumeDue(
	context.Context,
) ([]resumedIndexVerification, error) {
	f.events.add("resume")
	return f.resumeResults, f.resumeErr
}

type fakeVerifiedIndexActions struct {
	actionID     int64
	applyCalls   int
	retainCalls  int
	revertCalls  int
	revertReason string
	events       *eventLog
}

func (f *fakeVerifiedIndexActions) Apply(context.Context, verifiedIndexAction) (int64, error) {
	f.events.add("apply")
	f.applyCalls++
	return f.actionID, nil
}

func (f *fakeVerifiedIndexActions) Retain(
	context.Context, int64, verify.Verdict,
) error {
	f.events.add("retain")
	f.retainCalls++
	return nil
}

func (f *fakeVerifiedIndexActions) Revert(
	_ context.Context, _ int64, _ string, verdict verify.Verdict,
) error {
	f.events.add("revert")
	f.revertCalls++
	f.revertReason = verdict.Reason
	return nil
}

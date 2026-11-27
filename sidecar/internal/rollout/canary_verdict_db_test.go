package rollout

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// executor-04: the canary waits for each target's own verdict instead of
// judging a reversible action while it is still "monitoring"; an engine
// error rolls back what it changed; a verdict still pending at its
// deadline halts without a rollback.

func instanceStatuses(t *testing.T, f *canaryFixture, runID string) map[string]InstanceRecord {
	t.Helper()
	_, instances, err := f.svc.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("read instances: %v", err)
	}
	out := map[string]InstanceRecord{}
	for _, inst := range instances {
		out[inst.InstanceID] = inst
	}
	return out
}

func TestCanaryWaitsForMonitoringTargetsThenCompletes(t *testing.T) {
	f := newCanaryFixture(t, "a", "b")
	f.targets["a"].settleAfter, f.targets["b"].settleAfter = 3, 5
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "complete" || run.AppliedInstances != 2 || run.HaltReason != "" {
		t.Fatalf("run = %+v, want complete", run)
	}
	for name, want := range map[string]int{"a": 4, "b": 6} {
		target := f.targets[name]
		if got := target.reads[target.nextAction]; got != want {
			t.Fatalf("%s verdict read %d times, want %d (polled until terminal)", name, got,
				want)
		}
		if len(target.rolledBack) != 0 {
			t.Fatalf("%s rolled back on a success: %v", name, target.rolledBack)
		}
	}
	got := instanceStatuses(t, f, run.EvidenceID)
	if got["a"].Status != StatusApplied || got["b"].Status != StatusApplied ||
		!strings.Contains(got["b"].Detail, "verification \"success\"") {
		t.Fatalf("instances = %+v", got)
	}
}

func TestCanaryRevertAfterMonitoringHaltsAndRollsBack(t *testing.T) {
	f := newCanaryFixture(t, "a", "b", "c")
	f.targets["b"].revert = true
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "halted" || run.HaltReason != "verification_failed" {
		t.Fatalf("run = %+v, want halted on the revert", run)
	}
	// b's own monitor rolled it back (outcome rolled_back); only a is undone.
	if len(f.targets["a"].rolledBack) != 1 || len(f.targets["b"].rolledBack) != 0 ||
		len(f.targets["c"].executed) != 0 {
		t.Fatalf("rollbacks a=%v b=%v, c executed %v", f.targets["a"].rolledBack,
			f.targets["b"].rolledBack, f.targets["c"].executed)
	}
	// Three polls until terminal, then one read by the halt's rollback.
	if reads := f.targets["b"].reads[f.targets["b"].nextAction]; reads != 4 {
		t.Fatalf("b's verdict read %d times, want 4 (judged only once terminal)", reads)
	}
}

func TestCanaryStillMonitoringAtTheDeadlineIsPending(t *testing.T) {
	f := newCanaryFixture(t, "a", "b", "c")
	b := f.targets["b"]
	b.neverSettle = true
	b.deadline = -3 * time.Minute // the monitor cap plus the 2 min grace has passed
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "halted" || run.HaltReason != "verification_pending" {
		t.Fatalf("run = %+v, want halted as verification_pending", run)
	}
	if len(f.targets["a"].rolledBack) != 0 || len(b.rolledBack) != 0 ||
		len(f.targets["c"].executed) != 0 {
		t.Fatalf("a pending verdict rolled back (a=%v b=%v) or widened (c=%v)",
			f.targets["a"].rolledBack, b.rolledBack, f.targets["c"].executed)
	}
	got := instanceStatuses(t, f, run.EvidenceID)
	if got["a"].Status != StatusApplied || got["b"].Status != StatusVerificationPending ||
		got["b"].ActionLogID == 0 {
		t.Fatalf("instances = %+v", got)
	}
}

func TestCanaryRefusesASourceWithoutAVerification(t *testing.T) {
	f := newCanaryFixture(t, "a")
	src := f.targets["source"]
	a := src.source[42]
	a.Verification = ""
	src.source[44] = a
	req := canaryRequest("a")
	req.ActionLogID = 44
	_, err := f.svc.Run(context.Background(), req)
	if !errors.Is(err, ErrSourceNotVerified) || !strings.Contains(err.Error(),
		`verification ""`) {
		t.Fatalf("err = %v, want ErrSourceNotVerified naming the empty verification", err)
	}
	if len(f.targets["a"].executed) != 0 {
		t.Fatal("an unverified source reached a target")
	}
}

func TestCanaryApplyErrorRollsBackAndFailsTheRun(t *testing.T) {
	f := newCanaryFixture(t, "a", "b", "c")
	f.targets["b"].executeErr = errors.New("policy refused operator action")
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err == nil || !strings.Contains(err.Error(), "policy refused operator action") {
		t.Fatalf("err = %v, want the apply error", err)
	}
	if run.State != "failed" || run.HaltReason != "rollout_error" {
		t.Fatalf("run = %+v, want failed with rollout_error", run)
	}
	if len(f.targets["a"].rolledBack) != 1 || len(f.targets["c"].executed) != 0 {
		t.Fatalf("a rollbacks %v, c executed %v", f.targets["a"].rolledBack,
			f.targets["c"].executed)
	}
	stored, _, gerr := f.svc.Get(context.Background(), run.EvidenceID)
	if gerr != nil || stored.State != "failed" || stored.HaltReason != "rollout_error" {
		t.Fatalf("stored run = %+v (%v)", stored, gerr)
	}
	got := instanceStatuses(t, f, run.EvidenceID)
	if got["a"].Status != StatusRolledBack || got["b"].Status != StatusFailed ||
		!strings.Contains(got["b"].Detail, "policy refused") {
		t.Fatalf("instances = %+v", got)
	}
}

func TestCanaryRollbackFailureAfterAnErrorIsAppliedUnverified(t *testing.T) {
	f := newCanaryFixture(t, "a", "b")
	f.targets["a"].rollbackErr = errors.New("lock_timeout dropping the index")
	f.targets["b"].executeErr = errors.New("database b has no executor")
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b"))
	if err == nil || run.State != "failed" || run.HaltReason != "rollout_error" {
		t.Fatalf("run = %+v (%v), want failed with rollout_error", run, err)
	}
	got := instanceStatuses(t, f, run.EvidenceID)
	if got["a"].Status != StatusAppliedUnverified ||
		!strings.Contains(got["a"].Detail, "lock_timeout dropping the index") {
		t.Fatalf("instance a = %+v, want applied_unverified with the rollback error", got["a"])
	}
}

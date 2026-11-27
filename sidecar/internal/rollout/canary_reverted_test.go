package rollout

import (
	"context"
	"errors"
	"testing"
	"time"
)

// executor-04 review: a revert verdict means the target's own monitor has
// already rolled the change back. The halt's rollback must record that
// instance rolled_back, not ask the target to roll it back a second time
// (a real executor refuses: "action already rolled back") and record a
// false rollback_failed.

func TestCanaryRevertByTheTargetMonitorIsRecordedRolledBack(t *testing.T) {
	f := newCanaryFixture(t, "a", "b", "c")
	b := f.targets["b"]
	b.revert = true
	b.rollbackErr = errors.New("action already rolled back or rollback in progress")
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.State != "halted" || run.HaltReason != "verification_failed" {
		t.Fatalf("run = %+v, want halted on the revert", run)
	}
	if len(b.rolledBack) != 0 {
		t.Fatalf("b was asked to roll back an action its monitor rolled back: %v",
			b.rolledBack)
	}
	if len(f.targets["a"].rolledBack) != 1 || len(f.targets["c"].executed) != 0 {
		t.Fatalf("a rollbacks %v, c executed %v", f.targets["a"].rolledBack,
			f.targets["c"].executed)
	}
	got := instanceStatuses(t, f, run.EvidenceID)
	if got["a"].Status != StatusRolledBack || got["b"].Status != StatusRolledBack {
		t.Fatalf("instances = %+v, want a and b rolled_back", got)
	}
}

// Error propagation: when the verdict cannot be read before the rollback,
// the rollback is still attempted (fail toward undoing the change).
func TestCanaryRollbackStillRunsWhenTheVerdictReadFails(t *testing.T) {
	f := newCanaryFixture(t, "a")
	a := f.targets["a"]
	backend := &canaryBackend{svc: f.svc}
	err := backend.Rollback(context.Background(), Instance{ID: "a"},
		AppliedChange{Handle: "999"}) // no such action: ActionResult errors
	if err != nil || len(a.rolledBack) != 1 || a.rolledBack[0] != 999 {
		t.Fatalf("rollback err %v, rolled back %v; want one rollback of 999", err,
			a.rolledBack)
	}
}

// State transition: a rollback in progress on the target is not a verdict
// yet; the measurement waits for it to end and then judges it failed.
func TestMeasureWaitsThroughARollbackInProgress(t *testing.T) {
	target := &clockTarget{verdicts: [][2]string{{"monitoring", ""}, {"rolling_back", ""},
		{"rolling_back", ""}, {"rolled_back", ""}}, deadline: clockBase.Add(time.Hour)}
	o, waits, err := measureWith(t, target, 15*time.Second, "7")
	if err != nil {
		t.Fatal(err)
	}
	if target.polls != 4 || len(waits) != 3 {
		t.Fatalf("polls = %d, waits = %v; want 4 polls and 3 waits", target.polls, waits)
	}
	if !o.Failed || o.Pending || o.RegressionPct != 100 {
		t.Fatalf("outcome = %+v, want a judged failure once rolled back", o)
	}
}

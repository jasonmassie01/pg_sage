package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// guard-01b: a backfill job's cancel item never takes the manual path. The
// pressure runner rechecks the job, the pressure and the backend before it
// signals; the manual path would run a bare pg_cancel_backend(pid) on a pid
// the server may have reused. With no runner owning the item (none
// registered, or it declines), the executor refuses it.
func TestRunApprovedAction_PressureJobWithoutItsRunnerIsRefused(t *testing.T) {
	item := store.QueuedAction{ID: 12, FindingID: 3, Status: "approved",
		ActionType: "cancel_backend", ProposedSQL: "SELECT pg_cancel_backend(4242)",
		IdentityKey: PressureJobIdentityPrefix + "7:4242:1700000000000000"}
	for name, runner := range map[string]*fakeRunner{
		"no runner": nil, "runner declines": {owns: false},
	} {
		exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
		if runner != nil {
			if err := exec.AddApprovedActionRunner("pressure_jobs",
				[]string{PressureJobIdentityPrefix}, runner); err != nil {
				t.Fatalf("%s: add: %v", name, err)
			}
		}
		_, err := exec.RunApprovedAction(context.Background(), item, 2)
		if !errors.Is(err, ErrPressureJobOutsideRunner) {
			t.Fatalf("%s: run = %v, want ErrPressureJobOutsideRunner", name, err)
		}
		if !strings.Contains(err.Error(), "queue item 12") {
			t.Fatalf("%s: the refusal does not name the item: %v", name, err)
		}
		if runner != nil && runner.calls != 0 {
			t.Fatalf("%s: a declining runner ran %d times", name, runner.calls)
		}
	}
}

// The runner that owns the item still runs it, with the approver.
func TestRunApprovedAction_PressureJobRunsThroughItsRunner(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	runner := &fakeRunner{owns: true}
	if err := exec.AddApprovedActionRunner("pressure_jobs",
		[]string{PressureJobIdentityPrefix}, runner); err != nil {
		t.Fatalf("add: %v", err)
	}
	item := store.QueuedAction{ID: 13, FindingID: 3, Status: "approved",
		ActionType: "cancel_backend", IdentityKey: PressureJobIdentityPrefix + "7:1:1"}
	if _, err := exec.RunApprovedAction(context.Background(), item, 5); err != nil {
		t.Fatalf("run: %v", err)
	}
	if runner.calls != 1 || runner.approver != 5 || runner.action.ID != 13 {
		t.Fatalf("runner calls %d approver %d item %d, want 1, 5, 13", runner.calls,
			runner.approver, runner.action.ID)
	}
}

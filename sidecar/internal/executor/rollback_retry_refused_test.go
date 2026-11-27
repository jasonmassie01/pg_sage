package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// executor-07 review: the Actions page offers Retry rollback on
// rollback_skipped rows, but a revert abandoned because the index is no
// longer the one pg_sage created must not be retried: its DROP would hit
// another object. No concurrent access tests: refusedRevertRetry is pure,
// and the operator claim's CAS is covered by the manual rollback tests.

func TestRefusedRevertRetry(t *testing.T) {
	abandoned := revertRefusedPrefix + "index public.i is no longer the object pg_sage created"
	cases := []struct {
		outcome, reason string
		refused         bool
	}{
		{"rollback_skipped", abandoned, true},
		{"rollback_skipped", "emergency stop active; automatic rollback withheld", false},
		{"rollback_skipped", "", false},
		{"rollback_failed", abandoned, false},
		{"success", abandoned, false},
		{"", "", false},
		{"rollback_skipped", "Revert refused: case differs", false},
	}
	for _, c := range cases {
		err := refusedRevertRetry(c.outcome, c.reason)
		if (err != nil) != c.refused {
			t.Errorf("refusedRevertRetry(%q, %q) = %v; refused want %v",
				c.outcome, c.reason, err, c.refused)
		}
		if err != nil && !strings.Contains(err.Error(), c.reason) {
			t.Errorf("error %q does not name the abandon reason", err)
		}
	}
}

func TestRollbackAction_RefusesAbandonedRevert(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := stuckExecutor(t, pool)
	_, index := stuckIndex(t, ctx, pool)
	id := insertStuckRollback(t, ctx, pool, "create_index",
		"DROP INDEX CONCURRENTLY IF EXISTS "+index, time.Minute)
	reason := revertRefusedPrefix + "index " + index + " is no longer the object pg_sage created"
	setOutcome(t, ctx, exec, id, "rollback_skipped", reason)
	err := exec.RollbackAction(ctx, id, "operator retry")
	if err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("retry of an abandoned revert: err = %v, want a refusal", err)
	}
	if !indexPresentByName(t, ctx, pool, index) {
		t.Fatal("the refused retry dropped the index")
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rollback_skipped" {
		t.Fatalf("outcome = %q, want rollback_skipped untouched", outcome)
	}
	// A rollback withheld by the emergency stop is still retryable.
	setOutcome(t, ctx, exec, id, "rollback_skipped",
		"emergency stop active; automatic rollback withheld")
	if err := exec.RollbackAction(ctx, id, "operator retry"); err != nil {
		t.Fatalf("retry of a withheld rollback: %v", err)
	}
	if indexPresentByName(t, ctx, pool, index) {
		t.Fatal("the withheld rollback's retry did not drop the index")
	}
}

func setOutcome(t *testing.T, ctx context.Context, exec *Executor, id int64,
	outcome, reason string) {
	t.Helper()
	if err := updateActionOutcome(ctx, exec.pool, id, outcome, reason); err != nil {
		t.Fatalf("set outcome %s: %v", outcome, err)
	}
}

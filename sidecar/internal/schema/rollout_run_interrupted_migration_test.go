package schema

import (
	"strings"
	"testing"
)

// executor-04b: a fleet canary run records its targets and can be
// interrupted; its instances take the in-flight statuses applying and
// awaiting_verdict and the interrupted status. The step runs twice
// cleanly, and anything else is still refused.

func TestRolloutRunInterruptedMigrationTwiceAndChecked(t *testing.T) {
	pool, ctx := requireDB(t)
	for i := 0; i < 2; i++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	run := rolloutRunFixture(ctx, t)
	var targets string
	if err := pool.QueryRow(ctx, `SELECT targets::text FROM sage.rollout_run
		WHERE evidence_id = $1`, run).Scan(&targets); err != nil || targets != "[]" {
		t.Fatalf("default targets = %q (%v), want []", targets, err)
	}
	for _, status := range []string{"applying", "awaiting_verdict", "interrupted",
		"verification_pending", "applied", "not_locally_verified"} {
		if err := insertRolloutInstance(ctx, t, run, status); err != nil {
			t.Errorf("instance status %s refused: %v", status, err)
		}
	}
	err := insertRolloutInstance(ctx, t, run, "resumed")
	if err == nil || !strings.Contains(err.Error(), "rollout_instance_status_check") {
		t.Fatalf("an unknown instance status was accepted or refused oddly: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.rollout_run SET state = 'interrupted'
		WHERE evidence_id = $1`, run); err != nil {
		t.Fatalf("state interrupted refused: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE sage.rollout_run SET state = 'resumed'
		WHERE evidence_id = $1`, run)
	if err == nil || !strings.Contains(err.Error(), "rollout_run_state_check") {
		t.Fatalf("an unknown run state was accepted or refused oddly: %v", err)
	}
	var checks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.rollout_run'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%state%'`).Scan(&checks); err != nil ||
		checks != 1 {
		t.Fatalf("run state checks = %d (%v), want exactly 1", checks, err)
	}
}

// State transition: an install from before executor-04b (no targets, the
// narrower checks) is widened by the next bootstrap.
func TestRolloutRunInterruptedMigrationWidensAnOldInstall(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.rollout_instance;
		ALTER TABLE sage.rollout_run DROP CONSTRAINT rollout_run_state_check;
		ALTER TABLE sage.rollout_run ADD CONSTRAINT rollout_run_state_check
		CHECK (state IN ('canary', 'expanding', 'halted', 'complete', 'failed'));
		ALTER TABLE sage.rollout_run DROP COLUMN targets;
		ALTER TABLE sage.rollout_instance DROP CONSTRAINT rollout_instance_status_check;
		ALTER TABLE sage.rollout_instance ADD CONSTRAINT rollout_instance_status_check
		CHECK (status IN ('not_locally_verified', 'applied', 'rolled_back',
		'rollback_failed', 'rollback_unavailable', 'failed', 'verification_pending',
		'applied_unverified'))`); err != nil {
		t.Fatalf("restore the old install: %v", err)
	}
	run := rolloutRunFixture(ctx, t)
	if err := insertRolloutInstance(ctx, t, run, "awaiting_verdict"); err == nil {
		t.Fatal("the old constraint accepted awaiting_verdict")
	}
	bootstrapWithRetry(t, ctx, pool)
	if err := insertRolloutInstance(ctx, t, run, "awaiting_verdict"); err != nil {
		t.Fatalf("awaiting_verdict refused after the migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.rollout_run SET state = 'interrupted',
		targets = '["a"]' WHERE evidence_id = $1`, run); err != nil {
		t.Fatalf("interrupted run with targets refused after the migration: %v", err)
	}
}

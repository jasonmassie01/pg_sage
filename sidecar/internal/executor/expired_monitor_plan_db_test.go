package executor

import (
	"strings"
	"testing"
	"time"
)

// plan-02a: an expired monitor on a plan step's action queues no lab-01c
// revert item and rolls nothing back, whatever verify.on_expired says:
// the step's own on_expired decides (the plan engine reads the action's
// verification_expired outcome). The expiry is still recorded.
func TestExpiredMonitorOnAPlanStepQueuesNoRevertItem(t *testing.T) {
	for _, onExpired := range []string{"ask", "revert", "keep"} {
		t.Run(onExpired, func(t *testing.T) { expiredPlanStep(t, onExpired) })
	}
}

// expiredPlanStep runs one mode: each fixture holds the package's database
// lock until its test ends, so each mode is its own subtest.
func expiredPlanStep(t *testing.T, onExpired string) {
	t.Helper()
	f := newRollbackGateFixture(t)
	f.e.cfg.Verify.OnExpired = onExpired
	id := insertMonitoredAction(t, f.pool, "interrupted", probeRollback,
		regressedBeforeState)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.action_queue (finding_id,
		proposed_sql, action_risk, identity_key, proposed_by, status, action_log_id)
		VALUES (1, 'CREATE INDEX CONCURRENTLY i ON t (a)', 'moderate', 'plan_step:1:1',
		'plan:1', 'approved', $1)`, id); err != nil {
		t.Fatalf("%s: link the plan step: %v", onExpired, err)
	}
	ageAction(t, f.ctx, f.pool, id, 112*24*time.Hour)
	if err := f.e.resumeOrphanedMonitors(f.ctx); err != nil {
		t.Fatalf("%s: resume: %v", onExpired, err)
	}
	f.e.monitors.Wait()
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "verification_expired" {
		t.Fatalf("%s: outcome %q, want verification_expired (not rolled back)",
			onExpired, outcome)
	}
	if items := f.revertItems(t, id); len(items) != 0 {
		t.Fatalf("%s: %d revert items queued for a plan step, want none", onExpired,
			len(items))
	}
	d := expiryDecisions(t, f, id)
	if len(d) != 1 || d[0].verdict != "observe_only" ||
		!strings.Contains(d[0].reason, "plan step") {
		t.Fatalf("%s: decisions %+v, want one observe_only naming the plan step",
			onExpired, d)
	}
}

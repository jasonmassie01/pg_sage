package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// lab-01c review: an expired monitor is never left with nobody asked and
// never offered a revert that cannot run. A REINDEX has no rollback SQL,
// so it is kept and recorded; a verify.on_expired=revert rollback the
// gate refuses (emergency stop) asks a person instead.

func TestExpiredMonitorWithoutRollbackSQLIsKept(t *testing.T) {
	f := newRollbackGateFixture(t)
	var id int64
	err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, outcome, before_state)
		VALUES ('reindex', 'REINDEX INDEX CONCURRENTLY public.rb_probe', NULL,
		        'interrupted', '{}'::jsonb) RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert reindex action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1",
			id)
	})
	ageAction(t, f.ctx, f.pool, id, 112*24*time.Hour)
	if err := f.e.resumeOrphanedMonitors(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	f.e.monitors.Wait()
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "verification_expired" {
		t.Fatalf("outcome %q, want verification_expired", outcome)
	}
	if items := f.revertItems(t, id); len(items) != 0 {
		t.Fatalf("an action with no rollback SQL was offered a revert: %+v", items)
	}
	d := expiryDecisions(t, f, id)
	if len(d) != 1 || d[0].verdict != "observe_only" ||
		!strings.Contains(d[0].reason, "no rollback SQL") {
		t.Fatalf("decisions %+v, want one observe_only decision naming no rollback SQL", d)
	}
}

func TestExpiredMonitorRefusedRevertAsksInstead(t *testing.T) {
	f := newRollbackGateFixture(t)
	f.e.cfg.Verify.OnExpired = "revert"
	f.e.WithEmergencyStopCheck(func(context.Context) bool { return true })
	id := insertMonitoredAction(t, f.pool, "interrupted", probeRollback, regressedBeforeState)
	ageAction(t, f.ctx, f.pool, id, 112*24*time.Hour)
	if err := f.e.resumeOrphanedMonitors(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	f.e.monitors.Wait()
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "verification_expired" {
		t.Fatalf("outcome %q, want verification_expired (the rollback was refused)", outcome)
	}
	items := f.revertItems(t, id)
	if len(items) != 1 || items[0].ProposedSQL != probeRollback ||
		items[0].Status != "pending" {
		t.Fatalf("revert items %+v, want one pending revert of %q", items, probeRollback)
	}
	d := expiryDecisions(t, f, id)
	if len(d) != 1 || d[0].verdict != "queue_approval" ||
		!strings.Contains(d[0].reason, "rollback was refused") ||
		!strings.Contains(d[0].reason, "verify.on_expired=revert") {
		t.Fatalf("decisions %+v, want one queue_approval naming the refused rollback", d)
	}
}

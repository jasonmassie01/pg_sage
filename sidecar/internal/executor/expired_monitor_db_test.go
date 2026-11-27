package executor

import (
	"strings"
	"testing"
	"time"
)

// lab-01c: a monitor whose window ended while pg_sage was not watching
// keeps its verification_expired record, and then verify.on_expired
// decides: ask (default) queues one "Revert action N" item, keep keeps the
// change (recorded), revert rolls it back through the operator rollback.
// Every path writes the decision ledger.

// expiredFixture is an interrupted monitored action whose window ended
// 112 days ago, resumed once with on_expired set.
func expiredFixture(t *testing.T, onExpired string) (rollbackGateFixture, int64) {
	t.Helper()
	f := newRollbackGateFixture(t)
	f.e.cfg.Verify.OnExpired = onExpired
	id := insertMonitoredAction(t, f.pool, "interrupted", probeRollback, regressedBeforeState)
	ageAction(t, f.ctx, f.pool, id, 112*24*time.Hour)
	if err := f.e.resumeOrphanedMonitors(f.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	f.e.monitors.Wait()
	return f, id
}

func expiryDecisions(t *testing.T, f rollbackGateFixture, id int64) []rollbackDecisionRow {
	t.Helper()
	var out []rollbackDecisionRow
	for _, d := range f.decisions(t, id) {
		if d.source == "verification_expired" {
			out = append(out, d)
		}
	}
	return out
}

func TestExpiredMonitorAsksWithOneRevertItem(t *testing.T) {
	f, id := expiredFixture(t, "ask")
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "verification_expired" {
		t.Fatalf("outcome %q, want the verification_expired record kept", outcome)
	}
	items := f.revertItems(t, id)
	if len(items) != 1 || items[0].ActionType != ActionTypeRevertAction ||
		items[0].ProposedSQL != probeRollback || items[0].Status != "pending" {
		t.Fatalf("revert items %+v, want one pending revert of %q", items, probeRollback)
	}
	d := expiryDecisions(t, f, id)
	if len(d) != 1 || d[0].verdict != "queue_approval" ||
		!strings.Contains(d[0].reason, "verify.on_expired=ask") || d[0].queueID == nil ||
		int(*d[0].queueID) != items[0].ID {
		t.Fatalf("decisions %+v, want one queue_approval linked to item %d", d, items[0].ID)
	}
	// Idempotent: a second resume queues nothing more.
	if err := f.e.resumeOrphanedMonitors(f.ctx); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	f.e.monitors.Wait()
	if items := f.revertItems(t, id); len(items) != 1 {
		t.Fatalf("second resume left %d revert items, want 1", len(items))
	}
	// The hygiene sweep must not supersede the item while the action is
	// still expired and unjudged.
	if _, err := f.e.supersedeStaleApprovals(f.ctx); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if items := f.revertItems(t, id); len(items) != 1 {
		t.Fatalf("the sweep superseded the expired action's revert item")
	}
}

// State transition: approving the item runs the rollback once.
func TestExpiredMonitorApprovedRevertRunsTheRollback(t *testing.T) {
	f, id := expiredFixture(t, "ask")
	items := f.revertItems(t, id)
	if len(items) != 1 {
		t.Fatalf("revert items %+v, want one", items)
	}
	if r := f.e.ApprovalReadiness(items[0], time.Now()); !r.Eligible {
		t.Fatalf("the expired action's revert item is not approvable: %+v", r)
	}
	run, err := f.e.RunApprovedAction(f.ctx, items[0], 1)
	if err != nil || run.ActionLogID != id {
		t.Fatalf("approved revert %+v, %v", run, err)
	}
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome %q, want rolled_back", outcome)
	}
	if _, err := f.e.RunApprovedAction(f.ctx, items[0], 1); err == nil {
		t.Fatal("a second approval ran the rollback again")
	}
}

// State transition: rejecting the item records "kept by operator" and
// leaves the change in place.
func TestExpiredMonitorRejectedRevertIsKeptByOperator(t *testing.T) {
	f, id := expiredFixture(t, "ask")
	items := f.revertItems(t, id)
	if len(items) != 1 {
		t.Fatalf("revert items %+v, want one", items)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_queue SET status = 'rejected'
		WHERE id = $1`, items[0].ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	for range 2 { // idempotent
		if err := f.e.settleKeptExpired(f.ctx); err != nil {
			t.Fatalf("settleKeptExpired: %v", err)
		}
	}
	outcome, _ := actionOutcomeFor(t, f.pool, id)
	reason := actionReason(t, f.ctx, f.pool, id)
	if outcome != "verification_expired" || strings.Count(reason, "kept by operator") != 1 {
		t.Fatalf("outcome %q reason %q, want verification_expired kept by operator once",
			outcome, reason)
	}
	d := expiryDecisions(t, f, id)
	if len(d) != 2 || d[1].verdict != "observe_only" ||
		!strings.Contains(d[1].reason, "kept by operator") {
		t.Fatalf("decisions %+v, want the ask then one kept-by-operator decision", d)
	}
}

func TestExpiredMonitorKeepIsRecordedWithoutAnItem(t *testing.T) {
	f, id := expiredFixture(t, "keep")
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "verification_expired" {
		t.Fatalf("outcome %q, want verification_expired", outcome)
	}
	if items := f.revertItems(t, id); len(items) != 0 {
		t.Fatalf("keep queued %+v", items)
	}
	d := expiryDecisions(t, f, id)
	if len(d) != 1 || d[0].verdict != "observe_only" ||
		!strings.Contains(d[0].reason, "verify.on_expired=keep") {
		t.Fatalf("decisions %+v, want one observe_only keep decision", d)
	}
}

func TestExpiredMonitorRevertRollsBack(t *testing.T) {
	f, id := expiredFixture(t, "revert")
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome %q, want rolled_back through the operator rollback", outcome)
	}
	if r := actionReason(t, f.ctx, f.pool, id); !strings.Contains(r,
		"verify.on_expired=revert") {
		t.Fatalf("reason %q, want it to name verify.on_expired=revert", r)
	}
	if items := f.revertItems(t, id); len(items) != 0 {
		t.Fatalf("revert queued %+v", items)
	}
	d := expiryDecisions(t, f, id)
	if len(d) != 1 || d[0].verdict != "execute" {
		t.Fatalf("decisions %+v, want one execute decision for the expiry", d)
	}
}

package executor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// policy-05 review: pg_sage's own (finding) revert is decided on the same
// typed rollback path as an approved one. Its verdict must carry the
// gate's lock ceiling and decision, and its decision must name the
// reverted action, or the ceiling does not bind and CHECK-01 cannot join
// the decision to the action.

// selfMonitor runs the finding monitor of actionID (pg_sage's own change)
// with a spy in place of the DDL.
func (f rollbackGateFixture) selfMonitor(
	actionID int64, forwardSQL, rollbackSQL string,
) *recordedRollback {
	rec := &recordedRollback{}
	finding := analyzer.Finding{Category: "table_bloat", ObjectIdentifier: "public.t",
		RecommendedSQL: forwardSQL, RollbackSQL: rollbackSQL}
	cfg := f.e.findingMonitorConfig(finding, actionID)
	cfg.execRollback, cfg.ThresholdPct, cfg.WindowMinutes = rec.exec, 10, 0
	MonitorAndRollback(f.ctx, f.pool, actionID, rollbackSQL, cfg, nopLog, nil)
	return rec
}

// Boundary: the lock ceiling caps pg_sage's own in-transaction revert, and
// the execute decision names the reverted action.
func TestSelfRevertTakesTheLockCeilingAndNamesTheAction(t *testing.T) {
	f := newRollbackGateFixture(t)
	const forward = "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.05)"
	const revert = "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.2)"
	id := insertMonitoredAction(t, f.pool, "monitoring", revert, regressedBeforeState)

	rec := f.selfMonitor(id, forward, revert)

	got := f.decisions(t, id)
	if rec.calls != 1 || rec.lock[0] != 700 {
		t.Fatalf("rollback %#v lock %v, want one at the 700 ms ceiling (decisions %+v)",
			rec.sql, rec.lock, got)
	}
	if len(got) != 1 || got[0].verdict != "execute" ||
		got[0].gateReason != string(policy.ReasonRollbackMitigation) ||
		got[0].source != "automatic_rollback" {
		t.Fatalf("decisions = %+v, want one automatic rollback_mitigation execute "+
			"naming action %d", got, id)
	}
}

// State transitions: pg_sage's own revert withheld by a disabled executor
// queues one revert item, and the blocked decision is linked to it.
func TestSelfRevertWithheldQueuesALinkedItem(t *testing.T) {
	f := newRollbackGateFixture(t)
	f.e.SetExecutorEnabled(false)
	const forward = "CREATE INDEX CONCURRENTLY rb_probe ON public.t (a)"
	id := insertMonitoredAction(t, f.pool, "monitoring", probeRollback, regressedBeforeState)

	rec := f.selfMonitor(id, forward, probeRollback)

	if rec.calls != 0 {
		t.Fatalf("a disabled executor ran the revert: %#v", rec.sql)
	}
	items := f.revertItems(t, id)
	if len(items) != 1 || items[0].ProposedSQL != probeRollback {
		t.Fatalf("revert items = %+v, want one pending revert of %q", items, probeRollback)
	}
	got := f.decisions(t, id)
	if len(got) != 1 || got[0].verdict != "blocked" || got[0].reason != "executor_disabled" ||
		got[0].queueID == nil || int(*got[0].queueID) != items[0].ID {
		t.Fatalf("decisions = %+v, want one blocked decision linked to item %d", got,
			items[0].ID)
	}
}

// State transitions: a newer pending proposal with the same SQL (the
// analyzer proposing the very drop a revert item would run) must not
// supersede the revert item, or the withheld revert dead-ends again.
func TestRevertItemOutlivesASameSQLProposal(t *testing.T) {
	f := newRollbackGateFixture(t)
	f.e.SetExecutorEnabled(false)
	id := insertMonitoredAction(t, f.pool, "monitoring", probeRollback, regressedBeforeState)
	f.monitor(id, probeRollback)
	items := f.revertItems(t, id)
	if len(items) != 1 {
		t.Fatalf("revert items = %+v, want one", items)
	}
	var findingID int
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql, status)
		VALUES ('unused_index', 'warning', 'index', 'public.rb_probe', 'unused',
		'{}'::jsonb, $1, 'open') RETURNING id`, probeRollback).Scan(&findingID); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	other, err := store.NewActionStore(f.pool).Propose(f.ctx, nil, findingID,
		" "+probeRollback+" ", "", "safe")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := f.e.supersedeStaleApprovals(f.ctx); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	var revertStatus, otherStatus, reason string
	if err := f.pool.QueryRow(f.ctx, `SELECT r.status, COALESCE(r.reason, ''), o.status
		FROM sage.action_queue r, sage.action_queue o WHERE r.id = $1 AND o.id = $2`,
		items[0].ID, other).Scan(&revertStatus, &reason, &otherStatus); err != nil {
		t.Fatal(err)
	}
	if revertStatus != "pending" || otherStatus != "pending" {
		t.Fatalf("revert item %s (%s), proposal %s: want both pending", revertStatus,
			reason, otherStatus)
	}
	if !strings.HasPrefix(items[0].ProposedSQL, "DROP INDEX") {
		t.Fatalf("revert item SQL %q", items[0].ProposedSQL)
	}
}

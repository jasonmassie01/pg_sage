package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// policy-05 on real PostgreSQL: every revert of an approved, manual or
// resumed action is a recorded rollback decision of the standing gate,
// windows do not bind it, and a withheld revert is offered to a person.

// rollbackAt is Wednesday 14:00 UTC, outside the staffed window.
var rollbackAt = time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)

type rollbackGateFixture struct {
	e    *Executor
	pool *pgxpool.Pool
	ctx  context.Context
}

// staffedRollbackPolicy is the staffed profile (weekdays 01:00-05:00) with
// a 700 ms lock ceiling.
func staffedRollbackPolicy() policy.Document {
	doc := policy.StaffedProfile()
	doc.LockDurationCeilingMS = 700
	return doc
}

func newRollbackGateFixture(t *testing.T) rollbackGateFixture {
	t.Helper()
	pool, ctx := isolatedSageDB(t)
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Safety.LockTimeoutMs = 1500
	e := New(pool, cfg, time.Now().Add(-90*24*time.Hour), noopExecLog)
	e.EnableStandingPolicyDocument(staffedRollbackPolicy(), func() time.Time {
		return rollbackAt
	})
	e.WithActionStore(store.NewActionStore(pool), "approval")
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.Shutdown(sctx)
	})
	return rollbackGateFixture{e: e, pool: pool, ctx: ctx}
}

type rollbackDecisionRow struct {
	verdict, reason, gateReason, source string
	queueID                             *int64
}

// decisions are the rollback decisions recorded for action actionID.
func (f rollbackGateFixture) decisions(t *testing.T, actionID int64) []rollbackDecisionRow {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT verdict, reason,
		COALESCE(evidence->>'gate_reason', ''), COALESCE(evidence->>'source', ''), queue_id
		FROM sage.decision WHERE evidence->>'rollback' = 'true'
		  AND (evidence->>'rolled_back_action_id')::bigint = $1 ORDER BY id`, actionID)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	defer rows.Close()
	var out []rollbackDecisionRow
	for rows.Next() {
		var d rollbackDecisionRow
		if err := rows.Scan(&d.verdict, &d.reason, &d.gateReason, &d.source,
			&d.queueID); err != nil {
			t.Fatalf("scan decision: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// monitor runs the approved/manual action monitor of actionID with a spy
// in place of the DDL.
func (f rollbackGateFixture) monitor(actionID int64, rollbackSQL string) *recordedRollback {
	rec := &recordedRollback{}
	cfg := f.e.actionRollbackConfig(actionID, "CREATE INDEX CONCURRENTLY rb_probe ON public.t (a)")
	cfg.execRollback, cfg.ThresholdPct, cfg.WindowMinutes = rec.exec, 10, 0
	MonitorAndRollback(f.ctx, f.pool, actionID, rollbackSQL, cfg, nopLog, nil)
	return rec
}

func (f rollbackGateFixture) revertItems(t *testing.T, actionID int64) []store.QueuedAction {
	t.Helper()
	pending, err := store.NewActionStore(f.pool).ListPending(f.ctx, nil)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	var out []store.QueuedAction
	for _, item := range pending {
		if item.RevertsActionID != nil && *item.RevertsActionID == actionID {
			out = append(out, item)
		}
	}
	return out
}

const probeRollback = "DROP INDEX CONCURRENTLY IF EXISTS public.rb_probe"

// State transitions: an approved action regresses outside every window;
// the revert runs, with one execute decision carrying rollback=true.
func TestApprovedActionRegressionIsRevertedWithOneDecision(t *testing.T) {
	f := newRollbackGateFixture(t)
	id := insertMonitoredAction(t, f.pool, "monitoring", probeRollback, regressedBeforeState)

	rec := f.monitor(id, probeRollback)

	if rec.calls != 1 || rec.sql[0] != probeRollback || rec.lock[0] != 1500 {
		t.Fatalf("rollback = %#v lock %v, want one concurrent revert at 1500 ms", rec.sql,
			rec.lock)
	}
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
	got := f.decisions(t, id)
	if len(got) != 1 || got[0].verdict != "execute" ||
		got[0].gateReason != string(policy.ReasonRollbackMitigation) ||
		got[0].source != "automatic_rollback" {
		t.Fatalf("decisions = %+v, want one automatic rollback_mitigation execute", got)
	}
	if items := f.revertItems(t, id); len(items) != 0 {
		t.Fatalf("an executed revert queued %d revert items", len(items))
	}
}

// Boundary: the policy's lock ceiling caps an in-transaction revert.
func TestRevertInTransactionTakesTheLockCeiling(t *testing.T) {
	f := newRollbackGateFixture(t)
	const reset = "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.2)"
	id := insertMonitoredAction(t, f.pool, "monitoring", reset, regressedBeforeState)

	rec := f.monitor(id, reset)

	if rec.calls != 1 || rec.lock[0] != 700 {
		t.Fatalf("rollback %#v lock %v, want one at the 700 ms ceiling (decisions %+v)",
			rec.sql, rec.lock, f.decisions(t, id))
	}
}

// State transitions: with the executor disabled the revert is withheld as
// rollback_skipped, recorded, and offered to a person as one revert item.
func TestDisabledExecutorRevertIsSkippedAndQueued(t *testing.T) {
	f := newRollbackGateFixture(t)
	f.e.SetExecutorEnabled(false)
	id := insertMonitoredAction(t, f.pool, "monitoring", probeRollback, regressedBeforeState)

	rec := f.monitor(id, probeRollback)

	if rec.calls != 0 {
		t.Fatalf("a disabled executor ran the revert: %#v", rec.sql)
	}
	var outcome, reason string
	if err := f.pool.QueryRow(f.ctx, `SELECT outcome, COALESCE(rollback_reason, '')
		FROM sage.action_log WHERE id = $1`, id).Scan(&outcome, &reason); err != nil {
		t.Fatal(err)
	}
	if outcome != "rollback_skipped" || !strings.Contains(reason, "executor_disabled") {
		t.Fatalf("outcome %q (%s), want rollback_skipped for executor_disabled", outcome,
			reason)
	}
	items := f.revertItems(t, id)
	if len(items) != 1 || items[0].ActionType != ActionTypeRevertAction ||
		items[0].ProposedSQL != probeRollback || items[0].Status != "pending" {
		t.Fatalf("revert items = %+v, want one pending revert of %q", items, probeRollback)
	}
	got := f.decisions(t, id)
	if len(got) != 1 || got[0].verdict != "blocked" || got[0].reason != "executor_disabled" ||
		got[0].queueID == nil || int(*got[0].queueID) != items[0].ID {
		t.Fatalf("decisions = %+v, want one blocked decision linked to item %d", got,
			items[0].ID)
	}
}

// State transitions: the revert item is not approvable while the executor
// is disabled; once enabled, approving it runs the revert once.
func TestApprovedRevertItemRunsTheOperatorRollback(t *testing.T) {
	f := newRollbackGateFixture(t)
	f.e.SetExecutorEnabled(false)
	id := insertMonitoredAction(t, f.pool, "monitoring", probeRollback, regressedBeforeState)
	f.monitor(id, probeRollback)
	items := f.revertItems(t, id)
	if len(items) != 1 {
		t.Fatalf("revert items = %+v, want one", items)
	}
	item := items[0]
	if r := f.e.ApprovalReadiness(item, time.Now()); r.Eligible {
		t.Fatalf("revert item approvable while the executor is disabled: %+v", r)
	}
	f.e.SetExecutorEnabled(true)
	if r := f.e.ApprovalReadiness(item, time.Now()); !r.Eligible {
		t.Fatalf("revert item not approvable once enabled: %+v", r)
	}
	run, err := f.e.RunApprovedAction(f.ctx, item, 1)
	if err != nil || run.ActionLogID != id {
		t.Fatalf("approved revert = %+v, %v", run, err)
	}
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
	if _, err := f.e.RunApprovedAction(f.ctx, item, 1); err == nil ||
		!strings.Contains(err.Error(), "no longer withheld") {
		t.Fatalf("second approval = %v, want refused as no longer withheld", err)
	}
	got := f.decisions(t, id)
	if len(got) != 2 || got[1].verdict != "execute" || got[1].source != "operator_rollback" {
		t.Fatalf("decisions = %+v, want the blocked one then an operator execute", got)
	}
}

// Invalid input: an item claiming to revert an action that is gone, or a
// revert item whose SQL differs from the recorded rollback, is refused.
func TestRevertItemRefusesAMissingActionOrOtherSQL(t *testing.T) {
	f := newRollbackGateFixture(t)
	gone := int64(987654321)
	missing := store.QueuedAction{ActionType: ActionTypeRevertAction, RevertsActionID: &gone,
		ProposedSQL: probeRollback}
	if _, err := f.e.RunApprovedAction(f.ctx, missing, 1); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("revert of a missing action = %v", err)
	}
	unlinked := store.QueuedAction{ActionType: ActionTypeRevertAction}
	if r := f.e.ApprovalReadiness(unlinked, time.Now()); r.Eligible ||
		!strings.Contains(r.DeferReason, "gone") {
		t.Fatalf("unlinked revert item readiness = %+v", r)
	}
	id := insertMonitoredAction(t, f.pool, "rollback_skipped", probeRollback, "{}")
	other := store.QueuedAction{ActionType: ActionTypeRevertAction, RevertsActionID: &id,
		ProposedSQL: "DROP INDEX CONCURRENTLY IF EXISTS public.someone_elses"}
	if _, err := f.e.RunApprovedAction(f.ctx, other, 1); err == nil ||
		!strings.Contains(err.Error(), rollbackSQLMismatch) {
		t.Fatalf("revert item with other SQL = %v, want %s", err, rollbackSQLMismatch)
	}
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "rollback_skipped" {
		t.Fatalf("outcome = %q, want rollback_skipped unchanged", outcome)
	}
}

// Integration (CHECK-03): a person's revert at 14:00, outside the
// 01:00-05:00 window, drops the real index, writes one decision, and the
// 24-hour audit finds nothing; the forward change would be withheld.
func TestOperatorRollbackOutsideWindowRecordsDecision(t *testing.T) {
	f := newRollbackGateFixture(t)
	table := fmt.Sprintf("rbg_%d", time.Now().UnixNano())
	create := "CREATE INDEX CONCURRENTLY " + table + "_a ON public." + table + " (a)"
	mustExec(t, f.pool, f.ctx, "CREATE TABLE public."+table+" (a int)")
	mustExec(t, f.pool, f.ctx, strings.Replace(create, " CONCURRENTLY", "", 1))
	if d := f.e.explainOperatorAction(f.ctx, create); d.Decision == PolicyDecisionExecute {
		t.Fatalf("the forward create ran outside the window: %+v", d)
	}
	id := f.insertExecutedAction(t, create, "DROP INDEX CONCURRENTLY IF EXISTS public."+
		table+"_a")

	if err := f.e.RollbackAction(f.ctx, id, "operator at 14:00"); err != nil {
		t.Fatalf("operator rollback outside the window: %v", err)
	}

	var exists, hasDecision bool
	if err := f.pool.QueryRow(f.ctx, `SELECT to_regclass($1) IS NOT NULL,
		(SELECT decision_id IS NOT NULL FROM sage.action_log WHERE id = $2)`,
		"public."+table+"_a", id).Scan(&exists, &hasDecision); err != nil {
		t.Fatal(err)
	}
	if exists || !hasDecision {
		t.Fatalf("index exists=%v, action decision_id set=%v", exists, hasDecision)
	}
	got := f.decisions(t, id)
	if len(got) != 1 || got[0].verdict != "execute" ||
		got[0].reason != string(policy.ReasonRollbackMitigation) ||
		got[0].source != "operator_rollback" {
		t.Fatalf("decisions = %+v, want one operator rollback_mitigation execute", got)
	}
	violations, err := ledger.NewPostgresRepository(f.pool).FindAuditViolations(f.ctx)
	if err != nil || len(violations) != 0 {
		t.Fatalf("audit violations = %+v (%v), want none", violations, err)
	}
}

// insertExecutedAction records an executed, verified action backed by an
// execute decision, as the forward path leaves it.
func (f rollbackGateFixture) insertExecutedAction(t *testing.T, sql, rollbackSQL string) int64 {
	t.Helper()
	var decisionID, id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.decision (feature, intent,
		evidence, evidence_id, verdict, reason, risk_tier, policy_version)
		VALUES ('index', 'operator', '{}'::jsonb, $1, 'execute', 'operator_approved',
		        'moderate', 1) RETURNING id`, ledger.NewEvidenceID()).
		Scan(&decisionID); err != nil {
		t.Fatalf("insert forward decision: %v", err)
	}
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log (action_type,
		sql_executed, rollback_sql, outcome, decision_id)
		VALUES ('create_index', $1, $2, 'success', $3) RETURNING id`, sql, rollbackSQL,
		decisionID).Scan(&id); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	return id
}

// Error propagation: a person's revert while the executor is disabled is
// refused by the recorded gate decision; nothing is claimed or run.
func TestOperatorRollbackRefusedWhenExecutorDisabled(t *testing.T) {
	f := newRollbackGateFixture(t)
	id := f.insertExecutedAction(t, "CREATE INDEX CONCURRENTLY rbd ON public.t (a)",
		"DROP INDEX CONCURRENTLY IF EXISTS public.rbd")
	f.e.SetExecutorEnabled(false)

	err := f.e.RollbackAction(f.ctx, id, "operator")

	if err == nil || !strings.Contains(err.Error(), "executor disabled") {
		t.Fatalf("rollback with the executor disabled = %v", err)
	}
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "success" {
		t.Fatalf("outcome = %q, want success (nothing claimed)", outcome)
	}
	got := f.decisions(t, id)
	if len(got) != 1 || got[0].verdict != "blocked" || got[0].reason != "executor_disabled" {
		t.Fatalf("decisions = %+v, want one blocked executor_disabled", got)
	}
}

// Concurrency: the monitor and a person's revert race on one action. The
// claimRollback compare-and-set lets one revert run; every authorization
// that happened is recorded.
func TestMonitorAndOperatorRollbackRaceRunOnce(t *testing.T) {
	f := newRollbackGateFixture(t)
	id := insertMonitoredAction(t, f.pool, "monitoring", probeRollback, regressedBeforeState)
	var wg sync.WaitGroup
	var spy *recordedRollback
	var operatorErr error
	wg.Add(2)
	go func() { defer wg.Done(); spy = f.monitor(id, probeRollback) }()
	go func() { defer wg.Done(); operatorErr = f.e.RollbackAction(f.ctx, id, "operator") }()
	wg.Wait()

	ran := spy.calls
	if operatorErr == nil {
		ran++
	}
	if ran != 1 {
		t.Fatalf("reverts run = %d (monitor %d, operator err %v), want exactly one",
			ran, spy.calls, operatorErr)
	}
	if outcome, _ := actionOutcomeFor(t, f.pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
	executes := 0
	for _, d := range f.decisions(t, id) {
		if d.verdict == "execute" {
			executes++
		}
	}
	if executes < 1 || executes > 2 {
		t.Fatalf("execute decisions = %d, want one per authorization (1 or 2)", executes)
	}
}

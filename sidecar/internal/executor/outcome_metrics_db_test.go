package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// observability-04 on real PostgreSQL: the outcome writes count once
// they commit, a failed write counts nothing, and the gauges read the
// action ledger.

// ownMetricsPool is a pool of its own, named db by an executor, so no
// other test's writes count under db.
func ownMetricsPool(t *testing.T, db string) (*pgxpool.Pool, *Executor, context.Context) {
	t.Helper()
	shared, ctx := requireDB(t)
	_ = shared // bootstraps the schema once
	pool, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("own pool: %v", err)
	}
	t.Cleanup(pool.Close)
	exec := stuckExecutor(t, pool)
	exec.WithDatabaseName(db)
	return pool, exec, ctx
}

// insertDecidedIndexAction records an executed create_index action with
// its gate decision, so its verification can be finalized.
func insertDecidedIndexAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, outcome, rollbackSQL string,
) int64 {
	t.Helper()
	var decisionID, id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent,
		evidence, evidence_id, verdict, reason, risk_tier, policy_version)
		VALUES ('index', 'obs04', '{}'::jsonb, $1, 'execute', 'fixture', 'safe', 1)
		RETURNING id`, ledger.NewEvidenceID()).Scan(&decisionID); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type,
		sql_executed, rollback_sql, outcome, decision_id)
		VALUES ('create_index', 'CREATE INDEX CONCURRENTLY obs04 ON public.t (a)', $1, $2,
		        $3) RETURNING id`, rollbackSQL, outcome, decisionID).Scan(&id); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.verification WHERE action_log_id=$1", id)
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_outcome WHERE action_log_id=$1", id)
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	return id
}

// Happy path: an index build verified as an improvement counts success
// (verdict success); an operator then rolls it back: rolled_back.
func TestOutcomeMetricsDB_SuccessThenRollback(t *testing.T) {
	db := testDatabase(t)
	pool, exec, ctx := ownMetricsPool(t, db)
	_, index := stuckIndex(t, ctx, pool)
	id := insertDecidedIndexAction(t, ctx, pool, "monitoring",
		"DROP INDEX CONCURRENTLY IF EXISTS "+index)
	if outcomeCount(db, "index", "success") != 0 {
		t.Fatal("a monitoring action was counted before its verdict")
	}
	if !settleOutcome(ctx, pool, verify.Outcome{ActionLogID: id,
		Verdict: verify.OutcomeImproved, Reason: "faster",
		Evidence: map[string]any{}}, nopLog) {
		t.Fatal("settle did not record the verdict")
	}
	if got := outcomeCount(db, "index", "success"); got != 1 {
		t.Fatalf("index success = %d, want 1 (%v)", got, seriesOf(db))
	}
	if got := verdictCount(db, "index", "success"); got != 1 {
		t.Fatalf("index verdict success = %d, want 1", got)
	}
	if err := exec.RollbackAction(ctx, id, "operator"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
	if got := outcomeCount(db, "index", "rolled_back"); got != 1 {
		t.Fatalf("index rolled_back = %d, want 1 (%v)", got, seriesOf(db))
	}
	if got := outcomeCount(db, "index", "success"); got != 1 {
		t.Fatalf("index success = %d after the rollback, want still 1", got)
	}
}

// CHECK-22: a regression rolled back at verification counts rolled_back
// with verdict revert, and nothing else.
func TestOutcomeMetricsDB_RegressionRolledBackCountsRevert(t *testing.T) {
	db := testDatabase(t)
	pool, _, ctx := ownMetricsPool(t, db)
	rollbackSQL := "DROP INDEX CONCURRENTLY IF EXISTS public.obs04_missing"
	id := insertDecidedIndexAction(t, ctx, pool, "monitoring", rollbackSQL)
	rec := &recordedRollback{}
	rollbackRegressedAction(ctx, pool, id, rollbackSQL, monitorConfig(rec), nopLog)
	if outcome, _ := actionOutcomeFor(t, pool, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
	if got := outcomeCount(db, "index", "rolled_back"); got != 1 {
		t.Fatalf("index rolled_back = %d, want 1 (%v)", got, seriesOf(db))
	}
	if got := verdictCount(db, "index", "revert"); got != 1 {
		t.Fatalf("index verdict revert = %d, want 1 (%v)", got, VerdictCounts())
	}
	if got := len(seriesOf(db)); got != 1 {
		t.Fatalf("series = %v, want only rolled_back", seriesOf(db))
	}
}

// A compare-and-set that loses (the row is no longer monitorable) counts
// nothing: only the write that changed the row counts.
func TestOutcomeMetricsDB_LostCompareAndSetCountsNothing(t *testing.T) {
	db := testDatabase(t)
	pool, _, ctx := ownMetricsPool(t, db)
	id := insertDecidedIndexAction(t, ctx, pool, "rolled_back", "")
	if setMonitoredOutcome(ctx, pool, id, "unverifiable", "late verdict") {
		t.Fatal("a settled action was overwritten")
	}
	if got := seriesOf(db); len(got) != 0 {
		t.Fatalf("a lost compare-and-set counted %v", got)
	}
}

// A terminal write that changed no row (the action is gone) counts
// nothing: only an outcome that is in the ledger is exported.
func TestOutcomeMetricsDB_WriteToAMissingActionCountsNothing(t *testing.T) {
	db := testDatabase(t)
	pool, _, ctx := ownMetricsPool(t, db)
	var missing int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) + 1000000
		FROM sage.action_log`).Scan(&missing); err != nil {
		t.Fatalf("pick a missing id: %v", err)
	}
	if err := recordActionOutcome(ctx, pool, nopLog, missing, "failed", "gone"); err != nil {
		t.Fatalf("an update of no row is not an error: %v", err)
	}
	if got := seriesOf(db); len(got) != 0 {
		t.Fatalf("a write that changed no row counted %v", got)
	}
	id := insertDecidedIndexAction(t, ctx, pool, "rolling_back", "")
	if err := recordActionOutcome(ctx, pool, nopLog, id, "rollback_failed", "x"); err != nil {
		t.Fatalf("record rollback_failed: %v", err)
	}
	if got := outcomeCount(db, "index", "failed"); got != 1 {
		t.Fatalf("index failed = %d, want 1 (%v)", got, seriesOf(db))
	}
}

// Error propagation: a failed action_log write records no outcome.
func TestOutcomeMetrics_FailedWriteCountsNothing(t *testing.T) {
	db := testDatabase(t)
	pool := closedPool(t)
	exec := New(pool, resumeExecutorConfig(), time.Time{}, nopLog)
	exec.WithDatabaseName(db)
	ctx := context.Background()
	if err := recordActionOutcome(ctx, pool, nopLog, 7, "failed", "x"); err == nil {
		t.Fatal("a write on a closed pool succeeded")
	}
	if setMonitoredOutcome(ctx, pool, 7, "unverifiable", "x") {
		t.Fatal("a compare-and-set on a closed pool succeeded")
	}
	id := exec.logManualActionWithDecision(ctx, 0, "CREATE INDEX i ON t (a)", "", nil,
		errors.New("boom"), nil, 0, nil)
	if id != 0 {
		t.Fatalf("logged action %d on a closed pool", id)
	}
	if got := seriesOf(db); len(got) != 0 {
		t.Fatalf("failed writes counted %v", got)
	}
}

// Boundary: a pending approval counts once its cooldown has passed and
// until it expires; decided, cooling and expired items do not.
func TestReadActionGaugesDB_PendingApprovalBoundaries(t *testing.T) {
	ctx, tx := snapshotTx(t)
	base, err := ReadActionGauges(ctx, tx)
	if err != nil {
		t.Fatalf("read baseline gauges: %v", err)
	}
	for _, row := range []struct {
		status, risk, cooldown, expires string
	}{
		{"pending", "high", "NULL", "now() + interval '1 day'"},
		{"pending", "high", "now() - interval '1 second'", "now() + interval '1 day'"},
		{"pending", "high", "now() + interval '1 hour'", "now() + interval '1 day'"},
		{"pending", "safe", "NULL", "now() - interval '1 second'"},
		{"pending", "moderate", "NULL", "NULL"},
		{"pending", "Mystery", "NULL", "NULL"},
		{"approved", "high", "NULL", "now() + interval '1 day'"},
		{"rejected", "safe", "NULL", "NULL"},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO sage.action_queue
			(proposed_sql, action_risk, status, cooldown_until, expires_at)
			VALUES ('SELECT 1', $1, $2, `+row.cooldown+`, `+row.expires+`)`,
			row.risk, row.status); err != nil {
			t.Fatalf("insert queue row: %v", err)
		}
	}
	g, err := ReadActionGauges(ctx, tx)
	if err != nil {
		t.Fatalf("read gauges: %v", err)
	}
	want := map[string]int64{"high": 2, "moderate": 1, "other": 1, "safe": 0}
	for risk, n := range want {
		if got := g.PendingByRisk[risk] - base.PendingByRisk[risk]; got != n {
			t.Fatalf("pending %s grew by %d, want %d (%v)", risk, got, n, g.PendingByRisk)
		}
	}
	for risk := range g.PendingByRisk {
		if _, known := want[risk]; !known {
			t.Fatalf("risk label %q is outside the bounded set", risk)
		}
	}
}

// The last failure is the latest failed, partial or rollback_failed
// action, by when its outcome was recorded.
func TestReadActionGaugesDB_LastFailure(t *testing.T) {
	ctx, tx := snapshotTx(t)
	latest := time.Date(2100, 11, 2, 3, 4, 5, 0, time.UTC)
	for _, row := range []struct {
		outcome string
		at      time.Time
	}{
		{"failed", latest.Add(-time.Hour)},
		{"rollback_failed", latest},
		{"success", latest.Add(time.Hour)},
		{"rolled_back", latest.Add(2 * time.Hour)},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
			outcome, executed_at, measured_at) VALUES ('create_index', 'x', $1, $2, $2)`,
			row.outcome, row.at); err != nil {
			t.Fatalf("insert action: %v", err)
		}
	}
	g, err := ReadActionGauges(ctx, tx)
	if err != nil {
		t.Fatalf("read gauges: %v", err)
	}
	if !g.LastFailure.Equal(latest) {
		t.Fatalf("last failure = %v, want %v", g.LastFailure, latest)
	}
}

// A read that fails is an error the refresher reports, never zero.
func TestReadActionGauges_FailedReadIsAnError(t *testing.T) {
	g, err := ReadActionGauges(context.Background(), closedPool(t))
	if err == nil {
		t.Fatal("a failed read returned no error")
	}
	if g.PendingByRisk != nil || !g.LastFailure.IsZero() {
		t.Fatalf("a failed read returned values %+v", g)
	}
}

// snapshotTx is a repeatable-read transaction, rolled back at cleanup:
// other tests' commits do not show, so deltas inside it are exact.
func snapshotTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	pool, ctx := requireDB(t)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return ctx, tx
}

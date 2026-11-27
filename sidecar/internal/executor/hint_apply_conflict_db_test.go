package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/tuner"
)

// tuning-02: the hint apply INSERT has no WHERE NOT EXISTS guard, so over
// an existing row pg_hint_plan 1.7's unique index raises 23505 and the
// action fails loudly instead of "succeeding" as a no-op whose rollback
// could later delete the row that was already there.
//
// No concurrent access tests here: the unique index is what serializes two
// installs, and the executor's DDL slot and target lease are covered by the
// apply tests.

// Error propagation: the unique violation reaches the caller of
// ExecInTransaction with its SQLSTATE, and the operator's row is untouched.
func TestHintApply_ConflictReturnsUniqueViolation(t *testing.T) {
	fx := newHintRollbackFixture(t)
	fx.install(t, rbOperatorHint)
	err := ExecInTransaction(fx.ctx, fx.pool, tuner.BuildInsertSQL(fx.qid, rbOwnHint),
		10*time.Second)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("ExecInTransaction over an existing row = %v, want SQLSTATE 23505", err)
	}
	if got := fx.installed(t); len(got) != 1 || got[0] != rbOperatorHint {
		t.Fatalf("installed = %v, want [%s]", got, rbOperatorHint)
	}
}

// Happy path: with no row for the statement the same INSERT installs it.
func TestHintApply_InstallsWhenFree(t *testing.T) {
	fx := newHintRollbackFixture(t)
	if err := ExecInTransaction(fx.ctx, fx.pool, tuner.BuildInsertSQL(fx.qid, rbOwnHint),
		10*time.Second); err != nil {
		t.Fatalf("ExecInTransaction: %v", err)
	}
	if got := fx.installed(t); len(got) != 1 || got[0] != rbOwnHint {
		t.Fatalf("installed = %v, want [%s]", got, rbOwnHint)
	}
}

// CHECK-01: an operator-approved pg_sage hint action over an operator's row
// ends failed, so no rollback monitor starts, and leaves the one row in place.
func TestHintApply_ManualActionOverOperatorRowFails(t *testing.T) {
	fx := newHintRollbackFixture(t)
	fx.install(t, rbOperatorHint)
	sql := tuner.BuildInsertSQL(fx.qid, rbOwnHint)
	rollback := tuner.BuildDeleteSQL(fx.qid, rbOwnHint)
	var findingID int
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, rollback_sql)
		VALUES ('query_tuning', 'warning', 'query', $1, 'hint', '{}', 'rec', $2, $3)
		RETURNING id`, fmt.Sprintf("queryid:%d", fx.qid), sql, rollback,
	).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, "DELETE FROM sage.action_log WHERE finding_id = $1", findingID)
		_, _ = fx.pool.Exec(c, "DELETE FROM sage.findings WHERE id = $1", findingID)
	})
	e := withTestStandingGate(manualExecutor(fx.pool))
	_, err := e.ExecuteManual(fx.ctx, findingID, sql, rollback, nil)
	if err == nil || !strings.Contains(err.Error(), "23505") {
		t.Fatalf("ExecuteManual over an operator row = %v, want the 23505 failure", err)
	}
	var outcome string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT outcome FROM sage.action_log
		WHERE finding_id = $1`, findingID).Scan(&outcome); err != nil {
		t.Fatalf("read action (exactly one expected): %v", err)
	}
	if outcome != "failed" {
		t.Fatalf("action outcome = %q, want failed (no rollback to monitor)", outcome)
	}
	if got := fx.installed(t); len(got) != 1 || got[0] != rbOperatorHint {
		t.Fatalf("installed = %v, want only [%s]", got, rbOperatorHint)
	}
}

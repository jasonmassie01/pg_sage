package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ledger"
)

// Dogfood lifeos (v1.8.3, action 6385, finding 18007): the recommendation
// paired CREATE INDEX idx_memories_active_query_opt with a rollback that
// drops idx_memories_active_partial (a stale rollback_sql column copied
// into the revision). The identity check withheld it correctly, but the
// withheld row was an action_log row with no verification, which the
// ledger self-audit reported as missing_verification.

func staleRollback(string) string {
	return "DROP INDEX CONCURRENTLY IF EXISTS idx_memories_active_partial"
}

func verifiedPartialDetail() map[string]any {
	detail := legacyDetail("partial_index")
	detail["what_if_verdict"] = "verified"
	detail["hypopg_validated"] = true
	return detail
}

type refusedAction struct {
	id       int64
	outcome  string
	reason   string
	decision *int64
}

func (fx *staleFixture) onlyAction(t *testing.T) refusedAction {
	t.Helper()
	var a refusedAction
	var n int
	err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) OVER (), id, outcome,
		COALESCE(rollback_reason, ''), decision_id FROM sage.action_log
		WHERE sql_executed = $1 ORDER BY id LIMIT 1`, fx.f.RecommendedSQL).
		Scan(&n, &a.id, &a.outcome, &a.reason, &a.decision)
	if err != nil || n != 1 {
		t.Fatalf("action_log rows = %d (%v), want exactly one", n, err)
	}
	return a
}

func TestStaleRollbackIsWithheldNotRun(t *testing.T) {
	fx := newLegacyFixture(t, "partial_index", verifiedPartialDetail(), staleRollback)
	fx.exec.RunCycle(fx.ctx, false)
	a := fx.onlyAction(t)
	if a.outcome != "failed" ||
		!strings.Contains(a.reason, "rollback drops idx_memories_active_partial") {
		t.Fatalf("action = %+v, want failed: rollback drops another index", a)
	}
	if fx.indexExists(t, fx.index()) {
		t.Fatal("CREATE INDEX ran with a rollback that drops another index")
	}
}

// The withheld action is closed with a terminal verification that says it
// never ran, so the self-audit does not report it.
func TestWithheldActionHasTerminalVerification(t *testing.T) {
	fx := newLegacyFixture(t, "partial_index", verifiedPartialDetail(), staleRollback)
	fx.exec.RunCycle(fx.ctx, false)
	a := fx.onlyAction(t)
	if a.decision == nil {
		t.Fatalf("withheld action %d has no decision", a.id)
	}
	var verdict, reason string
	var completed bool
	err := fx.pool.QueryRow(fx.ctx, `SELECT v.verdict, COALESCE(v.reason, ''),
		v.completed_at IS NOT NULL FROM sage.action_log al
		JOIN sage.verification v ON v.id = al.verification_id
		WHERE al.id = $1 AND v.action_log_id = $1`, a.id).Scan(&verdict, &reason, &completed)
	if err != nil {
		t.Fatalf("verification of withheld action %d: %v", a.id, err)
	}
	if verdict != "failed" || !completed || !strings.HasPrefix(reason, "not executed: ") ||
		!strings.Contains(reason, "rollback drops") {
		t.Fatalf("verification = %q/%t %q, want a completed failed verdict saying the "+
			"action never ran", verdict, completed, reason)
	}
	for _, v := range auditViolations(t, fx) {
		if v.ActionID == a.id {
			t.Fatalf("self-audit still reports withheld action %d: %+v", a.id, v)
		}
	}
}

// The audit stays strict: an action that ran and has no verification is
// still a violation.
func TestAuditStillReportsExecutedActionWithoutVerification(t *testing.T) {
	fx := newLegacyFixture(t, "partial_index", verifiedPartialDetail(), staleRollback)
	fx.exec.RunCycle(fx.ctx, false)
	refused := fx.onlyAction(t)
	var ranID int64
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, decision_id)
		VALUES ('analyze', 'ANALYZE public.`+fx.table+`', 'success', $1)
		RETURNING id`, *refused.decision).Scan(&ranID); err != nil {
		t.Fatalf("insert executed action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1",
			ranID)
	})
	for _, v := range auditViolations(t, fx) {
		if v.ActionID == ranID && v.Kind == "missing_verification" {
			return
		}
	}
	t.Fatalf("executed action %d without verification is not reported", ranID)
}

func auditViolations(t *testing.T, fx *staleFixture) []ledger.AuditViolation {
	t.Helper()
	violations, err := ledger.NewPostgresRepository(fx.pool).FindAuditViolations(fx.ctx)
	if err != nil {
		t.Fatalf("self-audit: %v", err)
	}
	return violations
}

// An operator approval of a CREATE INDEX whose rollback drops another
// index is refused before anything is recorded or run: the rollback must
// always undo the DDL being run.
func TestExecuteManual_RefusesRollbackOfAnotherIndex(t *testing.T) {
	e := manualGuardExecutor()
	sql := "CREATE INDEX CONCURRENTLY idx_memories_active_query_opt ON public.memories " +
		"(status) WHERE valid_to IS NULL"
	for _, rollback := range []string{
		"DROP INDEX CONCURRENTLY IF EXISTS idx_memories_active_partial",
		"DROP INDEX CONCURRENTLY other.idx_memories_active_query_opt",
		"DROP INDEX CONCURRENTLY idx_memories_active_query_opt CASCADE",
		"DROP INDEX CONCURRENTLY idx_memories_active_query_opt; DROP TABLE public.memories",
		"SELECT 1",
	} {
		id, err := e.ExecuteManual(context.Background(), 1, sql, rollback, nil)
		if !errors.Is(err, ErrRollbackMismatch) || id != 0 {
			t.Errorf("rollback %q: id=%d err=%v, want ErrRollbackMismatch", rollback, id, err)
		}
	}
}

func TestExecuteManual_MatchingRollbackPassesGuard(t *testing.T) {
	e := manualGuardExecutor()
	sql := "CREATE INDEX CONCURRENTLY idx_q ON public.memories (status)"
	for _, rollback := range []string{"", "DROP INDEX CONCURRENTLY IF EXISTS idx_q",
		`DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_q"`, "DROP INDEX public.idx_q;"} {
		_, err := e.ExecuteManual(context.Background(), 1, sql, rollback, nil)
		if errors.Is(err, ErrRollbackMismatch) {
			t.Errorf("rollback %q refused as a mismatch", rollback)
		}
	}
	unnamed := `CREATE INDEX CONCURRENTLY ON "public"."orders" ("customer_id")`
	_, err := e.ExecuteManual(context.Background(), 1, unnamed, "", nil)
	if errors.Is(err, ErrRollbackMismatch) {
		t.Error("unnamed FK index without rollback refused as a mismatch")
	}
}

func manualGuardExecutor() *Executor {
	return New(nil, config.DefaultConfig(), time.Now().Add(-90*24*time.Hour),
		func(string, string, ...any) {})
}

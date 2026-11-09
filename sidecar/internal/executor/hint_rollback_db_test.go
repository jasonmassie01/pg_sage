package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// tuning-02a: an action recorded before the fix carries the unscoped hint
// DELETE. At rollback time the executor rewrites it to pg_sage's recorded
// hint text, or refuses with hint_text_unknown instead of deleting blindly.
//
// No concurrent access tests here: scopeHintRollback is a read-only lookup,
// and the rollback claim that follows it is covered by the rollback tests.

const (
	rbOwnHint      = "HashJoin(a b)"
	rbOperatorHint = "NestLoop(a b)"
)

type hintRollbackFixture struct {
	pool *pgxpool.Pool
	ctx  context.Context
	exec *Executor
	qid  int64
}

func newHintRollbackFixture(t *testing.T) *hintRollbackFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	requireQueryIDHintLayout(t, pool)
	for _, s := range []string{
		`CREATE SCHEMA IF NOT EXISTS hint_plan`,
		`CREATE TABLE IF NOT EXISTS hint_plan.hints (id serial PRIMARY KEY,
			query_id bigint NOT NULL, application_name text NOT NULL, hints text NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS hints_id_and_app
			ON hint_plan.hints (query_id, application_name)`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	fx := &hintRollbackFixture{pool: pool, ctx: ctx,
		qid: int64(9_220_000_000) + time.Now().UnixNano()%1_000_000}
	fx.exec = New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	fx.exec.cfg.Trust.Level = "advisory"
	fx.exec.SetExecutorEnabled(true)
	fx.exec.emergencyStopFn = func(context.Context) bool { return false }
	// The operator rollback is a standing-gate decision (policy-05); production
	// always installs the gate.
	fx.exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM hint_plan.hints WHERE query_id = $1`, fx.qid)
		_, _ = pool.Exec(c, `DELETE FROM sage.query_hints WHERE queryid = $1`, fx.qid)
		_, _ = pool.Exec(c, `DELETE FROM sage.action_log WHERE sql_executed LIKE $1`,
			fmt.Sprintf("%%%d, ''%%", fx.qid)) // INSERT ... SELECT or VALUES form
	})
	return fx
}

// requireQueryIDHintLayout skips on a server whose installed pg_hint_plan
// predates 1.7 (PG14-16): its hint_plan.hints keys hints by
// norm_query_string, the tuner treats that table as not ready, and so no
// hint action, legacy or current, can exist there to roll back.
func requireQueryIDHintLayout(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var exists, hasQueryID bool
	err := pool.QueryRow(context.Background(), `SELECT
		to_regclass('hint_plan.hints') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
			WHERE attrelid = to_regclass('hint_plan.hints')
			  AND attname = 'query_id' AND NOT attisdropped)`).Scan(&exists, &hasQueryID)
	if err != nil {
		t.Fatalf("inspect hint_plan.hints: %v", err)
	}
	if exists && !hasQueryID {
		t.Skip("pg_hint_plan < 1.7: hint_plan.hints has no query_id column")
	}
}

func (fx *hintRollbackFixture) install(t *testing.T, hint string) {
	t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, `INSERT INTO hint_plan.hints
		(query_id, application_name, hints) VALUES ($1, '', $2)`, fx.qid, hint); err != nil {
		t.Fatalf("install hint: %v", err)
	}
}

func (fx *hintRollbackFixture) record(t *testing.T, hint string) {
	t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status) VALUES ($1, $2, 's', 'active')`,
		fx.qid, hint); err != nil {
		t.Fatalf("record hint: %v", err)
	}
}

// legacyAction logs an applied hint action as releases before tuning-02a
// did: the INSERT for rbOwnHint and the unscoped rollback DELETE.
func (fx *hintRollbackFixture) legacyAction(t *testing.T, outcome string) int64 {
	t.Helper()
	var id int64
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, outcome)
		VALUES ('apply_query_hint', $1, $2, $3) RETURNING id`,
		tuner.BuildInsertSQL(fx.qid, rbOwnHint),
		legacyHintDelete(fmt.Sprint(fx.qid)), outcome).Scan(&id); err != nil {
		t.Fatalf("log legacy action: %v", err)
	}
	return id
}

func (fx *hintRollbackFixture) installed(t *testing.T) []string {
	t.Helper()
	rows, err := fx.pool.Query(fx.ctx, `SELECT hints FROM hint_plan.hints
		WHERE query_id = $1 ORDER BY hints`, fx.qid)
	if err != nil {
		t.Fatalf("read installed hints: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (fx *hintRollbackFixture) outcome(t *testing.T, id int64) (string, string) {
	t.Helper()
	var outcome, reason string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT outcome, COALESCE(rollback_reason, '')
		FROM sage.action_log WHERE id = $1`, id).Scan(&outcome, &reason); err != nil {
		t.Fatalf("read action %d: %v", id, err)
	}
	return outcome, reason
}

// Boundary + legacy: the old action's INSERT was a no-op over an operator's
// H2. The manual rollback is rewritten to H1's text and deletes nothing.
func TestHintRollback_ManualLegacyKeepsOperatorHint(t *testing.T) {
	fx := newHintRollbackFixture(t)
	fx.record(t, rbOwnHint)
	fx.install(t, rbOperatorHint)
	id := fx.legacyAction(t, "success")
	if err := fx.exec.RollbackAction(fx.ctx, id, "operator test"); err != nil {
		t.Fatalf("RollbackAction: %v", err)
	}
	if got := fx.installed(t); len(got) != 1 || got[0] != rbOperatorHint {
		t.Fatalf("installed after rollback = %v, want [%s]", got, rbOperatorHint)
	}
	if outcome, _ := fx.outcome(t, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
}

// Happy path + legacy: the old action did install H1; the rewritten
// rollback removes exactly it.
func TestHintRollback_ManualLegacyDeletesOwnHint(t *testing.T) {
	fx := newHintRollbackFixture(t)
	fx.record(t, rbOperatorHint) // an older pg_sage proposal for the query
	fx.record(t, rbOwnHint)
	fx.install(t, rbOwnHint)
	id := fx.legacyAction(t, "success")
	if err := fx.exec.RollbackAction(fx.ctx, id, "operator test"); err != nil {
		t.Fatalf("RollbackAction: %v", err)
	}
	if got := fx.installed(t); len(got) != 0 {
		t.Fatalf("installed after rollback = %v, want none", got)
	}
}

// Legacy without recorded text: refused with hint_text_unknown, nothing is
// deleted and the action is not claimed.
func TestHintRollback_ManualLegacyUnknownTextRefused(t *testing.T) {
	fx := newHintRollbackFixture(t)
	fx.install(t, rbOperatorHint)
	id := fx.legacyAction(t, "success")
	err := fx.exec.RollbackAction(fx.ctx, id, "operator test")
	if !errors.Is(err, ErrHintTextUnknown) ||
		!strings.Contains(err.Error(), "hint_text_unknown") {
		t.Fatalf("RollbackAction = %v, want ErrHintTextUnknown", err)
	}
	if got := fx.installed(t); len(got) != 1 || got[0] != rbOperatorHint {
		t.Fatalf("installed after refusal = %v, want [%s]", got, rbOperatorHint)
	}
	if outcome, _ := fx.outcome(t, id); outcome != "success" {
		t.Fatalf("outcome = %q, want success (unclaimed)", outcome)
	}
}

// Automatic rollback on a regression: the legacy SQL is scoped before the
// policy sees it, and the operator's row survives.
func TestHintRollback_AutoLegacyScoped(t *testing.T) {
	fx := newHintRollbackFixture(t)
	fx.record(t, rbOwnHint)
	fx.install(t, rbOperatorHint)
	id := fx.legacyAction(t, "pending")
	var authorized []string
	cfg := r2MonitorConfig()
	cfg.Authorize = func(_ context.Context, sql string) bool {
		authorized = append(authorized, sql)
		return true
	}
	rollbackRegressedAction(fx.ctx, fx.pool, id,
		legacyHintDelete(fmt.Sprint(fx.qid)), cfg, nopLog)
	want := tuner.BuildDeleteSQL(fx.qid, rbOwnHint)
	if len(authorized) != 1 || authorized[0] != want {
		t.Fatalf("authorized = %q, want [%q]", authorized, want)
	}
	if outcome, _ := fx.outcome(t, id); outcome != "rolled_back" {
		t.Fatalf("outcome = %q, want rolled_back", outcome)
	}
	if got := fx.installed(t); len(got) != 1 || got[0] != rbOperatorHint {
		t.Fatalf("installed after rollback = %v, want [%s]", got, rbOperatorHint)
	}
}

// Automatic rollback with no recorded text: skipped with the reason
// hint_text_unknown; the policy is never asked and nothing is deleted.
func TestHintRollback_AutoLegacyUnknownTextSkipped(t *testing.T) {
	fx := newHintRollbackFixture(t)
	fx.install(t, rbOperatorHint)
	id := fx.legacyAction(t, "pending")
	cfg := r2MonitorConfig()
	asked := false
	cfg.Authorize = func(context.Context, string) bool { asked = true; return true }
	rollbackRegressedAction(fx.ctx, fx.pool, id,
		legacyHintDelete(fmt.Sprint(fx.qid)), cfg, nopLog)
	outcome, reason := fx.outcome(t, id)
	if outcome != "rollback_skipped" || !strings.Contains(reason, "hint_text_unknown") {
		t.Fatalf("outcome = %q (%q), want rollback_skipped hint_text_unknown",
			outcome, reason)
	}
	if asked {
		t.Fatal("policy was asked to authorize an unscoped hint rollback")
	}
	if got := fx.installed(t); len(got) != 1 || got[0] != rbOperatorHint {
		t.Fatalf("installed after skip = %v, want [%s]", got, rbOperatorHint)
	}
}

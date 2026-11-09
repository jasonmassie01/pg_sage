package tuner

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// tuning-02a: every DELETE pg_sage builds for hint_plan.hints is scoped to
// pg_sage's own hint text, so a rollback or removal never deletes a hint an
// operator installed for the same query.

const (
	scopeOwnHint      = "HashJoin(a b)"
	scopeOperatorHint = "NestLoop(a b)"
)

func hintScopeDB(t *testing.T, ids ...int64) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := connectTunerTestDB(t)
	t.Cleanup(pool.Close)
	requireQueryIDHintTable(t, pool)
	prepareHintTables(t, pool, ids)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM hint_plan.hints WHERE query_id = ANY($1)`, ids)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.query_hints WHERE queryid = ANY($1)`, ids)
	})
	return pool, context.Background()
}

func installHintRow(t *testing.T, pool *pgxpool.Pool, qid int64, app, hint string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO hint_plan.hints
		(query_id, application_name, hints) VALUES ($1, $2, $3)`, qid, app, hint); err != nil {
		t.Fatalf("install hint row %d/%q: %v", qid, app, err)
	}
}

func recordSageHint(t *testing.T, pool *pgxpool.Pool, qid int64, hint, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status) VALUES ($1, $2, 's', $3)`,
		qid, hint, status); err != nil {
		t.Fatalf("record sage hint %d: %v", qid, err)
	}
}

// installedHints lists the hint texts installed for qid (any application).
func installedHints(t *testing.T, pool *pgxpool.Pool, qid int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT application_name || ':' || hints
		FROM hint_plan.hints WHERE query_id = $1 ORDER BY application_name, hints`, qid)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func execRows(t *testing.T, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	tag, err := pool.Exec(context.Background(), sql)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return tag.RowsAffected()
}

// Boundary (failed before tuning-02a): pg_sage's H1 apply over an
// operator's H2 installs nothing (since tuning-02 it fails on the unique
// index), so rolling it back must delete nothing.
func TestHintScope_NoopActionRollbackKeepsOperatorHint(t *testing.T) {
	const qid = int64(9_210_001)
	pool, ctx := hintScopeDB(t, qid)
	installHintRow(t, pool, qid, "", scopeOperatorHint)
	if _, err := pool.Exec(ctx, BuildInsertSQL(qid, scopeOwnHint)); err == nil {
		t.Fatal("apply over an operator row succeeded, want a unique violation")
	}
	if n := execRows(t, pool, BuildDeleteSQL(qid, scopeOwnHint)); n != 0 {
		t.Fatalf("rollback deleted %d rows, want 0", n)
	}
	got := installedHints(t, pool, qid)
	if len(got) != 1 || got[0] != ":"+scopeOperatorHint {
		t.Fatalf("installed after rollback = %v, want [:%s]", got, scopeOperatorHint)
	}
}

// Happy path: rolling back an action that did insert H1 deletes exactly H1,
// not another application's row or another query's row.
func TestHintScope_RollbackDeletesExactlyOwnHint(t *testing.T) {
	const qid, other = int64(9_210_011), int64(9_210_012)
	pool, _ := hintScopeDB(t, qid, other)
	if n := execRows(t, pool, BuildInsertSQL(qid, scopeOwnHint)); n != 1 {
		t.Fatalf("apply inserted %d rows, want 1", n)
	}
	installHintRow(t, pool, qid, "psql", scopeOwnHint)
	installHintRow(t, pool, other, "", scopeOwnHint)
	if n := execRows(t, pool, BuildDeleteSQL(qid, scopeOwnHint)); n != 1 {
		t.Fatalf("rollback deleted %d rows, want 1", n)
	}
	if got := installedHints(t, pool, qid); len(got) != 1 || got[0] != "psql:"+scopeOwnHint {
		t.Fatalf("query %d after rollback = %v, want only the psql row", qid, got)
	}
	if got := installedHints(t, pool, other); len(got) != 1 {
		t.Fatalf("query %d after rollback = %v, want untouched", other, got)
	}
}

// Removal: a retired H1 whose row an operator replaced with H2 yields no
// removal finding; a still-installed retired H1 yields a scoped DELETE.
func TestHintScope_RemovalLeavesOperatorReplacement(t *testing.T) {
	const replaced, installed = int64(9_210_021), int64(9_210_022)
	pool, ctx := hintScopeDB(t, replaced, installed)
	recordSageHint(t, pool, replaced, scopeOwnHint, "retired")
	installHintRow(t, pool, replaced, "", scopeOperatorHint)
	recordSageHint(t, pool, installed, scopeOwnHint, "broken")
	installHintRow(t, pool, installed, "", scopeOwnHint)
	tu := New(pool, TunerConfig{}, &HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: true}, noopLog2)
	findings := tu.hintRemovalFindings(ctx)
	if len(findings) != 1 || findings[0].ObjectIdentifier != "queryid:9210022" {
		t.Fatalf("removal findings = %+v, want only queryid:9210022", findings)
	}
	f := findings[0]
	if f.RecommendedSQL != BuildDeleteSQL(installed, scopeOwnHint) {
		t.Fatalf("removal SQL = %q, want the scoped delete", f.RecommendedSQL)
	}
	if f.Detail["hint_status"] != "broken" {
		t.Fatalf("hint_status = %v, want broken", f.Detail["hint_status"])
	}
	// The operator replaces the row between the finding and its execution.
	if _, err := pool.Exec(ctx, `UPDATE hint_plan.hints SET hints = $2
		WHERE query_id = $1`, installed, scopeOperatorHint); err != nil {
		t.Fatal(err)
	}
	if n := execRows(t, pool, f.RecommendedSQL); n != 0 {
		t.Fatalf("stale removal deleted %d rows, want 0", n)
	}
	for _, qid := range []int64{replaced, installed} {
		if got := installedHints(t, pool, qid); len(got) != 1 ||
			got[0] != ":"+scopeOperatorHint {
			t.Errorf("query %d hints = %v, want the operator's", qid, got)
		}
	}
}

// Invalid input: a hint containing the default dollar tag gets another tag,
// and the DELETE still matches exactly the installed text.
func TestHintScope_TagCollisionStillMatchesExactly(t *testing.T) {
	const qid = int64(9_210_031)
	pool, _ := hintScopeDB(t, qid)
	hint := "Leading((a b)) /* $sageqh$ */"
	sql := BuildDeleteSQL(qid, hint)
	if !strings.Contains(sql, "$sageqh1$") {
		t.Fatalf("delete did not rotate the dollar tag: %s", sql)
	}
	if n := execRows(t, pool, BuildInsertSQL(qid, hint)); n != 1 {
		t.Fatalf("apply inserted %d rows, want 1", n)
	}
	if n := execRows(t, pool, BuildDeleteSQL(qid, hint+" ")); n != 0 {
		t.Fatalf("near-match delete removed %d rows, want 0", n)
	}
	if n := execRows(t, pool, sql); n != 1 {
		t.Fatalf("scoped delete removed %d rows, want 1", n)
	}
	if got := installedHints(t, pool, qid); len(got) != 0 {
		t.Fatalf("installed after delete = %v, want none", got)
	}
}

// State transition: an active pg_sage hint whose row now holds an
// operator's text is no longer installed, so reconcile marks it rolled back;
// an active hint whose own text is installed stays active.
func TestHintScope_ReconcileRolledBackByText(t *testing.T) {
	const replaced, kept = int64(9_210_041), int64(9_210_042)
	pool, ctx := hintScopeDB(t, replaced, kept)
	recordSageHint(t, pool, replaced, scopeOwnHint, "active")
	installHintRow(t, pool, replaced, "", scopeOperatorHint)
	recordSageHint(t, pool, kept, scopeOwnHint, "active")
	installHintRow(t, pool, kept, "", scopeOwnHint)
	readyTuner(pool).reconcileHintStatuses(ctx)
	for qid, want := range map[int64]string{replaced: "rolled_back", kept: "active"} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM sage.query_hints
			WHERE queryid = $1`, qid).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Errorf("query %d status = %q, want %q", qid, status, want)
		}
	}
}

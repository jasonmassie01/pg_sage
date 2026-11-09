package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// tuning-06: sage.query_hints.status gains 'rejected' and 'expired' and a
// NOT VALID CHECK on the lifecycle values (validated in a later release),
// added idempotently by Bootstrap.

func queryHintsStatusCheck(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (
	def string, validated bool) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT COALESCE(max(pg_get_constraintdef(oid)), ''),
       COALESCE(bool_or(convalidated), false)
FROM pg_constraint
WHERE conrelid = 'sage.query_hints'::regclass AND conname = 'query_hints_status_check'`).
		Scan(&def, &validated)
	if err != nil {
		t.Fatalf("read the query_hints status check: %v", err)
	}
	return def, validated
}

func TestQueryHintsStatusCheck_BootstrapTwice(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	def, validated := queryHintsStatusCheck(t, ctx, pool)
	if def == "" {
		t.Fatal("no query_hints_status_check constraint after Bootstrap")
	}
	if validated {
		t.Fatal("the status check is validated; it ships NOT VALID")
	}
	for _, s := range []string{"proposed", "active", "dormant", "retired", "broken",
		"rolled_back", "rejected", "expired"} {
		if !strings.Contains(def, "'"+s+"'") {
			t.Fatalf("status check %q does not allow %q", def, s)
		}
	}
}

// Invalid input: a new row with an unknown status is refused; every
// lifecycle status is accepted.
func TestQueryHintsStatusCheck_RefusesUnknownStatus(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	const qid = int64(-9_260_901)
	clean := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.query_hints WHERE queryid = $1", qid)
	}
	clean()
	t.Cleanup(clean)
	_, err := pool.Exec(ctx, `INSERT INTO sage.query_hints (queryid, hint_text, symptom,
		status) VALUES ($1, 'SeqScan(t)', 's', 'pending')`, qid)
	if err == nil || !strings.Contains(err.Error(), "query_hints_status_check") {
		t.Fatalf("an unknown status was accepted (err %v)", err)
	}
	for _, s := range []string{"rejected", "expired", "proposed"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints (queryid, hint_text,
			symptom, status) VALUES ($1, 'SeqScan(t)', 's', $2)`, qid, s); err != nil {
			t.Fatalf("status %s refused: %v", s, err)
		}
	}
}

// The migration re-runs cleanly on a migrated schema and bounds its lock
// wait.
func TestQueryHintsStatusCheck_RerunIsCleanAndBounded(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for run := 0; run < 2; run++ {
		if _, err := pool.Exec(ctx, ddlQueryHintsStatusCheck); err != nil {
			t.Fatalf("re-run %d: %v", run, err)
		}
	}
	if !strings.Contains(ddlQueryHintsStatusCheck, "lock_timeout") {
		t.Fatal("ddlQueryHintsStatusCheck must set a lock_timeout before its ALTER")
	}
}

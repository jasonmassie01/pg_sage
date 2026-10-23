package schema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHECK-FND-01: Bootstrap creates sage.change_review and
// sage.change_review_budget_day with every CHECK constraint and partial
// index; a second Bootstrap changes no pg_class, pg_constraint or pg_index
// row of sage.change_review*; sage.toil_model has one active
// 'change_review' row (15 minutes, model 1); invalid enums fail 23514.

var changeReviewChecks = []string{
	"change_review_source_check", "change_review_state_check",
	"change_review_verdict_check", "change_review_deterministic_verdict_check",
	"change_review_parse_mode_check", "change_review_model_status_check",
	"change_review_dba_verdict_check", "change_review_dba_direction_check",
	"change_review_landing_state_check", "change_review_landing_source_check",
	"change_review_receipt_verdict_check", "change_review_shadow_score_check",
	"change_review_credit_state_check", "change_review_override_reason_check",
	"change_review_complete_check", "change_review_landed_check",
	"change_review_credit_final_check",
}

var changeReviewPartialIndexes = map[string]string{
	"change_review_landing_awaiting": "landing_state = 'awaiting'",
	"change_review_receipt_due":      "receipt_verdict = 'pending'",
	"idx_change_review_credit":       "credit_state = 'final'",
	"change_review_dba_queue":        "dba_verdict IS NULL",
	"change_review_running":          "state = 'running'",
}

// catalogRows is every pg_class, pg_constraint and pg_index row of the
// change_review relations, rendered as text.
func catalogRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var out string
	err := pool.QueryRow(ctx, `SELECT
		(SELECT string_agg(format('%s|%s|%s|%s|%s', c.oid, c.relname, c.relkind,
			c.relnatts, c.relfilenode), ',' ORDER BY c.relname)
		   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'sage' AND c.relname LIKE 'change_review%'
		     OR n.nspname = 'sage' AND c.relname = 'idx_change_review_credit')
		|| '#' ||
		(SELECT string_agg(format('%s|%s|%s', k.oid, k.conname,
			pg_get_constraintdef(k.oid)), ',' ORDER BY k.conname)
		   FROM pg_constraint k WHERE k.conrelid IN
		   ('sage.change_review'::regclass, 'sage.change_review_budget_day'::regclass))
		|| '#' ||
		(SELECT string_agg(format('%s|%s|%s', i.indexrelid, i.indisvalid,
			pg_get_indexdef(i.indexrelid)), ',' ORDER BY i.indexrelid)
		   FROM pg_index i WHERE i.indrelid IN
		   ('sage.change_review'::regclass, 'sage.change_review_budget_day'::regclass))`).
		Scan(&out)
	if err != nil {
		t.Fatalf("read catalog rows: %v", err)
	}
	return out
}

func TestChangeReviewMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	before := catalogRows(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	if after := catalogRows(t, ctx, pool); after != before {
		t.Fatalf("a second Bootstrap changed the catalog:\nbefore %s\nafter  %s", before,
			after)
	}
	if !strings.Contains(before, "change_review_budget_day") {
		t.Fatal("sage.change_review_budget_day is missing")
	}
}

func TestChangeReviewConstraintsAndIndexes(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, name := range changeReviewChecks {
		var contype string
		err := pool.QueryRow(ctx, `SELECT contype FROM pg_constraint
			WHERE conrelid = 'sage.change_review'::regclass AND conname = $1`, name).
			Scan(&contype)
		if err != nil || contype != "c" {
			t.Errorf("CHECK %s: contype %q (%v)", name, contype, err)
		}
	}
	for name, predicate := range changeReviewPartialIndexes {
		var def string
		err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid) FROM pg_index
			WHERE indexrelid = to_regclass('sage.' || $1)`, name).Scan(&def)
		if err != nil || !strings.Contains(def, "WHERE") || !strings.Contains(def, predicate) {
			t.Errorf("partial index %s = %q (%v), want WHERE ... %s", name, def, err,
				predicate)
		}
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef('sage.idx_change_review_credit'::regclass)`).
		Scan(&def); err != nil || !strings.Contains(def, "INCLUDE (credited_minutes)") {
		t.Fatalf("idx_change_review_credit = %q (%v), want credited_minutes included", def, err)
	}
	for _, idx := range []string{"change_review_pr_created", "change_review_created"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`,
			idx).Scan(&ok); err != nil || !ok {
			t.Errorf("index %s missing (%v)", idx, err)
		}
	}
}

func TestChangeReviewToilModelSeed(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var n, minutes, version int
	err := pool.QueryRow(ctx, `SELECT count(*), max(base_minutes)::int, max(model_version)
		FROM sage.toil_model WHERE action_type = 'change_review' AND effective_to IS NULL`).
		Scan(&n, &minutes, &version)
	if err != nil || n != 1 || minutes != 15 || version != 1 {
		t.Fatalf("toil model rows %d, minutes %d, version %d (%v)", n, minutes, version, err)
	}
}

const changeReviewInsert = `INSERT INTO sage.change_review (database_name, source, actor,
	input_hash, verdict, credit_state) VALUES ('db', 'cli', 'mig:cr', repeat('a', 64), $1, $2)`

func TestChangeReviewEnumsRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.change_review WHERE actor = 'mig:cr'`)
	})
	if _, err := pool.Exec(ctx, changeReviewInsert, "safe", "none"); err != nil {
		t.Fatalf("a valid row was refused: %v", err)
	}
	for name, args := range map[string][2]string{
		"verdict unsafe": {"unsafe", "none"},
		"credit paid":    {"safe", "paid"},
	} {
		_, err := pool.Exec(ctx, changeReviewInsert, args[0], args[1])
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: %v, want SQLSTATE 23514", name, err)
		}
	}
	for name, sql := range changeReviewInvalidRows() {
		_, err := pool.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: %v, want SQLSTATE 23514", name, err)
		}
	}
}

func changeReviewInvalidRows() map[string]string {
	head := `INSERT INTO sage.change_review (database_name, source, actor, input_hash`
	vals := ` VALUES ('db', 'cli', 'mig:cr', repeat('a', 64)`
	return map[string]string{
		"unknown source": `INSERT INTO sage.change_review (database_name, source, actor,
			input_hash) VALUES ('db', 'slack', 'mig:cr', repeat('a', 64))`,
		"override without reason": head + `, dba_verdict, dba_direction)` + vals +
			`, 'override', 'too_strict')`,
		"complete without verdict": head + `, state)` + vals + `, 'complete')`,
		"landed without bracket":   head + `, landing_state)` + vals + `, 'landed')`,
		"final without timestamp":  head + `, credit_state)` + vals + `, 'final')`,
		"short input hash": `INSERT INTO sage.change_review (database_name, source, actor,
			input_hash) VALUES ('db', 'cli', 'mig:cr', 'abc')`,
		"bad head sha":    head + `, head_sha)` + vals + `, 'XYZ')`,
		"long title":      head + `, pr_title)` + vals + `, repeat('t', 301))`,
		"negative tokens": `INSERT INTO sage.change_review_budget_day (day, actor, tokens)
			VALUES (current_date, 'mig:cr', -1)`,
	}
}

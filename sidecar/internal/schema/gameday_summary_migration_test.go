package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// benches-08: sage.sre_game_days gains summary (what the run's report
// scored, '{}' when there is none) and the terminal status
// completed_unscored (the run finished but scored no run).

// preSummaryGameDays is the sre_game_days shape after benches-03.
const preSummaryGameDays = `
ALTER TABLE sage.sre_game_days DROP COLUMN IF EXISTS summary;
ALTER TABLE sage.sre_game_days DROP CONSTRAINT IF EXISTS sre_game_days_status_v2_check;
ALTER TABLE sage.sre_game_days ADD CONSTRAINT sre_game_days_status_v2_check CHECK (status IN
    ('running', 'completed', 'failed', 'destroy_failed', 'abandoned'));
`

// requireGameDaySummaryShape checks the migrated shape: one status check
// taking completed_unscored and every earlier status, and summary a
// NOT NULL object defaulting to '{}'.
func requireGameDaySummaryShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	checks := gameDayStatusChecks(t, ctx, pool)
	if len(checks) != 1 {
		t.Fatalf("status checks = %q, want exactly one", checks)
	}
	for _, s := range []string{"running", "completed", "failed", "destroy_failed",
		"abandoned", "completed_unscored"} {
		if !strings.Contains(checks[0], "'"+s+"'") {
			t.Fatalf("status check %q does not allow %s", checks[0], s)
		}
	}
	var notNull bool
	var def, typ string
	if err := pool.QueryRow(ctx, `SELECT a.attnotnull, format_type(a.atttypid, a.atttypmod),
		COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a LEFT JOIN pg_attrdef d
		  ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = 'sage.sre_game_days'::regclass AND a.attname = 'summary'
		  AND NOT a.attisdropped`).Scan(&notNull, &typ, &def); err != nil {
		t.Fatalf("summary column: %v", err)
	}
	if !notNull || typ != "jsonb" || !strings.Contains(def, "'{}'") {
		t.Fatalf("summary = %s not null %v default %q, want jsonb NOT NULL DEFAULT '{}'",
			typ, notNull, def)
	}
}

// Integration: a fresh bootstrap, a second one, and an upgrade of the
// benches-03 shape all end in the same shape; a row written before the
// upgrade reads '{}', and completed_unscored is accepted afterwards.
func TestGameDaySummaryMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireGameDaySummaryShape(t, ctx, pool)

	dep := "00000000-0000-4000-8000-0000000b3508"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.sre_game_days WHERE deployment_id = $1`, dep)
	})
	if _, err := pool.Exec(ctx, preSummaryGameDays); err != nil {
		t.Fatalf("simulating the pre-benches-08 schema: %v", err)
	}
	insert := `INSERT INTO sage.sre_game_days (deployment_id, id, database_name, provider,
		families, status, finished_at) VALUES ($1, gen_random_uuid(), 'orders', 'dle', '{}',
		$2, now())`
	if _, err := pool.Exec(ctx, insert, dep, "completed_unscored"); err == nil {
		t.Fatal("the pre-benches-08 shape accepted completed_unscored; the test is not " +
			"simulating it")
	}
	if _, err := pool.Exec(ctx, insert, dep, "completed"); err != nil {
		t.Fatalf("a legacy completed row: %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	requireGameDaySummaryShape(t, ctx, pool)
	var summary string
	if err := pool.QueryRow(ctx, `SELECT summary::text FROM sage.sre_game_days
		WHERE deployment_id = $1`, dep).Scan(&summary); err != nil || summary != "{}" {
		t.Fatalf("legacy row summary = %q (%v), want {}", summary, err)
	}
	if _, err := pool.Exec(ctx, insert, dep, "completed_unscored"); err != nil {
		t.Fatalf("completed_unscored after the upgrade: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, dep, "unscored"); err == nil {
		t.Fatal("an unknown status was accepted")
	}
	bootstrapWithRetry(t, ctx, pool)
	requireGameDaySummaryShape(t, ctx, pool)
	// The benches-03 shape (checked by its own test) still holds.
	requireBenchCrashShape(t, ctx, pool)
}

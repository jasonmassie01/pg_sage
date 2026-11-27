package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// benches-03: sage.sre_game_days gains kind ('game_day' or 'local') and
// the terminal status 'abandoned' (a run its process never finished), and
// sage.bench_fixture_state records the ALTER SYSTEM settings a fault
// program set, so the next run can reset a crashed run's leftovers.

// legacyGameDays is the sre_game_days shape before benches-03.
const legacyGameDays = `
ALTER TABLE sage.sre_game_days DROP COLUMN IF EXISTS kind;
ALTER TABLE sage.sre_game_days DROP CONSTRAINT IF EXISTS sre_game_days_status_v2_check;
ALTER TABLE sage.sre_game_days DROP CONSTRAINT IF EXISTS sre_game_days_status_check;
ALTER TABLE sage.sre_game_days ADD CONSTRAINT sre_game_days_status_check CHECK (status IN
    ('running', 'completed', 'failed', 'destroy_failed'));
`

// sage.bench_fixture_state is not dropped in the legacy shape: fault
// programs of other test packages may be using it on a shared server. A
// fresh server's first bootstrap creates it.

func gameDayStatusChecks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'sage.sre_game_days'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%status = ANY%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatal(err)
		}
		out = append(out, def)
	}
	return out
}

// requireBenchCrashShape checks the migrated shape, and that it accepts an
// abandoned local run and refuses an unknown kind or status.
func requireBenchCrashShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	checks := gameDayStatusChecks(t, ctx, pool)
	if len(checks) != 1 || !strings.Contains(checks[0], "abandoned") {
		t.Fatalf("status checks = %q, want one allowing abandoned", checks)
	}
	var kindDefault string
	if err := pool.QueryRow(ctx, `SELECT pg_get_expr(d.adbin, d.adrelid)
		FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid = d.adrelid
		 AND a.attnum = d.adnum
		WHERE d.adrelid = 'sage.sre_game_days'::regclass AND a.attname = 'kind'`).
		Scan(&kindDefault); err != nil || !strings.Contains(kindDefault, "game_day") {
		t.Fatalf("kind default = %q (%v), want 'game_day'", kindDefault, err)
	}
	dep := "00000000-0000-4000-8000-0000000b3503"
	// Deleted on return: the legacy shape's check refuses an abandoned row.
	defer func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.sre_game_days WHERE deployment_id = $1`, dep)
	}()
	insert := `INSERT INTO sage.sre_game_days (deployment_id, id, database_name, provider,
		families, status, finished_at, kind)
		VALUES ($1, gen_random_uuid(), 'orders', 'dle', '{}', $2, now(), $3)`
	if _, err := pool.Exec(ctx, insert, dep, "abandoned", "local"); err != nil {
		t.Fatalf("an abandoned local run: %v", err)
	}
	for _, bad := range [][2]string{{"lost", "local"}, {"failed", "nightly"}} {
		if _, err := pool.Exec(ctx, insert, dep, bad[0], bad[1]); err == nil {
			t.Fatalf("status %q kind %q was accepted", bad[0], bad[1])
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.bench_fixture_state (setting)
		VALUES ('max_wal_size') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("bench_fixture_state: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.bench_fixture_state
		WHERE setting = 'max_wal_size'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.bench_fixture_state (setting)
		VALUES ('max_wal_size; DROP TABLE x')`); err == nil {
		t.Fatal("bench_fixture_state accepted a setting that is not a setting name")
	}
}

// Integration: a fresh bootstrap, a second one, and an upgrade of the
// pre-benches-03 shape all end in the same shape; existing rows become
// game days.
func TestBenchCrashRecoveryMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requireBenchCrashShape(t, ctx, pool)

	if _, err := pool.Exec(ctx, legacyGameDays); err != nil {
		t.Fatalf("simulating the pre-benches-03 schema: %v", err)
	}
	dep := "00000000-0000-4000-8000-0000000b3504"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.sre_game_days WHERE deployment_id = $1`, dep)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_game_days (deployment_id, id,
		database_name, provider, families, status) VALUES
		($1, gen_random_uuid(), 'orders', 'dle', '{}', 'running')`, dep); err != nil {
		t.Fatalf("a legacy running row: %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	requireBenchCrashShape(t, ctx, pool)
	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM sage.sre_game_days
		WHERE deployment_id = $1`, dep).Scan(&kind); err != nil || kind != "game_day" {
		t.Fatalf("legacy row kind = %q (%v), want game_day", kind, err)
	}
	bootstrapWithRetry(t, ctx, pool)
	requireBenchCrashShape(t, ctx, pool)
}

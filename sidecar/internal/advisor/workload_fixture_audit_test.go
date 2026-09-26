//go:build integration || e2e

package advisor

import (
	"time"
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// waitForFixtureDeadTuples waits until pg_stat_user_tables shows the
// seeded dead tuples. PG14's statistics collector publishes them
// asynchronously; scenarios that read dead-tuple counts must not race it.
func waitForFixtureDeadTuples(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var dead int64
		err := pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT n_dead_tup
			FROM pg_stat_user_tables WHERE relid = 'public.advisor_fixture'::regclass), 0)`,
		).Scan(&dead)
		if err != nil {
			t.Fatalf("read advisor fixture stats: %v", err)
		}
		if dead > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("statistics never reported the seeded dead tuples")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// seedAuditWorkload makes advisor scenarios independent of preexisting user data.
func seedAuditWorkload(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
  CREATE TABLE IF NOT EXISTS public.advisor_fixture (
   id integer PRIMARY KEY, payload text
  ) WITH (autovacuum_enabled=false);
  TRUNCATE public.advisor_fixture;
  INSERT INTO public.advisor_fixture SELECT i, repeat('fixture',30)
   FROM generate_series(1,500) i;
  DELETE FROM public.advisor_fixture WHERE id <= 200;
  ANALYZE public.advisor_fixture;
  DO $$ BEGIN
   -- PG15+ flushes pending stats on demand; PG14's collector is async.
   IF to_regproc('pg_catalog.pg_stat_force_next_flush') IS NOT NULL THEN
    PERFORM pg_catalog.pg_stat_force_next_flush();
   END IF;
  END $$`)
	if err != nil {
		t.Fatalf("seed advisor workload: %v", err)
	}
	waitForFixtureDeadTuples(t, pool)
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS public.advisor_fixture"); err != nil {
			t.Errorf("clean advisor workload: %v", err)
		}
	})
}

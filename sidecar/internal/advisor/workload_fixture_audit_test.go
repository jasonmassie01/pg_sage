//go:build integration || e2e

package advisor

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
  SELECT pg_stat_force_next_flush()`)
	if err != nil {
		t.Fatalf("seed advisor workload: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS public.advisor_fixture"); err != nil {
			t.Errorf("clean advisor workload: %v", err)
		}
	})
}

package provideradvice

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: the what-if workload read finds pg_stat_statements in
// schema ext through a session pinned to pg_catalog, pg_temp.
func TestTableWorkloadExtensionOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "provadviceext")
	if _, err := admin.Exec(ctx, "CREATE TABLE public.ext_probe (v int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	const probe = "SELECT count(*) FROM public.ext_probe"
	testdb.SeedStatement(t, admin, pool, probe, 2)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	got, err := (&DBEvaluator{}).tableWorkload(ctx, tx, "ext_probe")
	if err != nil {
		t.Fatalf("tableWorkload: %v", err)
	}
	found := false
	for _, q := range got {
		found = found || q.Text == probe
	}
	if !found {
		t.Fatalf("tableWorkload = %+v, want %q", got, probe)
	}
}

package lint

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: the JSONB analyzer's slow-query read finds
// pg_stat_statements in schema ext through a session pinned to
// pg_catalog, pg_temp.
func TestFetchSlowQueriesExtensionOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "lintext")
	if _, err := admin.Exec(ctx, "CREATE TABLE public.ext_probe (v int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	testdb.SeedStatement(t, admin, pool, "SELECT count(*) FROM public.ext_probe", 2)
	rows, err := (&LLMJsonbAnalyzer{pool: pool}).fetchSlowQueries(ctx, []string{"ext_probe"})
	if err != nil || len(rows) == 0 {
		t.Fatalf("fetchSlowQueries = %+v, %v; want the probe statement", rows, err)
	}
}

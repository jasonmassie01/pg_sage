package selfcost

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: the sampler reads pg_stat_statements(false) and its info
// view in schema ext through a session pinned to pg_catalog, pg_temp.
func TestSamplerExtensionOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "selfcostext")
	testdb.SeedStatement(t, admin, pool,
		"/* pg_sage */ SELECT count(*) FROM pg_catalog.pg_proc", 2)
	out, known, err := NewSampler(nil).Statements(ctx, pool)
	if err != nil || !known {
		t.Fatalf("Statements known=%v err=%v, want a known sample", known, err)
	}
	if len(out) == 0 {
		t.Fatal("Statements found none of pg_sage's tagged statements")
	}
}

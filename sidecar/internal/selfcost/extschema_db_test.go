package selfcost

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: the sampler reads pg_stat_statements(false) and its info
// view in schema ext through a session pinned to pg_catalog, pg_temp.
//
// The statement is seeded as pg_sage's wire tagger sends it, the tag after
// the first keyword: the pinned pool does not tag, and PostgreSQL 18's
// pg_stat_statements drops a comment in front of the first token.
func TestSamplerExtensionOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "selfcostext")
	id := testdb.SeedStatement(t, admin, pool,
		"SELECT /* pg_sage */ count(*) FROM pg_catalog.pg_proc", 2)
	out, known, err := NewSampler(nil).Statements(ctx, pool)
	if err != nil || !known {
		t.Fatalf("Statements known=%v err=%v, want a known sample", known, err)
	}
	for k, c := range out {
		if k.QueryID == id && c.Calls >= 2 {
			return
		}
	}
	t.Fatalf("Statements %v, want the seeded entry %d with 2+ calls", out, id)
}

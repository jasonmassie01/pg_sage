package tuner

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: the tuner's pg_stat_statements readers find the view in
// schema ext through a session pinned to pg_catalog, pg_temp.
func TestTunerReadersFindExtensionOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "tunerext")
	const probe = "SELECT count(*) FROM pg_catalog.pg_attribute"
	id := testdb.SeedStatement(t, admin, pool, probe, 4)
	tu := New(pool, TunerConfig{MinQueryCalls: 1, PlanTimeRatio: 1}, nil, noopLogFn)
	text, err := tu.statementText(ctx, id)
	if err != nil || text != probe {
		t.Fatalf("statementText = %q, %v; want %q", text, err, probe)
	}
	obs, err := tu.fetchQueryStats(ctx, id)
	if err != nil || !obs.Found || obs.Calls < 4 || obs.ResetAt.IsZero() {
		t.Fatalf("fetchQueryStats = %+v, %v; want the probe's calls and a reset time",
			obs, err)
	}
	if _, err := tu.fetchCandidates(ctx); err != nil {
		t.Fatalf("fetchCandidates: %v", err)
	}
}

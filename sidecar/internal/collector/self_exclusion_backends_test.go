package collector

import (
	"context"
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// active_backends and idle_in_transaction count the application's client
// sessions. Backends that are not client sessions also carry the
// database's name in pg_stat_activity and show as active: autovacuum
// workers (they run on the package database between tests; one made
// TestSystemStatsCountApplicationSessionsOnly see 3 active backends on
// PG18 in CI), logical walsenders and parallel workers. A parallel query
// is the deterministic stand-in: one application session, its workers
// active beside it.
func TestSystemStatsCountClientSessionsNotTheirWorkers(t *testing.T) {
	c, _ := sageCollector(t)
	w := selfload.New(t, os.Getenv("SAGE_TEST_DATABASE_URL"))
	leader := w.StartParallel(t)
	s, err := c.collectSystem(context.Background())
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	workers, err := w.Workers(leader)
	if err != nil {
		t.Fatal(err)
	}
	if workers == 0 {
		t.Fatal("the probe's parallel workers ended before the read")
	}
	if s.ActiveBackends != 1 {
		t.Fatalf("active_backends = %d, want 1: the application session, not its %d "+
			"parallel workers", s.ActiveBackends, workers)
	}
	if s.IdleInTransaction != 0 {
		t.Fatalf("idle_in_transaction = %d, want 0", s.IdleInTransaction)
	}
}

package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// A config change's outcome baseline is stamped on the database clock the
// monitor judges its window by: on the host's, a database ahead of it
// would stretch the temp-spill window by the skew (executor-31).
func TestPrepareConfigChange_StampsTheOutcomeBaselineOnTheDatabaseClock(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	e.clock = leadingClock(pool, dbLead)
	before := testdb.MustDBNow(t, pool).Add(dbLead)

	cc, err := e.prepareConfigChange(ctx, "ALTER SYSTEM SET work_mem = '64MB'")

	after := testdb.MustDBNow(t, pool).Add(dbLead)
	if err != nil || cc == nil || cc.outcome == nil {
		t.Fatalf("prepare = %+v, %v; want a work_mem change with an outcome baseline", cc, err)
	}
	if at := cc.outcome.At; at.Before(before) || at.After(after) {
		t.Fatalf("baseline at %s, want the database clock %s..%s (host %s)", at, before,
			after, time.Now())
	}
	if cc.outcome.Metric != metricTempSpills {
		t.Fatalf("baseline metric = %q, want %q", cc.outcome.Metric, metricTempSpills)
	}
}

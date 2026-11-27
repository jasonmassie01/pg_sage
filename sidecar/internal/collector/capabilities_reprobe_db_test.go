package collector

import (
	"context"
	"testing"
)

// firstlook-05: the first look re-check asks the collector to re-probe its
// capabilities at once, so the checklist and the collector agree without
// waiting for the next cycle.
func TestReprobeCapabilities_PicksUpACreatedExtension(t *testing.T) {
	pool := capsDatabase(t)
	ctx := context.Background()
	cfg := testConfig()
	cfg.HasWALColumns, cfg.HasPlanTimeColumns = false, false
	c := New(pool, cfg, serverVersion(t, pool), noopLog)
	if caps := c.ReprobeCapabilities(ctx); caps.StatStatementsView {
		t.Fatalf("capabilities without the extension = %+v, want no view", caps)
	}
	mustExec(t, pool, "CREATE EXTENSION pg_stat_statements")
	caps := c.ReprobeCapabilities(ctx)
	if !caps.StatStatementsView || caps.StatStatementsVersion == "" {
		t.Fatalf("capabilities after CREATE EXTENSION = %+v, want the view", caps)
	}
	if !caps.WALColumns || !caps.PlanTimeColumns {
		t.Fatalf("capabilities after CREATE EXTENSION = %+v, want WAL and plan time "+
			"(pg_stat_statements 1.8 and later)", caps)
	}
	if got := c.Capabilities(); got != caps {
		t.Fatalf("stored capabilities %+v differ from the reprobe's %+v", got, caps)
	}
	mustExec(t, pool, "DROP EXTENSION pg_stat_statements")
	if caps := c.ReprobeCapabilities(ctx); caps.StatStatementsView || caps.WALColumns {
		t.Fatalf("capabilities after DROP EXTENSION = %+v, want no view", caps)
	}
}

// A failed probe keeps the last capabilities (the collector's own rule).
func TestReprobeCapabilities_CancelledKeepsTheLast(t *testing.T) {
	pool := capsDatabase(t)
	mustExec(t, pool, "CREATE EXTENSION pg_stat_statements")
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	before := c.ReprobeCapabilities(context.Background())
	if !before.StatStatementsView {
		t.Fatalf("capabilities = %+v, want the view", before)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := c.ReprobeCapabilities(ctx); got != before {
		t.Fatalf("cancelled reprobe = %+v, want the last %+v", got, before)
	}
}

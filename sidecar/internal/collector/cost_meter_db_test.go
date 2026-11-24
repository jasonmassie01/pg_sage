package collector

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/selfconfig"
)

// footprint-05 on a real server: the meter sees the statements a cycle
// really runs. On a catalog with 5,000 indexes the slowest of them costs
// more than the evidence probe's catalog scan, and the deadline derived
// from the meter covers it; a statement cut off by the deadline is
// counted and its step named.

const costSchema = "fp05_cost"

// seedManyIndexes creates 1,000 tables with five indexes each, in batches
// (one transaction holding 6,000 new relations would overrun the lock
// table).
func seedManyIndexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+costSchema+" CASCADE")
	})
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+costSchema+" CASCADE; "+
		"CREATE SCHEMA "+costSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for batch := 0; batch < 10; batch++ {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DO $$
BEGIN
  FOR i IN %d..%d LOOP
    EXECUTE format('CREATE TABLE %s.t%%s (a int, b int, c int, d int, e int)', i);
    EXECUTE format('CREATE INDEX ON %s.t%%s (a)', i);
    EXECUTE format('CREATE INDEX ON %s.t%%s (b)', i);
    EXECUTE format('CREATE INDEX ON %s.t%%s (c)', i);
    EXECUTE format('CREATE INDEX ON %s.t%%s (d)', i);
    EXECUTE format('CREATE INDEX ON %s.t%%s (e)', i);
  END LOOP;
END $$`, batch*100+1, batch*100+100, costSchema, costSchema, costSchema, costSchema,
			costSchema, costSchema)); err != nil {
			t.Fatalf("fixture batch %d: %v", batch, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relnamespace = $1::regnamespace`, costSchema).Scan(&n); err != nil || n != 5000 {
		t.Fatalf("fixture has %d indexes (%v), want 5000", n, err)
	}
}

func meteredCollector(t *testing.T, pool *pgxpool.Pool, queryTimeoutMs int) *Collector {
	t.Helper()
	cfg := testConfig()
	cfg.Safety.QueryTimeoutMs = queryTimeoutMs
	cfg.Safety.CPUCeilingPct = 100
	return New(pool, cfg, serverVersion(t, pool), noopLog)
}

func asSelfConfigCost(c CycleCost) func() (selfconfig.CollectorCost, bool) {
	return func() (selfconfig.CollectorCost, bool) {
		return selfconfig.CollectorCost{Cycles: c.Cycles, CycleMs: c.P90CycleMs,
			SlowestMs: c.P90SlowestMs, SlowestStep: c.SlowestStep, Timeouts: c.Timeouts}, true
	}
}

// fastestProbe is the quickest of three evidence passes: the probe at its
// best, so a load spike on the shared test machine during one probe does
// not decide the comparison (one run measured 30 ms against a usual 8 ms).
func fastestProbe(t *testing.T, ctx context.Context, pool *pgxpool.Pool) selfconfig.Evidence {
	t.Helper()
	var best selfconfig.Evidence
	for i := 0; i < 3; i++ {
		ev, err := selfconfig.Gather(ctx, pool)
		if err != nil || !ev.CatalogScanMs.Known || !ev.CollectorCycleMs.Known {
			t.Fatalf("probe: %+v (%v)", ev, err)
		}
		if i == 0 || ev.CatalogScanMs.Value < best.CatalogScanMs.Value {
			best = ev
		}
	}
	return best
}

func TestCostMeter_SlowestStatementExceedsTheProbe(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	seedManyIndexes(t, ctx, pool)
	c := meteredCollector(t, pool, 60000)
	c.cycle(ctx, &fakeTicker{})
	cost, ok := c.CycleCost()
	if !ok || cost.Cycles != 1 || cost.Timeouts != 0 {
		t.Fatalf("cost after one cycle = %+v (%v), want one clean cycle", cost, ok)
	}
	probe := fastestProbe(t, ctx, pool)
	t.Logf("slowest collector statement %.1f ms (%s), cycle %.1f ms; probe scan %.1f ms, "+
		"probe cycle estimate %.1f ms", cost.SlowestMs, cost.SlowestStep, cost.P90CycleMs,
		probe.CatalogScanMs.Value, probe.CollectorCycleMs.Value)
	// The interval rule's proxy: the probes' cycle estimate is a small
	// fraction of what a cycle's statements really spend.
	if cost.P90CycleMs <= 5*probe.CollectorCycleMs.Value {
		t.Fatalf("measured cycle %.1f ms <= 5x the probe estimate %.1f ms", cost.P90CycleMs,
			probe.CollectorCycleMs.Value)
	}
	if cost.SlowestMs <= probe.CatalogScanMs.Value {
		t.Fatalf("slowest statement %.1f ms (%s) <= probe scan %.1f ms: the probe "+
			"would size the deadline from a cheaper read than the collector makes",
			cost.SlowestMs, cost.SlowestStep, probe.CatalogScanMs.Value)
	}
	if cost.P90CycleMs < cost.SlowestMs || cost.SlowestStep == "" {
		t.Fatalf("cycle %.1f ms < slowest %.1f ms or no step: %+v", cost.P90CycleMs,
			cost.SlowestMs, cost)
	}
	ev := probe.WithCollectorCost(asSelfConfigCost(cost))
	if ev.Source != selfconfig.SourceCollector {
		t.Fatalf("evidence source %q, want collector", ev.Source)
	}
	r, _ := selfconfig.Lookup("safety.query_timeout_ms")
	p := r.Evaluate(ev, c.cfg)
	if !p.OK || p.Raw == nil || *p.Raw != 3*cost.P90SlowestMs {
		t.Fatalf("derived %+v, want 3x the slowest statement (%.1f ms)", p, cost.P90SlowestMs)
	}
	if p.Value < cost.SlowestMs && p.Value < p.Bounds.Max {
		t.Fatalf("derived deadline %v ms does not cover the slowest statement %.1f ms",
			p.Value, cost.SlowestMs)
	}
}

// Error propagation: a statement held past the deadline by a
// before-statement sleep is a counted timeout naming its step; the step's
// retry then succeeds, so the cycle has exactly one. The deadline is wide
// enough that no other read on a loaded CI server reaches it (at 300 ms,
// statements_usage did); the injected sleep is well past it.
func TestCostMeter_TimeoutIsCountedAndItsStepNamed(t *testing.T) {
	const deadlineMs = 2000
	pool := testPool(t)
	c := meteredCollector(t, pool, deadlineMs)
	var slept atomic.Bool
	ctx := catalogread.WithBeforeStatement(context.Background(),
		func(ctx context.Context, tx pgx.Tx) error {
			if stepOf(ctx) != "foreign_keys" || !slept.CompareAndSwap(false, true) {
				return nil
			}
			_, err := tx.Exec(ctx, "SELECT pg_sleep(5)")
			return err
		})
	c.cycle(ctx, &fakeTicker{})
	cost, ok := c.CycleCost()
	if !ok || !slept.Load() {
		t.Fatalf("cost %+v (%v), hook ran %v", cost, ok, slept.Load())
	}
	if cost.Timeouts != 1 || !slices.Equal(cost.TimeoutSteps, []string{"foreign_keys"}) {
		t.Fatalf("timeouts %d in %v, want 1 in foreign_keys", cost.Timeouts, cost.TimeoutSteps)
	}
	if cost.SlowestMs < deadlineMs*0.97 || cost.SlowestStep != "foreign_keys" {
		t.Fatalf("slowest %.1f ms in %q, want the ~%d ms timed-out read", cost.SlowestMs,
			cost.SlowestStep, deadlineMs)
	}
	snap := c.LatestSnapshot()
	if snap == nil || !snap.Available("foreign_keys") {
		t.Fatalf("the retried foreign_keys read is missing: %+v", snap)
	}
	ev := selfconfig.Evidence{}.WithCollectorCost(asSelfConfigCost(cost))
	r, _ := selfconfig.Lookup("safety.query_timeout_ms")
	p := r.Evaluate(ev, c.cfg)
	if !p.OK || p.Value < 2*deadlineMs {
		t.Fatalf("after a timeout the deadline derived %+v, want at least 2x %d ms", p,
			deadlineMs)
	}
}

// Concurrency on a real server: CycleCost is read while cycles run.
func TestCostMeter_ReadDuringCycles(t *testing.T) {
	pool := testPool(t)
	c := meteredCollector(t, pool, 60000)
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if cost, ok := c.CycleCost(); ok && cost.Cycles > 3 {
					t.Errorf("read %d cycles of 3", cost.Cycles)
				}
			}
		}
	}()
	for i := 0; i < 3; i++ {
		c.cycle(ctx, &fakeTicker{})
	}
	close(stop)
	wg.Wait()
	if cost, ok := c.CycleCost(); !ok || cost.Cycles != 3 || cost.P90CycleMs <= 0 {
		t.Fatalf("after 3 cycles = %+v (%v)", cost, ok)
	}
}

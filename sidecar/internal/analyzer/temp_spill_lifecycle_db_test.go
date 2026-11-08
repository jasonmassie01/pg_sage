package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestTempSpillDurableLifecycle(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	id := time.Now().UnixNano()
	ident := fmt.Sprintf("queryid:%d", id)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE category='temp_spill' AND object_identifier=$1", ident)
	})
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	q := collector.QueryStats{QueryID: id, Query: "SELECT payload FROM public.lifecycle_orders ORDER BY payload", Calls: 10, TempBlksWritten: 1000, TempBlksRead: 1000}
	now := time.Now()
	prev := &collector.Snapshot{CollectedAt: now.Add(-time.Minute), StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{q}}
	q.Calls++
	q.TempBlksWritten += 256
	q.TempBlksRead += 256
	cur := &collector.Snapshot{CollectedAt: now, StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{q}}
	findings := ruleTempSpills(cur, prev, nil, nil)
	a.eval = newCycleEval()
	a.eval.evaluated("temp_spill")
	a.finishTempSpillCycle(ctx, cur, prev, findings)
	a.finalizeCycle(ctx, findings, a.eval.resolvable(findings))
	if countFindingRows(t, pool, "temp_spill", ident, "open") != 1 {
		t.Fatal("real spill finding was not opened")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*collector.Snapshot, *collector.Snapshot)
	}{
		{"idle", func(_, _ *collector.Snapshot) {}},
		{"evicted_or_truncated", func(c, _ *collector.Snapshot) { c.Queries = nil }},
		{"new_baseline_missing", func(_, p *collector.Snapshot) { p.Queries = nil }},
		{"individual_counter_reset", func(c, _ *collector.Snapshot) { c.Queries[0].Calls = 1; c.Queries[0].TempBlksWritten = 0 }},
		{"global_epoch_reset", func(c, _ *collector.Snapshot) { c.StatsEpoch = now }},
		{"unknown_epochs", func(c, p *collector.Snapshot) { c.StatsEpoch = time.Time{}; p.StatsEpoch = time.Time{} }},
		{"small_spill", func(c, _ *collector.Snapshot) { c.Queries[0].Calls++; c.Queries[0].TempBlksWritten++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &collector.Snapshot{CollectedAt: now, StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{q}}
			c := &collector.Snapshot{CollectedAt: now.Add(time.Minute), StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{q}}
			tc.mutate(c, p)
			f := ruleTempSpills(c, p, nil, nil)
			a.eval = newCycleEval()
			a.eval.evaluated("temp_spill")
			a.finishTempSpillCycle(ctx, c, p, f)
			if a.eval.resolvable(f)["temp_spill"] {
				t.Fatal("temp spills entered category-wide resolution")
			}
			a.finalizeCycle(ctx, f, a.eval.resolvable(f))
			if countFindingRows(t, pool, "temp_spill", ident, "open") != 1 {
				t.Fatal("unknown or ongoing spill was falsely resolved")
			}
		})
	}
	p := &collector.Snapshot{CollectedAt: now, StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{q}}
	q.Calls++
	c := &collector.Snapshot{CollectedAt: now.Add(time.Minute), StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{q}}
	a.eval = newCycleEval()
	a.finishTempSpillCycle(ctx, c, p, nil)
	a.finalizeCycle(ctx, nil, a.eval.resolvable(nil))
	if countFindingRows(t, pool, "temp_spill", ident, "resolved") != 1 {
		t.Fatal("new observed zero-spill execution did not resolve finding")
	}
}

package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestTempSpillEvidenceAndGuards(t *testing.T) {
	now := time.Now()
	base := collector.QueryStats{QueryID: 42, Query: "SELECT payload FROM public.orders ORDER BY payload", Calls: 10, TempBlksWritten: 1000, TempBlksRead: 900}
	for _, tc := range []struct {
		name   string
		mutate func(*collector.Snapshot, *collector.Snapshot)
		want   int
	}{
		{"new_spill", func(_, _ *collector.Snapshot) {}, 1},
		{"old_spill_only", func(c, p *collector.Snapshot) { c.Queries[0] = p.Queries[0] }, 0},
		{"below_noise_floor", func(c, _ *collector.Snapshot) { c.Queries[0].TempBlksWritten = 1127 }, 0},
		{"new_query_no_baseline", func(_, p *collector.Snapshot) { p.Queries = nil }, 0},
		{"counter_reset", func(c, _ *collector.Snapshot) { c.Queries[0].TempBlksWritten = 400 }, 0},
		{"calls_reset", func(c, _ *collector.Snapshot) { c.Queries[0].Calls = 1 }, 0},
		{"epoch_changed", func(c, _ *collector.Snapshot) { c.StatsEpoch = now }, 0},
		{"epoch_unknown", func(c, p *collector.Snapshot) { c.StatsEpoch = time.Time{}; p.StatsEpoch = time.Time{} }, 0},
		{"reset_flag", func(c, _ *collector.Snapshot) { c.StatsReset = true }, 0},
		{"unavailable", func(c, _ *collector.Snapshot) { c.Unavailable = map[string]string{"queries": "permission denied"} }, 0},
		{"out_of_order", func(c, p *collector.Snapshot) { c.CollectedAt = p.CollectedAt }, 0},
		{"own_monitoring_query", func(c, _ *collector.Snapshot) { c.Queries[0].Query = "/* pg_sage */ SELECT * FROM pg_stat_activity" }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &collector.Snapshot{CollectedAt: now.Add(-time.Minute), StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{base}}
			q := base
			q.Calls++
			q.TempBlksWritten += 256
			q.TempBlksRead += 256
			c := &collector.Snapshot{CollectedAt: now, StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{q}}
			tc.mutate(c, p)
			got := ruleTempSpills(c, p, nil, nil)
			if len(got) != tc.want {
				t.Fatalf("findings=%v, want %d", got, tc.want)
			}
			if tc.want == 1 {
				f := got[0]
				if f.RecommendedSQL != "" || f.RollbackSQL != "" || f.Detail["active_hint_validated"] != false || f.Detail["delta_temp_blks_written"] != int64(256) {
					t.Fatalf("spill advice must be factual and non-executable: %+v", f)
				}
				for _, text := range []string{"SET LOCAL work_mem", "parallel worker", "No active hint", "Lakebase"} {
					if !strings.Contains(f.Recommendation, text) {
						t.Errorf("missing advice %q", text)
					}
				}
			}
		})
	}
	if hasTempSpillWindow(nil, nil) {
		t.Fatal("nil snapshots must be unknown")
	}
}

package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// collection-06: the cache rule reads the interval ratio, not the
// lifetime one, with a floor of blocks accessed.

func ivPtrF(v float64) *float64 { return &v }
func ivPtrI(v int64) *int64     { return &v }

func cacheSnap(lifetime float64, hits, reads int64) *collector.Snapshot {
	s := &collector.Snapshot{}
	s.System.CacheHitRatio = lifetime
	total := hits + reads
	iv := &collector.SystemInterval{Seconds: 60, BlksAccessed: ivPtrI(total)}
	if total > 0 {
		iv.CacheHitRatio = ivPtrF(float64(hits) / float64(total))
	}
	s.System.Interval = iv
	return s
}

func cacheCfg() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Analyzer.CacheHitRatioWarning = 0.95
	return cfg
}

// CHECK-29: lifetime 0.995 hides an interval of 0.80 over 50k blocks.
func TestRuleCacheHitRatio_IntervalDegradationLifetimeHides(t *testing.T) {
	cfg := cacheCfg()
	snap := cacheSnap(0.995, 40_000, 10_000)
	if !hasCacheRatio(snap, nil) {
		t.Fatal("hasCacheRatio = false with a 50k-block interval")
	}
	got := ruleCacheHitRatio(snap, nil, cfg, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1 (interval 0.80 under threshold 0.95)", len(got))
	}
	f := got[0]
	if f.Category != "cache_hit_ratio" || f.ObjectIdentifier != "buffer_cache" {
		t.Errorf("finding = %s/%s", f.Category, f.ObjectIdentifier)
	}
	if f.Severity != "warning" {
		t.Errorf("severity %q at exactly 0.80, want warning (critical is below 0.80)",
			f.Severity)
	}
	if !strings.Contains(f.Title, "80.00%") || !strings.Contains(f.Title, "60s") {
		t.Errorf("title %q lacks the interval ratio and window", f.Title)
	}
	if f.Detail["cache_hit_ratio"] != 0.8 || f.Detail["lifetime_cache_hit_ratio"] != 0.995 {
		t.Errorf("detail ratios = %v / %v, want 0.8 interval and 0.995 lifetime",
			f.Detail["cache_hit_ratio"], f.Detail["lifetime_cache_hit_ratio"])
	}
	if f.Detail["blocks_accessed"] != int64(50_000) || f.Detail["interval_seconds"] != 60.0 {
		t.Errorf("detail window = %v blocks over %v s", f.Detail["blocks_accessed"],
			f.Detail["interval_seconds"])
	}
	critical := ruleCacheHitRatio(cacheSnap(0.995, 37_500, 12_500), nil, cfg, nil)
	if len(critical) != 1 || critical[0].Severity != "critical" {
		t.Fatalf("interval 0.75: findings %+v, want one critical", critical)
	}
}

// Boundary: the 10,000-block floor; 500 blocks prove nothing.
func TestRuleCacheHitRatio_BlockFloor(t *testing.T) {
	cfg := cacheCfg()
	cases := []struct {
		hits, reads int64
		want        int
	}{
		{400, 100, 0},
		{7_999, 2_000, 0},
		{8_000, 2_000, 1},
	}
	for _, tc := range cases {
		snap := cacheSnap(0.995, tc.hits, tc.reads)
		got := ruleCacheHitRatio(snap, nil, cfg, nil)
		if len(got) != tc.want {
			t.Errorf("%d blocks: findings %d, want %d", tc.hits+tc.reads, len(got), tc.want)
		}
		if hasCacheRatio(snap, nil) != (tc.want == 1) {
			t.Errorf("%d blocks: hasCacheRatio = %v", tc.hits+tc.reads, !(tc.want == 1))
		}
	}
}

// A healthy interval under a poor lifetime ratio clears the finding.
func TestRuleCacheHitRatio_HealthyIntervalPoorLifetime(t *testing.T) {
	snap := cacheSnap(0.60, 99_000, 1_000)
	if !hasCacheRatio(snap, nil) {
		t.Fatal("hasCacheRatio = false with a measured interval")
	}
	if got := ruleCacheHitRatio(snap, nil, cacheCfg(), nil); len(got) != 0 {
		t.Fatalf("findings = %d, want 0 for interval 0.99", len(got))
	}
}

// Nil/zero: no interval (first cycle after a restart), an unknown ratio
// (no blocks) or a reset interval is not evaluated.
func TestRuleCacheHitRatio_NoIntervalNotEvaluated(t *testing.T) {
	noInterval := &collector.Snapshot{}
	noInterval.System.CacheHitRatio = 0.50
	zeroBlocks := cacheSnap(0.50, 0, 0)
	reset := cacheSnap(0.50, 0, 0)
	reset.System.Interval.BlksAccessed = nil
	for name, snap := range map[string]*collector.Snapshot{
		"no_interval": noInterval, "zero_blocks": zeroBlocks, "reset": reset,
	} {
		if hasCacheRatio(snap, nil) {
			t.Errorf("%s: hasCacheRatio = true, want false", name)
		}
		if got := ruleCacheHitRatio(snap, nil, cacheCfg(), nil); len(got) != 0 {
			t.Errorf("%s: findings = %d, want 0", name, len(got))
		}
	}
}

// The lifetime value in the detail is normalized, and absent when unknown.
func TestRuleCacheHitRatio_LifetimeDetailUnknown(t *testing.T) {
	snap := cacheSnap(collector.CacheHitRatioUnknown, 8_000, 2_000)
	got := ruleCacheHitRatio(snap, nil, cacheCfg(), nil)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	if v, ok := got[0].Detail["lifetime_cache_hit_ratio"]; !ok || v != nil {
		t.Errorf("lifetime detail = %v (present %v), want nil", v, ok)
	}
	legacy := cacheSnap(99.5, 8_000, 2_000)
	got = ruleCacheHitRatio(legacy, nil, cacheCfg(), nil)
	if len(got) != 1 || got[0].Detail["lifetime_cache_hit_ratio"] != 0.995 {
		t.Fatalf("legacy percent lifetime detail = %+v, want 0.995", got)
	}
}

func ioSnap(
	epoch time.Time, ioRead, ioWrite *float64, execs map[int64]float64,
) *collector.Snapshot {
	s := &collector.Snapshot{StatsEpoch: epoch}
	s.System.Interval = &collector.SystemInterval{Seconds: 60,
		BlkReadTimeMs: ioRead, BlkWriteTimeMs: ioWrite}
	for id, total := range execs {
		s.Queries = append(s.Queries, collector.QueryStats{QueryID: id, TotalExecTime: total})
	}
	return s
}

var ioEpoch = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// computeIOUtilPct divides interval I/O time by the interval's exec time
// of the queries present in both snapshots.
func TestComputeIOUtilPct_IntervalOverExecDelta(t *testing.T) {
	prev := ioSnap(ioEpoch, nil, nil, map[int64]float64{1: 10_000, 2: 5_000, 3: 900})
	cur := ioSnap(ioEpoch, ivPtrF(300), ivPtrF(100),
		map[int64]float64{1: 10_600, 2: 5_200, 4: 50_000})
	// (300+100) / ((10600-10000) + (5200-5000)) = 400/800; query 4 is new
	// and query 3 left the list, so neither counts.
	if got := computeIOUtilPct(cur, prev); got != 50 {
		t.Fatalf("io util = %v, want 50", got)
	}
	capped := ioSnap(ioEpoch, ivPtrF(5_000), ivPtrF(0), map[int64]float64{1: 10_100})
	if got := computeIOUtilPct(capped, prev); got != 100 {
		t.Fatalf("io util = %v, want capped at 100", got)
	}
}

// Unknown inputs give 0: no previous, no interval I/O, a moved or unknown
// statements epoch, a reset, or no exec time in the interval.
func TestComputeIOUtilPct_UnknownIsZero(t *testing.T) {
	prev := ioSnap(ioEpoch, nil, nil, map[int64]float64{1: 1_000})
	good := func() *collector.Snapshot {
		return ioSnap(ioEpoch, ivPtrF(500), ivPtrF(0), map[int64]float64{1: 2_000})
	}
	if got := computeIOUtilPct(good(), prev); got != 50 {
		t.Fatalf("control io util = %v, want 50", got)
	}
	noInterval := good()
	noInterval.System.Interval = nil
	nilIO := good()
	nilIO.System.Interval.BlkReadTimeMs = nil
	moved := good()
	moved.StatsEpoch = ioEpoch.Add(time.Hour)
	unknown := good()
	unknown.StatsEpoch = time.Time{}
	reset := good()
	reset.StatsReset = true
	decreased := ioSnap(ioEpoch, ivPtrF(500), ivPtrF(0), map[int64]float64{1: 100})
	cases := map[string]*collector.Snapshot{
		"no_interval": noInterval, "nil_io": nilIO, "epoch_moved": moved,
		"epoch_unknown": unknown, "stats_reset": reset, "exec_decreased": decreased,
	}
	for name, cur := range cases {
		if got := computeIOUtilPct(cur, prev); got != 0 {
			t.Errorf("%s: io util = %v, want 0", name, got)
		}
	}
	if got := computeIOUtilPct(good(), nil); got != 0 {
		t.Errorf("no previous: io util = %v, want 0", got)
	}
	if got := computeIOUtilPct(nil, prev); got != 0 {
		t.Errorf("nil current: io util = %v, want 0", got)
	}
}

// No concurrent access tests: these rules are pure functions over two
// immutable snapshots.

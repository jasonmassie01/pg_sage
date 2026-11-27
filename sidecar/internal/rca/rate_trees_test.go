package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rca-03: the cache hit ratio, the cache tree, idle in transaction and the
// connection storm read what happened in the interval, not cumulative
// counters or averages.
//
// No concurrent access test: these detectors and trees are pure functions
// of the two snapshots, run under e.mu.

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// cacheSnap carries an interval cache hit ratio over blocks accessed.
func cacheSnap(at time.Time, lifetime, interval float64, blocks int64) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: at,
		System: collector.SystemStats{MaxConnections: 100, CacheHitRatio: lifetime,
			Interval: &collector.SystemInterval{Seconds: 60,
				CacheHitRatio: f64(interval), BlksAccessed: i64(blocks)}}}
}

// Happy path: a healthy lifetime ratio hides a bad minute; the interval
// ratio fires (critical: more than 5 points under the threshold).
func TestCacheHitDrop_UsesIntervalRatio(t *testing.T) {
	sig := newTestEngine().detectCacheHitDrop(
		cacheSnap(time.Now(), 0.999, 0.80, 50_000), nil, defaultCfg())
	if sig == nil {
		t.Fatal("interval ratio 0.80 did not fire")
	}
	if r := floatMetric(sig, "cache_hit_ratio"); r != 0.80 {
		t.Errorf("cache_hit_ratio = %v, want the interval's 0.80", r)
	}
	if sig.Severity != "critical" {
		t.Errorf("severity = %q, want critical", sig.Severity)
	}
	if b := intMetric(sig, "blocks_accessed"); b != 50_000 {
		t.Errorf("blocks_accessed = %d, want 50000", b)
	}
}

// A low lifetime ratio (an old cold start) with a healthy interval does
// not fire.
func TestCacheHitDrop_LowLifetimeHealthyIntervalQuiet(t *testing.T) {
	sig := newTestEngine().detectCacheHitDrop(
		cacheSnap(time.Now(), 0.50, 0.99, 50_000), nil, defaultCfg())
	if sig != nil {
		t.Errorf("fired on the lifetime ratio: %+v", sig.Metrics)
	}
}

// Nil: across a reset the interval (or its ratio) is nil: no signal,
// whatever the lifetime ratio says.
func TestCacheHitDrop_NilIntervalAcrossReset(t *testing.T) {
	eng := newTestEngine()
	noInterval := cacheSnap(time.Now(), 0.50, 0, 0)
	noInterval.System.Interval = nil
	noRatio := cacheSnap(time.Now(), 0.50, 0, 0)
	noRatio.System.Interval.CacheHitRatio = nil
	noBlocks := cacheSnap(time.Now(), 0.50, 0.5, 0)
	noBlocks.System.Interval.BlksAccessed = nil
	for name, s := range map[string]*collector.Snapshot{
		"no interval": noInterval, "no ratio": noRatio, "no block count": noBlocks,
	} {
		if sig := eng.detectCacheHitDrop(s, nil, defaultCfg()); sig != nil {
			t.Errorf("%s: fired %+v", name, sig.Metrics)
		}
	}
}

// Boundary: a quiet interval under 10,000 blocks is not evidence; at
// 10,000 it is. A ratio of 0 (every block a miss) is real, not "no data".
func TestCacheHitDrop_BlockFloorAndZeroRatio(t *testing.T) {
	eng := newTestEngine()
	if sig := eng.detectCacheHitDrop(cacheSnap(time.Now(), 0.99, 0.5, 9_999),
		nil, defaultCfg()); sig != nil {
		t.Errorf("9,999 blocks fired: %+v", sig.Metrics)
	}
	if eng.detectCacheHitDrop(cacheSnap(time.Now(), 0.99, 0.5, 10_000),
		nil, defaultCfg()) == nil {
		t.Error("10,000 blocks at 0.5 did not fire")
	}
	sig := eng.detectCacheHitDrop(cacheSnap(time.Now(), 0.99, 0, 20_000), nil, defaultCfg())
	if sig == nil || sig.Severity != "critical" {
		t.Errorf("all-miss interval: %+v, want a critical signal", sig)
	}
}

// Boundary: exactly at the threshold is healthy, just under is a warning.
func TestCacheHitDrop_ThresholdBoundary(t *testing.T) {
	eng := newTestEngine()
	if sig := eng.detectCacheHitDrop(cacheSnap(time.Now(), 0.99, 0.95, 20_000),
		nil, defaultCfg()); sig != nil {
		t.Errorf("0.95 fired: %+v", sig.Metrics)
	}
	sig := eng.detectCacheHitDrop(cacheSnap(time.Now(), 0.99, 0.9499, 20_000), nil, defaultCfg())
	if sig == nil || sig.Severity != "warning" {
		t.Errorf("0.9499: %+v, want a warning", sig)
	}
}

// cacheTreePair is two statement snapshots under one epoch.
func cacheTreePair(prevQ, currQ collector.QueryStats) (*collector.Snapshot, *collector.Snapshot) {
	now := time.Now().UTC()
	epoch := now.Add(-24 * time.Hour)
	prev := cacheSnap(now.Add(-time.Minute), 0.99, 0.99, 50_000)
	prev.StatsEpoch, prev.Queries = epoch, []collector.QueryStats{prevQ}
	curr := cacheSnap(now, 0.99, 0.80, 50_000)
	curr.StatsEpoch, curr.Queries = epoch, []collector.QueryStats{currQ}
	return prev, curr
}

func cacheSig(now time.Time) *Signal {
	return &Signal{ID: "cache_hit_ratio_drop", FiredAt: now, Severity: "critical",
		Metrics: map[string]any{"cache_hit_ratio": 0.80}}
}

func q(id, calls, reads int64) collector.QueryStats {
	return collector.QueryStats{QueryID: id, Calls: calls, SharedBlksRead: reads,
		Query: "SELECT * FROM big"}
}

func blamesQuery(inc Incident, id string) bool {
	return strings.Contains(inc.RootCause, "Query "+id)
}

// Happy path: 150 reads per call this interval against 10 per call before
// names the query. The old tree compared cumulative reads (25,000 against
// 10,000, under 10x) and blamed the working set.
func TestCacheTree_PerCallSpikeNamesQuery(t *testing.T) {
	prev, curr := cacheTreePair(q(7, 1000, 10_000), q(7, 1100, 25_000))
	inc := newTestEngine().treeCacheHitDrop(cacheSig(curr.CollectedAt), curr, prev)
	if !blamesQuery(inc, "7") {
		t.Fatalf("root cause %q, want query 7", inc.RootCause)
	}
	ev := inc.CausalChain[0].Evidence
	if !strings.Contains(ev, "150 blks/call") || !strings.Contains(ev, "10 blks/call") {
		t.Errorf("evidence %q, want this interval's 150 and the earlier 10 blks/call", ev)
	}
}

// Cumulative growth at a steady per-call rate is not a spike. The old
// tree blamed the query: 1,100 cumulative reads > 10 x 100.
func TestCacheTree_CumulativeGrowthIsNotASpike(t *testing.T) {
	prev, curr := cacheTreePair(q(7, 100, 100), q(7, 1100, 1100))
	inc := newTestEngine().treeCacheHitDrop(cacheSig(curr.CollectedAt), curr, prev)
	if blamesQuery(inc, "7") || !strings.Contains(inc.RootCause, "Working set") {
		t.Errorf("root cause %q, want the working set", inc.RootCause)
	}
}

// Boundary: exactly 10x per call is not a spike; just over is.
func TestCacheTree_TenTimesBoundary(t *testing.T) {
	prev, curr := cacheTreePair(q(7, 100, 1000), q(7, 200, 1000+100*100))
	if inc := newTestEngine().treeCacheHitDrop(cacheSig(curr.CollectedAt),
		curr, prev); blamesQuery(inc, "7") {
		t.Errorf("exactly 10x blamed the query: %q", inc.RootCause)
	}
	prev, curr = cacheTreePair(q(7, 100, 1000), q(7, 200, 1000+100*100+1))
	if inc := newTestEngine().treeCacheHitDrop(cacheSig(curr.CollectedAt),
		curr, prev); !blamesQuery(inc, "7") {
		t.Errorf("just over 10x did not blame the query: %q", inc.RootCause)
	}
}

// The largest reader of the interval is named when several spike.
func TestCacheTree_NamesLargestIntervalReader(t *testing.T) {
	prev, curr := cacheTreePair(q(7, 100, 100), q(7, 110, 100+10*50))
	prev.Queries = append(prev.Queries, q(9, 100, 100))
	curr.Queries = append(curr.Queries, q(9, 200, 100+100*20))
	inc := newTestEngine().treeCacheHitDrop(cacheSig(curr.CollectedAt), curr, prev)
	if !blamesQuery(inc, "9") {
		t.Errorf("root cause %q, want query 9 (2,000 interval reads vs 500)", inc.RootCause)
	}
}

// State transition: a moved or unknown StatsEpoch, or a flagged reset,
// makes the counters incomparable: no query is blamed.
func TestCacheTree_IncomparableEpochBlamesNoQuery(t *testing.T) {
	for name, mutate := range map[string]func(prev, curr *collector.Snapshot){
		"moved epoch": func(_, c *collector.Snapshot) {
			c.StatsEpoch = c.StatsEpoch.Add(time.Hour)
		},
		"unknown epoch": func(p, c *collector.Snapshot) {
			p.StatsEpoch, c.StatsEpoch = time.Time{}, time.Time{}
		},
		"flagged reset": func(_, c *collector.Snapshot) { c.StatsReset = true },
	} {
		prev, curr := cacheTreePair(q(7, 1000, 10_000), q(7, 1100, 25_000))
		mutate(prev, curr)
		inc := newTestEngine().treeCacheHitDrop(cacheSig(curr.CollectedAt), curr, prev)
		if blamesQuery(inc, "7") {
			t.Errorf("%s: blamed query 7: %q", name, inc.RootCause)
		}
	}
}

// Invalid and zero input: no calls this interval, no calls or reads
// before, and counters that fell (an evicted, re-entered statement) blame
// no query; nor does a missing previous snapshot.
func TestCacheTree_ZeroOrFallingCountersBlameNoQuery(t *testing.T) {
	cases := map[string][2]collector.QueryStats{
		"no calls this interval": {q(7, 1000, 10_000), q(7, 1000, 90_000)},
		"no earlier calls":       {q(7, 0, 0), q(7, 100, 90_000)},
		"no earlier reads":       {q(7, 1000, 0), q(7, 1100, 90_000)},
		"reads fell":             {q(7, 1000, 10_000), q(7, 1100, 5_000)},
		"calls fell":             {q(7, 1000, 10_000), q(7, 10, 90_000)},
	}
	for name, c := range cases {
		prev, curr := cacheTreePair(c[0], c[1])
		inc := newTestEngine().treeCacheHitDrop(cacheSig(curr.CollectedAt), curr, prev)
		if blamesQuery(inc, "7") {
			t.Errorf("%s: blamed query 7: %q", name, inc.RootCause)
		}
	}
	_, curr := cacheTreePair(q(7, 1, 1), q(7, 1000, 90_000))
	if inc := newTestEngine().treeCacheHitDrop(cacheSig(curr.CollectedAt),
		curr, nil); blamesQuery(inc, "7") {
		t.Errorf("no previous snapshot blamed query 7: %q", inc.RootCause)
	}
}

// idleSnap has count idle-in-transaction sessions, the longest idle for
// maxSec.
func idleSnap(count int, maxSec float64) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: time.Now(),
		System: collector.SystemStats{MaxConnections: 100, CacheHitRatio: 0.999,
			IdleInTransaction: count, IdleInTxMaxSeconds: maxSec}}
}

// Happy path: one session idle for an hour among 50 short ones fires. The
// old detector read the 72 s average (and called it the maximum).
func TestIdleInTx_OneLongAmongManyShortFires(t *testing.T) {
	sig := newTestEngine().detectIdleInTxElevated(idleSnap(51, 3600), defaultCfg())
	if sig == nil {
		t.Fatal("one 3600 s idle-in-transaction session did not fire")
	}
	if v := floatMetric(sig, "max_duration_seconds"); v != 3600 {
		t.Errorf("max_duration_seconds = %v, want 3600", v)
	}
	if v := intMetric(sig, "idle_in_tx_count"); v != 51 {
		t.Errorf("idle_in_tx_count = %d, want 51", v)
	}
}

// Boundary: the longest idle exactly at the 5 minute threshold fires, one
// second under it does not.
func TestIdleInTx_ThresholdBoundary(t *testing.T) {
	eng := newTestEngine()
	if sig := eng.detectIdleInTxElevated(idleSnap(3, 299), defaultCfg()); sig != nil {
		t.Errorf("299 s fired: %+v", sig.Metrics)
	}
	if eng.detectIdleInTxElevated(idleSnap(3, 300), defaultCfg()) == nil {
		t.Error("300 s did not fire")
	}
}

// Zero: no idle-in-transaction session gives no signal whatever the
// duration says, and the read needs no configuration snapshot (it is
// collected without an LLM advisor too).
func TestIdleInTx_NoSessionsNoSignal(t *testing.T) {
	eng := newTestEngine()
	if sig := eng.detectIdleInTxElevated(idleSnap(0, 9999), defaultCfg()); sig != nil {
		t.Errorf("zero sessions fired: %+v", sig.Metrics)
	}
	s := idleSnap(2, 900)
	s.ConfigData = nil
	if eng.detectIdleInTxElevated(s, defaultCfg()) == nil {
		t.Error("no configuration snapshot suppressed the signal")
	}
}

// churnSnap is a saturated snapshot whose interval opened sessions over
// seconds; sessions nil is an unknown interval.
func churnSnap(at time.Time, seconds float64, sessions *int64) *collector.Snapshot {
	s := &collector.Snapshot{CollectedAt: at,
		System: collector.SystemStats{TotalBackends: 90, ClientBackends: 90,
			MaxConnections: 100, CacheHitRatio: 0.999},
		ConfigData: &collector.ConfigSnapshot{}}
	if seconds > 0 {
		s.System.Interval = &collector.SystemInterval{Seconds: seconds, Sessions: sessions}
	}
	return s
}

func connTree(t *testing.T, prev, curr *collector.Snapshot) Incident {
	t.Helper()
	eng := newTestEngine()
	sig := eng.detectConnectionsHigh(curr, prev, defaultCfg())
	if sig == nil {
		t.Fatal("connections_high did not fire on 90/100")
	}
	return eng.treeConnectionsHigh(sig, map[string]*Signal{sig.ID: sig}, curr, prev, defaultCfg())
}

func isStorm(inc Incident) bool { return strings.Contains(inc.RootCause, "Connection storm") }

// Happy path: 500 sessions opened this minute against 100 the minute
// before is a storm, although the sessions that survive (churn 3) did not
// move: closed connections are counted by pg_stat_database.sessions.
func TestConnectionTree_StormFromSessionsDelta(t *testing.T) {
	now := time.Now()
	inc := connTree(t, churnSnap(now.Add(-time.Minute), 60, i64(100)),
		churnSnap(now, 60, i64(500)))
	if !isStorm(inc) {
		t.Fatalf("root cause %q, want a connection storm", inc.RootCause)
	}
	ev := inc.CausalChain[0].Evidence
	if !strings.Contains(ev, "500/min") || !strings.Contains(ev, "100/min") {
		t.Errorf("evidence %q, want both session rates per minute", ev)
	}
}

// Boundary: the rate is per minute, so a longer interval is normalised
// (300 in 120 s is 150/min, 1.5x); exactly 2x is not a storm.
func TestConnectionTree_StormRateBoundary(t *testing.T) {
	now := time.Now()
	if inc := connTree(t, churnSnap(now.Add(-time.Minute), 60, i64(100)),
		churnSnap(now, 120, i64(300))); isStorm(inc) {
		t.Errorf("150/min against 100/min is a storm: %q", inc.RootCause)
	}
	if inc := connTree(t, churnSnap(now.Add(-time.Minute), 60, i64(100)),
		churnSnap(now, 60, i64(200))); isStorm(inc) {
		t.Errorf("exactly 2x is a storm: %q", inc.RootCause)
	}
	if inc := connTree(t, churnSnap(now.Add(-time.Minute), 60, i64(100)),
		churnSnap(now, 60, i64(201))); !isStorm(inc) {
		t.Errorf("just over 2x is not a storm: %q", inc.RootCause)
	}
}

// Nil and zero: an unknown interval on either side, or no session opened
// the interval before, claims no storm.
func TestConnectionTree_UnknownOrZeroSessionsNoStorm(t *testing.T) {
	now := time.Now()
	cases := map[string][2]*collector.Snapshot{
		"no previous interval": {churnSnap(now.Add(-time.Minute), 0, nil),
			churnSnap(now, 60, i64(500))},
		"previous sessions unknown": {churnSnap(now.Add(-time.Minute), 60, nil),
			churnSnap(now, 60, i64(500))},
		"current sessions unknown": {churnSnap(now.Add(-time.Minute), 60, i64(100)),
			churnSnap(now, 60, nil)},
		"zero sessions before": {churnSnap(now.Add(-time.Minute), 60, i64(0)),
			churnSnap(now, 60, i64(500))},
	}
	for name, c := range cases {
		if inc := connTree(t, c[0], c[1]); isStorm(inc) {
			t.Errorf("%s: storm claimed: %q", name, inc.RootCause)
		}
	}
	if inc := connTree(t, nil, churnSnap(now, 60, i64(500))); isStorm(inc) {
		t.Errorf("no previous snapshot: storm claimed: %q", inc.RootCause)
	}
}

package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// rca-03: wal_growth_spike compares the cluster's WAL rate (the insert
// LSN's advance per minute) of this interval with the previous interval's.
// The old detector divided sums of cumulative pg_stat_statements.wal_bytes,
// which stays near 1.0 in steady state, doubles on young counters after a
// reset and moves with the top-N set.
//
// The collector measures each interval's WAL advance (WALBytes over
// WALSeconds, TestSystemInterval_WAL*); the detector compares the
// intervals that ended at prev and at curr.
//
// No concurrent access test: the detector keeps no state; it reads the
// two snapshots it is given.

// walSnap is a primary's first snapshot at at: no interval before it.
func walSnap(at time.Time) *collector.Snapshot {
	return &collector.Snapshot{
		CollectedAt: at,
		System:      collector.SystemStats{MaxConnections: 100, CacheHitRatio: 0.999},
	}
}

// walIvSnap is a snapshot at at whose collector interval (one minute)
// wrote n bytes of WAL.
func walIvSnap(at time.Time, n int64) *collector.Snapshot {
	s := walSnap(at)
	s.System.Interval = &collector.SystemInterval{Seconds: 60, WALBytes: &n, WALSeconds: 60}
	return s
}

// walRun feeds the detector one snapshot a minute with the given WAL
// bytes written in each minute, after a first snapshot with no interval.
// It returns the signal of every pair: perMin[i] is the interval ending
// at snapshot i+1.
func walRun(t *testing.T, eng *Engine, perMin ...int64) []*Signal {
	t.Helper()
	return walRunCfg(t, eng, defaultCfg(), perMin...)
}

func walRunCfg(t *testing.T, eng *Engine, cfg *config.Config, perMin ...int64) []*Signal {
	t.Helper()
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	prev := walSnap(start)
	out := make([]*Signal, 0, len(perMin))
	for i, n := range perMin {
		curr := walIvSnap(start.Add(time.Duration(i+1)*time.Minute), n)
		out = append(out, eng.detectWALSpike(curr, prev, cfg))
		prev = curr
	}
	return out
}

func firedAt(sigs []*Signal) []int {
	var idx []int
	for i, s := range sigs {
		if s != nil {
			idx = append(idx, i)
		}
	}
	return idx
}

// Happy path: steady 10 MiB/min, then 100 MiB/min fires on that interval.
func TestWALSpike_SteadyThenTenfoldFires(t *testing.T) {
	sigs := walRun(t, newTestEngine(), 10*mib, 10*mib, 10*mib, 100*mib)
	if got := firedAt(sigs); len(got) != 1 || got[0] != 3 {
		t.Fatalf("fired at %v, want only the 100 MiB/min interval (3)", got)
	}
	s := sigs[3]
	if s.ID != "wal_growth_spike" || s.Severity != "warning" {
		t.Errorf("signal %q/%q, want wal_growth_spike/warning", s.ID, s.Severity)
	}
	if r := floatMetric(s, "ratio"); r != 10 {
		t.Errorf("ratio = %v, want 10", r)
	}
	if v := int64(intMetric(s, "current_wal_bytes_per_min")); v != 100*mib {
		t.Errorf("current_wal_bytes_per_min = %d, want %d", v, 100*mib)
	}
	if v := int64(intMetric(s, "previous_wal_bytes_per_min")); v != 10*mib {
		t.Errorf("previous_wal_bytes_per_min = %d, want %d", v, 10*mib)
	}
	if v := floatMetric(s, "multiplier"); v != 2 {
		t.Errorf("multiplier = %v, want 2", v)
	}
}

// Steady load with +-30% jitter never fires (CHECK-20, unit form): the
// rate ratio stays under 2 although every cumulative counter grows.
func TestWALSpike_SteadyLoadTwentyPairsNeverFire(t *testing.T) {
	var load []int64
	for i := 0; i < 20; i++ {
		load = append(load, 10*mib+int64(i%3-1)*3*mib)
	}
	if got := firedAt(walRun(t, newTestEngine(), load...)); len(got) != 0 {
		t.Fatalf("steady load fired at %v, want none", got)
	}
}

// Boundary: a ratio exactly at wal_spike_multiplier fires, one byte under
// it does not.
func TestWALSpike_RatioAtMultiplierBoundary(t *testing.T) {
	at := walRun(t, newTestEngine(), 10*mib, 20*mib)
	if at[1] == nil {
		t.Fatal("ratio exactly 2.0 did not fire")
	}
	if r := floatMetric(at[1], "ratio"); r != 2 {
		t.Errorf("ratio = %v, want exactly 2", r)
	}
	under := walRun(t, newTestEngine(), 10*mib, 20*mib-1)
	if under[1] != nil {
		t.Errorf("ratio just under 2.0 fired: %+v", under[1].Metrics)
	}
}

// Boundary: the configured multiplier is honoured (3x: 2.5x is quiet).
func TestWALSpike_ConfiguredMultiplier(t *testing.T) {
	cfg := defaultCfg()
	cfg.RCA.WALSpikeMultiplier = 3
	sigs := walRunCfg(t, newTestEngine(), cfg, 10*mib, 25*mib, 75*mib)
	if sigs[1] != nil {
		t.Errorf("2.5x fired under multiplier 3: %+v", sigs[1].Metrics)
	}
	if sigs[2] == nil {
		t.Error("3x did not fire under multiplier 3")
	} else if v := floatMetric(sigs[2], "multiplier"); v != 3 {
		t.Errorf("multiplier = %v, want 3", v)
	}
}

// Nil: no previous snapshot, and the first interval (no previous rate)
// give no signal however large.
func TestWALSpike_NoPreviousSnapshotOrRate(t *testing.T) {
	eng := newTestEngine()
	if sig := eng.detectWALSpike(walIvSnap(time.Now(), 900*mib), nil, defaultCfg()); sig != nil {
		t.Fatalf("nil previous fired: %+v", sig.Metrics)
	}
	if got := firedAt(walRun(t, newTestEngine(), 900*mib)); len(got) != 0 {
		t.Fatalf("the first interval fired at %v, want none", got)
	}
}

// Zero: a zero previous rate (an idle minute) gives no ratio and no signal.
func TestWALSpike_ZeroPreviousRate(t *testing.T) {
	if got := firedAt(walRun(t, newTestEngine(), 10*mib, 0, 50*mib)); len(got) != 0 {
		t.Fatalf("fired at %v after a zero-WAL interval, want none", got)
	}
}

// Floor: an idle cluster's 50x jump that stays under 1 MiB/min is quiet;
// exactly 1 MiB/min fires.
func TestWALSpike_IdleClusterBelowFloor(t *testing.T) {
	if got := firedAt(walRun(t, newTestEngine(), 10<<10, 500<<10)); len(got) != 0 {
		t.Fatalf("10 KiB -> 500 KiB/min fired at %v, want none (floor)", got)
	}
	under := walRun(t, newTestEngine(), 100<<10, mib-1)
	if under[1] != nil {
		t.Errorf("one byte under the floor fired: %+v", under[1].Metrics)
	}
	at := walRun(t, newTestEngine(), 100<<10, mib)
	if at[1] == nil {
		t.Error("exactly 1 MiB/min with ratio 10.24 did not fire")
	}
}

// State transition: an LSN regression (failover to a standby that was
// behind) or a standby leaves that interval's WAL unknown (the collector's
// TestSystemInterval_WALUnknown): neither the pair ending there nor the
// pair after it fires, and a later spike still does.
func TestWALSpike_UnknownIntervalAfterRegression(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()
	start := time.Now().UTC()
	s := []*collector.Snapshot{
		walSnap(start),
		walIvSnap(start.Add(1*time.Minute), 10*mib),
		walSnap(start.Add(2 * time.Minute)), // promoted, behind: WAL unknown
		walIvSnap(start.Add(3*time.Minute), 80*mib),
		walIvSnap(start.Add(4*time.Minute), 10*mib),
		walIvSnap(start.Add(5*time.Minute), 100*mib),
	}
	s[2].System.Interval = &collector.SystemInterval{Seconds: 60}
	var fired []int
	for i := 1; i < len(s); i++ {
		if eng.detectWALSpike(s[i], s[i-1], cfg) != nil {
			fired = append(fired, i)
		}
	}
	if len(fired) != 1 || fired[0] != 5 {
		t.Fatalf("fired at %v, want only snapshot 5 (10 -> 100 MiB/min)", fired)
	}
}

// State transition: a pg_stat_statements reset (moved StatsEpoch, young
// counters that triple) under steady WAL gives no signal. The old
// detector fired here: 3000 / 1000 summed statement WAL bytes.
func TestWALSpike_StatementsResetNoSignal(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()
	start := time.Now().UTC()
	epoch1, epoch2 := start.Add(-time.Hour), start.Add(90*time.Second)
	s := []*collector.Snapshot{
		walSnap(start),
		walIvSnap(start.Add(1*time.Minute), 10*mib),
		walIvSnap(start.Add(2*time.Minute), 10*mib),
		walIvSnap(start.Add(3*time.Minute), 10*mib),
	}
	s[0].StatsEpoch, s[1].StatsEpoch = epoch1, epoch1
	s[0].Queries = []collector.QueryStats{{QueryID: 1, WALBytes: 900 * mib}}
	s[1].Queries = []collector.QueryStats{{QueryID: 1, WALBytes: 910 * mib}}
	s[2].StatsEpoch, s[2].StatsReset = epoch2, true
	s[2].Queries = []collector.QueryStats{{QueryID: 1, WALBytes: 1000}}
	s[3].StatsEpoch = epoch2
	s[3].Queries = []collector.QueryStats{{QueryID: 1, WALBytes: 3000}}
	for i := 1; i < len(s); i++ {
		if sig := eng.detectWALSpike(s[i], s[i-1], cfg); sig != nil {
			t.Errorf("pair %d fired across a statements reset: %+v", i, sig.Metrics)
		}
	}
}

// Invalid input: an interval without a WAL advance (never read, or
// unreadable this cycle) or with no elapsed WAL time gives no signal, on
// either side of the comparison.
func TestWALSpike_UnknownOrZeroElapsedInterval(t *testing.T) {
	cfg := defaultCfg()
	start := time.Now().UTC()
	for name, mutate := range map[string]func(prev, curr *collector.Snapshot){
		"curr unread":   func(_, c *collector.Snapshot) { c.System.Interval.WALBytes = nil },
		"prev unread":   func(p, _ *collector.Snapshot) { p.System.Interval.WALBytes = nil },
		"curr no time":  func(_, c *collector.Snapshot) { c.System.Interval.WALSeconds = 0 },
		"prev no time":  func(p, _ *collector.Snapshot) { p.System.Interval.WALSeconds = 0 },
		"curr no iv":    func(_, c *collector.Snapshot) { c.System.Interval = nil },
		"prev negative": func(p, _ *collector.Snapshot) { p.System.Interval.WALSeconds = -60 },
	} {
		prev := walIvSnap(start, 10*mib)
		curr := walIvSnap(start.Add(time.Minute), 200*mib)
		if newTestEngine().detectWALSpike(curr, prev, cfg) == nil {
			t.Fatalf("%s: the unmutated pair did not fire", name)
		}
		mutate(prev, curr)
		if sig := newTestEngine().detectWALSpike(curr, prev, cfg); sig != nil {
			t.Errorf("%s: fired %+v", name, sig.Metrics)
		}
	}
}

// The analyzer runs every analyzer.interval_seconds (600 by default) over
// the collector's latest snapshot and the one before it (60 s apart): it
// sees one pair in ten. A spike in the minute before an analyzer cycle
// fires against the minute before it, not against a rate the engine saw
// ten minutes earlier (the review of rca-03 found that design never fired
// at the default cadence).
func TestWALSpike_FiresAtTheAnalyzerCadence(t *testing.T) {
	eng := newTestEngine()
	cfg := defaultCfg()
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	snaps := []*collector.Snapshot{walSnap(start)}
	for i := 1; i <= 30; i++ {
		n := int64(10 * mib)
		if i == 20 {
			n = 100 * mib
		}
		snaps = append(snaps, walIvSnap(start.Add(time.Duration(i)*time.Minute), n))
	}
	var fired []int
	for i := 10; i < len(snaps); i += 10 {
		if eng.detectWALSpike(snaps[i], snaps[i-1], cfg) != nil {
			fired = append(fired, i)
		}
	}
	if len(fired) != 1 || fired[0] != 20 {
		t.Fatalf("analyzer cycles fired at %v, want only snapshot 20", fired)
	}
}

// Fleet: the WAL rate is the cluster's, so only the cluster owner
// detects it (rca-08's ownership).
func TestWALSpike_ClusterNonOwnerNoSignal(t *testing.T) {
	eng := newTestEngine()
	eng.SetClusterOwner(false)
	if got := firedAt(walRun(t, eng, 10*mib, 100*mib)); len(got) != 0 {
		t.Fatalf("non-owner fired at %v, want none", got)
	}
}

// The incident text states the rates, not cumulative byte totals.
func TestWALSpike_IncidentStatesRates(t *testing.T) {
	eng := newTestEngine()
	sigs := walRun(t, eng, 10*mib, 30*mib)
	if sigs[1] == nil {
		t.Fatal("3x did not fire")
	}
	now := time.Now()
	incs := eng.runDecisionTrees([]*Signal{sigs[1]}, walSnap(now), nil, defaultCfg())
	if len(incs) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incs))
	}
	inc := incs[0]
	if !strings.Contains(inc.RootCause, "3.0x") {
		t.Errorf("root cause %q, want the 3.0x ratio", inc.RootCause)
	}
	ev := inc.CausalChain[0].Evidence
	if !strings.Contains(ev, "30.0 MiB/min") || !strings.Contains(ev, "10.0 MiB/min") {
		t.Errorf("evidence %q, want both rates in MiB/min", ev)
	}
}

package selfconfig

import (
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Boundary tests for every derivation rule: each threshold just below,
// at and just above, the clamps at both bounds, and unusable evidence
// (missing, zero, negative, NaN, Inf).

func rule(t *testing.T, key string) Rule {
	t.Helper()
	r, ok := Lookup(key)
	if !ok {
		t.Fatalf("no rule for %s", key)
	}
	return r
}

type boundaryCase struct {
	name string
	ev   Evidence
	want float64
}

func runBoundaries(t *testing.T, key string, cases []boundaryCase) {
	t.Helper()
	r := rule(t, key)
	for _, tc := range cases {
		p := r.Evaluate(tc.ev, config.DefaultConfig())
		if !p.OK {
			t.Errorf("%s/%s: not derived: %s", key, tc.name, p.Reason)
			continue
		}
		if p.Value != tc.want {
			t.Errorf("%s/%s: value %v, want %v", key, tc.name, p.Value, tc.want)
		}
		if len(p.Evidence) == 0 {
			t.Errorf("%s/%s: no cited evidence", key, tc.name)
		}
		if p.Reason == "" {
			t.Errorf("%s/%s: no reason", key, tc.name)
		}
	}
}

func runUnusable(t *testing.T, key string, build func(Measure) Evidence, wantReason string) {
	t.Helper()
	r := rule(t, key)
	for name, m := range map[string]Measure{
		"unknown":  {},
		"negative": Known(-1),
		"nan":      Known(math.NaN()),
		"inf":      Known(math.Inf(1)),
		"-inf":     Known(math.Inf(-1)),
	} {
		p := r.Evaluate(build(m), config.DefaultConfig())
		if p.OK {
			t.Errorf("%s/%s: derived %v from unusable evidence", key, name, p.Value)
		}
		if !strings.Contains(p.Reason, wantReason) {
			t.Errorf("%s/%s: reason %q does not mention %q", key, name, p.Reason, wantReason)
		}
	}
}

func TestCollectorIntervalBoundaries(t *testing.T) {
	cost := func(ms float64) Evidence {
		return Evidence{CollectorCycleMs: Known(ms), Relations: Known(1200)}
	}
	runBoundaries(t, "collector.interval_seconds", []boundaryCase{
		{"zero cost keeps the default", cost(0), 60},
		{"cost at the 1% budget of 60s", cost(600), 60},
		{"just over the budget rounds up to 90s", cost(600.5), 90},
		{"cost needing exactly 90s", cost(900), 90},
		{"just over 90s rounds up to 120s", cost(900.1), 120},
		{"cost at the 600s ceiling", cost(6000), 600},
		{"just over the ceiling clamps", cost(6000.1), 600},
		{"huge cost clamps", cost(1e12), 600},
	})
	runUnusable(t, "collector.interval_seconds", func(m Measure) Evidence {
		return Evidence{CollectorCycleMs: m, Relations: Known(10)}
	}, "collector cycle cost")
}

func TestQueryTimeoutBoundaries(t *testing.T) {
	scan := func(ms float64) Evidence {
		return Evidence{CatalogScanMs: Known(ms), Relations: Known(50000)}
	}
	runBoundaries(t, "safety.query_timeout_ms", []boundaryCase{
		{"instant scan keeps the default", scan(0), 500},
		{"scan with 4x headroom inside the default", scan(125), 500},
		{"just over rounds up to 600", scan(125.01), 600},
		{"scan needing exactly 600", scan(150), 600},
		{"scan at the 5000 ceiling", scan(1250), 5000},
		{"just over the ceiling clamps", scan(1250.1), 5000},
		{"huge scan clamps", scan(1e9), 5000},
	})
	runUnusable(t, "safety.query_timeout_ms", func(m Measure) Evidence {
		return Evidence{CatalogScanMs: m}
	}, "catalog scan")
}

func TestSequenceIntervalBoundaries(t *testing.T) {
	scan := func(ms float64) Evidence {
		return Evidence{SequenceScanMs: Known(ms), Sequences: Known(40000)}
	}
	// Defaults: lookback 6 h and 10 samples cap the interval at 2400 s.
	runBoundaries(t, "sre.runways.sequence_interval_seconds", []boundaryCase{
		{"cheap scan keeps the default", scan(10), 600},
		{"scan at the 0.1% budget of 600s", scan(600), 600},
		{"just over rounds up a minute", scan(600.1), 660},
		{"scan at the lookback cap", scan(2400), 2400},
		{"just over the lookback cap clamps", scan(2400.5), 2400},
		{"huge scan clamps", scan(1e9), 2400},
	})
	runUnusable(t, "sre.runways.sequence_interval_seconds", func(m Measure) Evidence {
		return Evidence{SequenceScanMs: m}
	}, "sequence scan")
}

func TestSequenceIntervalBoundsFollowTheRunwayLookback(t *testing.T) {
	r := rule(t, "sre.runways.sequence_interval_seconds")
	ev := Evidence{SequenceScanMs: Known(1e9)}

	long := config.DefaultConfig()
	long.SRE.Runways.LookbackHours = 24
	long.SRE.Runways.SampleRetentionHours = 48
	long.SRE.Runways.MinSamples = 3
	if p := r.Evaluate(ev, long); !p.OK || p.Value != 3600 || p.Bounds.Max != 3600 {
		t.Fatalf("24h lookback, 3 samples: %+v (want the 3600s hard ceiling)", p)
	}

	short := config.DefaultConfig()
	short.SRE.Runways.LookbackHours = 1
	short.SRE.Runways.MinSamples = 10
	p := r.Evaluate(ev, short)
	if p.OK {
		t.Fatalf("1h lookback with 10 samples leaves no room above 600s, derived %v", p.Value)
	}
	if !strings.Contains(p.Reason, "bounds") {
		t.Fatalf("empty bounds reason %q", p.Reason)
	}

	single := config.DefaultConfig()
	single.SRE.Runways.MinSamples = 1
	if p := r.Evaluate(ev, single); !p.OK || p.Bounds.Max != 3600 {
		t.Fatalf("min_samples 1 (no spacing constraint): %+v", p)
	}
	if p := r.Evaluate(ev, nil); !p.OK || p.Bounds.Max != 2400 {
		t.Fatalf("nil config uses the defaults: %+v", p)
	}
}

func TestTempFileThresholdBoundaries(t *testing.T) {
	// window_seconds defaults to 300: bytes per second for N MiB per window.
	perWindow := func(mib float64) Evidence {
		return Evidence{TempBytesPerSecond: Known(mib * (1 << 20) / 300)}
	}
	runBoundaries(t, "sre.detectors.temp_file_mb", []boundaryCase{
		{"no temp traffic keeps the default", perWindow(0), 1024},
		{"normal traffic at a quarter of the default", perWindow(256), 1024},
		{"just over rounds up 256 MiB", perWindow(256.1), 1280},
		{"traffic needing exactly 2048", perWindow(512), 2048},
		{"traffic at the 64 GiB ceiling", perWindow(16384), 65536},
		{"just over the ceiling clamps", perWindow(16384.5), 65536},
	})
	runUnusable(t, "sre.detectors.temp_file_mb", func(m Measure) Evidence {
		return Evidence{TempBytesPerSecond: m}
	}, "temp")
}

func TestTempFileThresholdFollowsTheDetectorWindow(t *testing.T) {
	r := rule(t, "sre.detectors.temp_file_mb")
	cfg := config.DefaultConfig()
	cfg.SRE.Detectors.WindowSeconds = 600
	ev := Evidence{TempBytesPerSecond: Known(512 * (1 << 20) / 300.0)}
	// 512 MiB per 300 s is 1024 MiB per 600 s window: 4x = 4096.
	if p := r.Evaluate(ev, cfg); !p.OK || p.Value != 4096 {
		t.Fatalf("600s window: %+v", p)
	}
}

func TestLWLockWaitersBoundaries(t *testing.T) {
	conns := func(n float64) Evidence { return Evidence{MaxConnections: Known(n)} }
	runBoundaries(t, "sre.detectors.lwlock_waiters", []boundaryCase{
		{"small server keeps the default", conns(100), 8},
		{"2% equals the default", conns(400), 8},
		{"just over rounds up to 9", conns(401), 9},
		{"large server", conns(1000), 20},
		{"at the 64 ceiling", conns(3200), 64},
		{"just over the ceiling clamps", conns(3201), 64},
	})
	runUnusable(t, "sre.detectors.lwlock_waiters", func(m Measure) Evidence {
		return Evidence{MaxConnections: m}
	}, "max_connections")
	if p := rule(t, "sre.detectors.lwlock_waiters").Evaluate(conns(0),
		config.DefaultConfig()); p.OK {
		t.Fatalf("max_connections 0 derived %v", p.Value)
	}
}

func TestEvaluateCitesTheEvidenceItUsed(t *testing.T) {
	p := rule(t, "collector.interval_seconds").Evaluate(Evidence{
		CollectorCycleMs: Known(1800), Relations: Known(250000),
	}, config.DefaultConfig())
	names := map[string]float64{}
	for _, c := range p.Evidence {
		names[c.Name] = c.Value
	}
	if names["collector_cycle_ms"] != 1800 || names["relations"] != 250000 {
		t.Fatalf("citations %+v", p.Evidence)
	}
	if p.Bounds.Min != 60 || p.Bounds.Max != 600 || p.Bounds.Default != 60 {
		t.Fatalf("bounds %+v", p.Bounds)
	}
}

func TestClamp(t *testing.T) {
	b := Bounds{Min: 10, Max: 20, Default: 15}
	for _, tc := range []struct{ in, want float64 }{
		{5, 10}, {10, 10}, {12, 12}, {20, 20}, {25, 20},
		{math.Inf(1), 20}, {math.Inf(-1), 10}, {math.NaN(), 15},
	} {
		if got := Clamp(tc.in, b); got != tc.want {
			t.Errorf("Clamp(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// tempEvidence is avgMiB of temp files per 300 s detector window on
// average, and the detector's largest window since the previous pass
// (peakMiB < 0: the detector measured none).
func tempEvidence(avgMiB, peakMiB float64) Evidence {
	ev := Evidence{TempBytesPerSecond: Known(avgMiB * (1 << 20) / 300)}
	if peakMiB >= 0 {
		ev.TempWindowPeakBytes = Known(peakMiB * (1 << 20))
	}
	return ev
}

// footprint-04: the soak samples the detector's real per-window peaks.
// 400 MiB per window on average derives 4x = 1600, rounded to 1792; the
// average never crosses the 1024 default, but the peaks of 1 pass in 5 do.
func TestTempPeaksAboveTheThresholdPromote(t *testing.T) {
	sim := newSimulation(rule(t, "sre.detectors.temp_file_mb"), defaults())
	sim.run(soakPasses+1, func(i int) Evidence {
		if i%5 == 0 {
			return tempEvidence(400, 1500)
		}
		return tempEvidence(400, 450)
	})
	st := sim.state
	if sim.promotedAt != soakPasses || st.Active == nil || *st.Active != 1792 {
		t.Fatalf("peaks over the threshold in 20%% of windows did not promote 1792 "+
			"at pass %d: promoted at %d, state %+v, ledger %+v", soakPasses,
			sim.promotedAt, st, sim.events)
	}
	if st.Pending == nil || *st.Pending != 1792 || st.Value != 1024 {
		t.Fatalf("restart-bound temp threshold changed live: %+v", st)
	}
}

func TestFlatTempTrafficDoesNotPromote(t *testing.T) {
	for name, peak := range map[string]float64{"flat peaks": 400, "no detector": -1} {
		sim := newSimulation(rule(t, "sre.detectors.temp_file_mb"), defaults())
		sim.run(soakPasses+24, func(int) Evidence { return tempEvidence(400, peak) })
		st := sim.state
		if st.Active != nil || sim.promotedAt >= 0 || st.Shadow == nil || *st.Shadow != 1792 {
			t.Fatalf("%s: promoted without noise: %+v", name, st)
		}
		if sim.count(EventHeld) != 1 || st.ShadowOutcome != OutcomeWorse ||
			!strings.Contains(st.ShadowReason, "0% of soak windows crossed 1024 MiB") {
			t.Fatalf("%s: hold not recorded once with its reason: %+v %+v", name,
				st, sim.events)
		}
	}
}

func TestTempObservationPrefersTheDetectorPeak(t *testing.T) {
	r, cfg := rule(t, "sre.detectors.temp_file_mb"), defaults()
	for _, c := range []struct {
		name string
		ev   Evidence
		want float64
		ok   bool
	}{
		{"peak", tempEvidence(100, 700), 700, true},
		{"no peak: the average", tempEvidence(100, -1), 100, true},
		{"zero peak", tempEvidence(100, 0), 0, true},
		{"unusable peak: the average", Evidence{TempBytesPerSecond: Known(float64(1<<20) / 3),
			TempWindowPeakBytes: Known(math.NaN())}, 100, true},
		{"negative peak: the average", Evidence{TempBytesPerSecond: Known(float64(1<<20) / 3),
			TempWindowPeakBytes: Known(-5)}, 100, true},
		{"peak without an average", Evidence{TempWindowPeakBytes: Known(256 << 20)}, 256, true},
		{"nothing", Evidence{}, 0, false},
	} {
		got, ok := r.Observe(c.ev, cfg)
		if ok != c.ok || math.Abs(got-c.want) > 1e-6 {
			t.Errorf("%s: observed %v (%v), want %v (%v)", c.name, got, ok, c.want, c.ok)
		}
	}
	// The derivation stays 4x the average, whatever the peak.
	p := r.Evaluate(tempEvidence(400, 5000), cfg)
	if !p.OK || p.Value != 1792 {
		t.Fatalf("the peak moved the derivation: %+v", p)
	}
	if !strings.Contains(rule(t, "sre.detectors.temp_file_mb").EvidenceDoc, "peak") {
		t.Fatal("the generated docs do not describe the peak sample")
	}
}

func TestTempPeakIsCited(t *testing.T) {
	p := rule(t, "sre.detectors.temp_file_mb").Evaluate(tempEvidence(400, 1500), defaults())
	for _, c := range p.Evidence {
		if c.Name == "temp_window_peak_bytes" && c.Value == 1500*(1<<20) && c.Unit == "bytes" {
			return
		}
	}
	t.Fatalf("the window peak is not cited: %+v", p.Evidence)
}

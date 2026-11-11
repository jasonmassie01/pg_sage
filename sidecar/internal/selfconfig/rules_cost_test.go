package selfconfig

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// footprint-05: once the collector has run, the collector interval is
// sized from its measured cycle (p90 of the summed statement time) and the
// read deadline from its slowest statement; the probes are the fallback.
// The collector budget follows self_budget.db_time_ms_per_hour when that is
// tighter than 1% of one backend, never looser.

func measured(cycleMs, slowestMs float64, timeouts int) Evidence {
	return probeEvidence().WithCollectorCost(costOf(CollectorCost{Cycles: 60,
		CycleMs: cycleMs, SlowestMs: slowestMs, SlowestStep: "indexes",
		Timeouts: timeouts}, true))
}

func withDBTimeBudget(msPerHour int) *config.Config {
	cfg := config.DefaultConfig()
	cfg.SelfBudget.DBTimeMsPerHour = msPerHour
	return cfg
}

func withQueryTimeout(ms int) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Safety.QueryTimeoutMs = ms
	return cfg
}

func evaluate(t *testing.T, key string, ev Evidence, cfg *config.Config) Proposal {
	t.Helper()
	p := rule(t, key).Evaluate(ev, cfg)
	if !p.OK {
		t.Fatalf("%s not derived: %s", key, p.Reason)
	}
	return p
}

func TestCollectorIntervalFromTheMeasuredCycle(t *testing.T) {
	// The probe says 100 ms (10 s at 1%); the collector measured 1,200 ms.
	p := evaluate(t, "collector.interval_seconds", measured(1200, 300, 0), defaults())
	if p.Value != 120 || p.Raw == nil || *p.Raw != 120 {
		t.Fatalf("interval %+v, want 120 s from the measured 1,200 ms cycle", p)
	}
	if !strings.Contains(p.Reason, "measured") {
		t.Fatalf("reason %q does not say the cycle was measured", p.Reason)
	}
	got := citationsByName(p.Evidence)
	if c := got["measured_cycle_ms"]; c.Value != 1200 || c.Source != SourceCollector {
		t.Fatalf("citations %+v, want measured_cycle_ms 1200 from the collector", p.Evidence)
	}
}

func TestCollectorIntervalFallsBackToTheProbe(t *testing.T) {
	ev := probeEvidence()
	ev.CollectorCycleMs = Known(900)
	p := evaluate(t, "collector.interval_seconds", ev, defaults())
	if p.Value != 90 {
		t.Fatalf("probe interval %v, want 90 s from the 900 ms probe", p.Value)
	}
	if c := citationsByName(p.Evidence)["collector_cycle_ms"]; c.Source != SourceProbe {
		t.Fatalf("probe derivation cites %+v, want source probe", p.Evidence)
	}
}

// Boundary: 18,000 ms/h is 0.5% of one backend (the interval doubles);
// 36,000 is exactly 1%; one ms less is tighter; 72,000 would loosen past 1%
// and is ignored; 0 means unset.
func TestCollectorBudgetFollowsTheSelfBudget(t *testing.T) {
	for _, tc := range []struct {
		msPerHour int
		want      float64
	}{
		{0, 120}, {18000, 240}, {36000, 120}, {35999, 150}, {72000, 120},
		{3600000, 120},
	} {
		p := evaluate(t, "collector.interval_seconds", measured(1200, 300, 0),
			withDBTimeBudget(tc.msPerHour))
		if p.Value != tc.want {
			t.Errorf("db_time_ms_per_hour %d: interval %v, want %v", tc.msPerHour, p.Value,
				tc.want)
		}
	}
	if b := collectorBudgetFor(nil); b != 0.01 {
		t.Fatalf("budget without a configuration = %v, want 1%%", b)
	}
	if b := collectorBudgetFor(withDBTimeBudget(18000)); b != 0.005 {
		t.Fatalf("budget at 18,000 ms/h = %v, want 0.5%%", b)
	}
}

// The soak comparison judges against the same budget the derivation used.
func TestCollectorCompareUsesTheSelfBudget(t *testing.T) {
	r := rule(t, "collector.interval_seconds")
	samples := []float64{1200, 1200, 1200}
	if v := r.Judge(120, 240, samples, defaults()); v.Outcome != OutcomeWorse {
		t.Fatalf("at 1%% a 1,200 ms cycle every 120 s is within budget; slowing is %+v", v)
	}
	if v := r.Judge(120, 240, samples, withDBTimeBudget(18000)); v.Outcome != OutcomeNotWorse {
		t.Fatalf("at 0.5%% the same cycle is over budget; slowing is %+v", v)
	}
}

func TestCollectorIntervalObservesTheMeasuredCycle(t *testing.T) {
	r := rule(t, "collector.interval_seconds")
	if v, ok := r.Observe(measured(1200, 300, 0), defaults()); !ok || v != 1200 {
		t.Fatalf("observed %v (%v), want the measured 1,200 ms", v, ok)
	}
	if v, ok := r.Observe(probeEvidence(), defaults()); !ok || v != 100 {
		t.Fatalf("observed %v (%v), want the 100 ms probe", v, ok)
	}
}

func TestQueryTimeoutFromTheSlowestStatement(t *testing.T) {
	cases := []struct {
		name     string
		ev       Evidence
		cfg      *config.Config
		want     float64
		flagged  bool
		probeRaw bool
	}{
		{"3x the slowest", measured(900, 400, 0), defaults(), 1200, false, false},
		{"fast statements keep the floor", measured(90, 50, 0), defaults(), 500, false, false},
		{"a timeout raises to 2x the current", measured(900, 400, 1), withQueryTimeout(1000),
			2000, true, false},
		{"3x still wins when larger", measured(900, 800, 1), withQueryTimeout(1000), 2400,
			true, false},
		{"a timeout at the default", measured(100, 100, 3), defaults(), 1000, true, false},
		{"raised past the ceiling clamps", measured(9000, 2000, 1), withQueryTimeout(5000),
			5000, true, false},
		{"probe fallback is 4x the scan", probeEvidence(), defaults(), 500, false, true},
	}
	for _, tc := range cases {
		p := evaluate(t, "safety.query_timeout_ms", tc.ev, tc.cfg)
		if p.Value != tc.want {
			t.Errorf("%s: deadline %v, want %v (%s)", tc.name, p.Value, tc.want, p.Reason)
		}
		got := citationsByName(p.Evidence)
		_, seen := got["timeouts_seen"]
		if seen != tc.flagged || strings.Contains(p.Reason, "timeouts_seen") != tc.flagged {
			t.Errorf("%s: timeouts_seen cited %v / in reason %q, want %v", tc.name, seen,
				p.Reason, tc.flagged)
		}
		if _, probe := got["slowest_catalog_ms"]; probe == tc.probeRaw {
			t.Errorf("%s: slowest_catalog_ms cited %v, want %v", tc.name, probe, !tc.probeRaw)
		}
	}
}

func TestQueryTimeoutProbeFallbackIsUnchanged(t *testing.T) {
	ev := probeEvidence()
	ev.CatalogScanMs = Known(200)
	p := evaluate(t, "safety.query_timeout_ms", ev, defaults())
	if p.Value != 800 || *p.Raw != 800 {
		t.Fatalf("probe deadline %+v, want 4x the 200 ms scan", p)
	}
	if c := citationsByName(p.Evidence)["catalog_scan_ms"]; c.Source != SourceProbe {
		t.Fatalf("probe derivation cites %+v, want source probe", p.Evidence)
	}
}

func TestQueryTimeoutObservesTheSlowestStatement(t *testing.T) {
	r := rule(t, "safety.query_timeout_ms")
	if v, ok := r.Observe(measured(900, 400, 0), defaults()); !ok || v != 400 {
		t.Fatalf("observed %v (%v), want the slowest 400 ms statement", v, ok)
	}
	if v, ok := r.Observe(probeEvidence(), defaults()); !ok || v != 80 {
		t.Fatalf("observed %v (%v), want the 80 ms probe", v, ok)
	}
}

// Nil/empty: no evidence at all derives nothing for either key and does
// not panic, with or without a configuration.
func TestCostRulesWithoutEvidence(t *testing.T) {
	for _, key := range []string{"collector.interval_seconds", "safety.query_timeout_ms"} {
		for _, cfg := range []*config.Config{nil, defaults()} {
			if p := rule(t, key).Evaluate(Evidence{}, cfg); p.OK {
				t.Errorf("%s derived %v from no evidence", key, p.Value)
			}
		}
	}
}

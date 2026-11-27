package selfconfig

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-07: the intervals whose "owners" production never registered are
// restart-bound for derivation too; only a key whose reader re-reads the
// runtime config each cycle is live.
func TestFormerOrphanIntervalsAreRestartForDerivation(t *testing.T) {
	for _, key := range []string{
		"analyzer.interval_seconds", "auto_explain.collect_interval_seconds",
		"schema_lint.scan_interval_minutes", "tuner.revalidation_interval_hours",
	} {
		if !isRestart(key) || DerivedLifecycle(key) != "restart" {
			t.Errorf("%s: isRestart=%v lifecycle=%q, want restart", key,
				isRestart(key), DerivedLifecycle(key))
		}
	}
}

// The collector re-reads its interval every cycle (footprint-01), so a
// promoted value is live although an operator edit waits for a restart.
func TestCollectorIntervalStaysLiveForDerivation(t *testing.T) {
	const key = "collector.interval_seconds"
	meta, ok := config.LookupFieldLifecycle(key)
	if !ok || meta.Lifecycle != config.LifecycleRestart {
		t.Fatalf("operator lifecycle of %s = %+v, want restart", key, meta)
	}
	if isRestart(key) || DerivedLifecycle(key) != "live" {
		t.Fatalf("%s derivation: isRestart=%v lifecycle=%q, want live", key,
			isRestart(key), DerivedLifecycle(key))
	}
}

// Invalid and empty keys are restart: an unknown key never applies live.
func TestDerivedLifecycleOfUnknownKeyIsRestart(t *testing.T) {
	for _, key := range []string{"", "collector.interval_second", "llm.model"} {
		if DerivedLifecycle(key) != "restart" {
			t.Errorf("DerivedLifecycle(%q) = %q, want restart", key, DerivedLifecycle(key))
		}
	}
}

// Every live cycle reader is a derived key: no stale entry.
func TestCycleReadersAreDerivedKeys(t *testing.T) {
	for key := range cycleReaders {
		if _, ok := Lookup(key); !ok {
			t.Errorf("%s is a cycle reader but has no derivation rule", key)
		}
	}
}

func analyzerIntervalRule() Rule {
	return Rule{
		Key: "analyzer.interval_seconds", Name: "test_analyzer_interval", Version: 1,
		Unit: "s", Summary: "test rule", Widens: WidensWhenLower, Cap: CapAtDefault,
		Step: 60, HardMin: 60, HardMax: 3600,
		Derive: func(Evidence, *config.Config) Derivation {
			return Derivation{OK: true, Value: 1200, Reason: "test"}
		},
		Get: func(c *config.Config) float64 { return float64(c.Analyzer.IntervalSeconds) },
		Set: func(c *config.Config, v float64) { c.Analyzer.IntervalSeconds = int(v) },
	}
}

// State transition (real Postgres): a promotion of analyzer.interval_seconds
// is reported pending restart and never written into the runtime config.
func TestAnalyzerIntervalPromotionIsPendingRestart(t *testing.T) {
	pool, _ := testPool(t)
	e, clk := newTestEngine(pool)
	e.Rules = []Rule{analyzerIntervalRule()}
	cfg := defaults()
	before := cfg.Analyzer.IntervalSeconds
	in := Input{Cfg: cfg, OperatorSet: noOperator(), Phase: PhaseStartup}
	reconcile(t, e, in)
	in.Phase = PhaseLive
	var r Result
	for i := 0; i < 25; i++ {
		clk.Advance(time.Hour)
		r = reconcile(t, e, in)["analyzer.interval_seconds"]
	}
	if r.Applied || r.Pending == nil || *r.Pending != 1200 || r.Value != float64(before) {
		t.Fatalf("live promotion = %+v, want pending 1200 with value %d", r, before)
	}
	if cfg.Analyzer.IntervalSeconds != before {
		t.Fatalf("runtime analyzer interval = %d, want %d until restart",
			cfg.Analyzer.IntervalSeconds, before)
	}
}

package selfconfig

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Shadow comparison per rule: the candidate is promoted only when what was
// measured during the soak under the active value says it is not worse.

func judge(t *testing.T, key string, active, candidate float64, samples ...float64) Verdict {
	t.Helper()
	return rule(t, key).Judge(active, candidate, samples, config.DefaultConfig())
}

func wantOutcome(t *testing.T, name string, v Verdict, want Outcome) {
	t.Helper()
	if v.Outcome != want {
		t.Errorf("%s: outcome %q (%s), want %q", name, v.Outcome, v.Reason, want)
	}
	if v.Reason == "" {
		t.Errorf("%s: verdict without a reason", name)
	}
}

func TestJudgeNeedsThreeSamples(t *testing.T) {
	for _, key := range []string{"collector.interval_seconds", "safety.query_timeout_ms",
		"sre.runways.sequence_interval_seconds", "sre.detectors.temp_file_mb"} {
		v := judge(t, key, 1, 2, 1, 1)
		wantOutcome(t, key, v, OutcomeInsufficient)
		if !strings.Contains(v.Reason, "2 of 3") {
			t.Errorf("%s: reason %q", key, v.Reason)
		}
		v = judge(t, key, 1, 2)
		if !strings.Contains(v.Reason, "0 of 3") {
			t.Errorf("%s: no samples reason %q", key, v.Reason)
		}
	}
}

func TestJudgeWithoutAComparisonIsStability(t *testing.T) {
	v := judge(t, "sre.detectors.lwlock_waiters", 8, 20)
	wantOutcome(t, "lwlock", v, OutcomeNotWorse)
	if !strings.Contains(v.Reason, "no measurable outcome") {
		t.Fatalf("reason %q", v.Reason)
	}
}

// Collector interval: pg_sage's own DB time per cycle against a 1% budget
// of one backend.
func TestJudgeCollectorInterval(t *testing.T) {
	k := "collector.interval_seconds"
	// 900 ms per cycle at 60 s is 1.5%: slowing to 90 s (1%) is justified.
	wantOutcome(t, "slower when over budget", judge(t, k, 60, 90, 900, 900, 900),
		OutcomeNotWorse)
	// 600 ms per cycle at 60 s is exactly 1%: within budget, slower is worse.
	wantOutcome(t, "slower at the budget", judge(t, k, 60, 90, 600, 600, 600), OutcomeWorse)
	wantOutcome(t, "slower when cheap", judge(t, k, 60, 300, 10, 20, 30), OutcomeWorse)
	// Faster: only if the faster interval stays within the budget.
	wantOutcome(t, "faster within budget", judge(t, k, 120, 60, 600, 600, 600), OutcomeNotWorse)
	wantOutcome(t, "faster over budget", judge(t, k, 120, 60, 601, 601, 601), OutcomeWorse)
	// The mean is judged, not one spike.
	wantOutcome(t, "spike averaged", judge(t, k, 60, 90, 100, 100, 2500), OutcomeNotWorse)
	wantOutcome(t, "equal values", judge(t, k, 60, 60, 900, 900, 900), OutcomeWorse)
}

// Catalog deadline: observed catalog scans need 2x headroom under the
// deadline.
func TestJudgeQueryTimeout(t *testing.T) {
	k := "safety.query_timeout_ms"
	wantOutcome(t, "raise when tight", judge(t, k, 500, 1200, 260, 300, 250), OutcomeNotWorse)
	wantOutcome(t, "raise at exactly half", judge(t, k, 500, 1200, 250, 250, 250), OutcomeWorse)
	wantOutcome(t, "raise when covered", judge(t, k, 500, 1200, 10, 20, 30), OutcomeWorse)
	wantOutcome(t, "lower with headroom", judge(t, k, 2000, 600, 300, 200, 100),
		OutcomeNotWorse)
	wantOutcome(t, "lower without headroom", judge(t, k, 2000, 600, 300.5, 200, 100),
		OutcomeWorse)
}

// Sequence sampling: one scan per interval against a 0.1% budget.
func TestJudgeSequenceInterval(t *testing.T) {
	k := "sre.runways.sequence_interval_seconds"
	wantOutcome(t, "slower when over budget", judge(t, k, 600, 1200, 1100, 1200, 1300),
		OutcomeNotWorse)
	wantOutcome(t, "slower at budget", judge(t, k, 600, 1200, 600, 600, 600), OutcomeWorse)
	wantOutcome(t, "faster within budget", judge(t, k, 1200, 600, 600, 500, 400),
		OutcomeNotWorse)
	wantOutcome(t, "faster over budget", judge(t, k, 1200, 600, 700, 700, 700), OutcomeWorse)
}

// Temp-file threshold: raise only when the active threshold is noisy on
// this workload (>=10% of windows over it) and the candidate is quiet
// (<=5%); lower only when the candidate stays quiet.
func TestJudgeTempFileThreshold(t *testing.T) {
	k := "sre.detectors.temp_file_mb"
	noisy := make([]float64, 0, 20)
	for i := 0; i < 18; i++ {
		noisy = append(noisy, 500)
	}
	noisy = append(noisy, 1500, 1600) // 2 of 20 (10%) over 1024, none over 2048
	wantOutcome(t, "raise when noisy", rule(t, k).Judge(1024, 2048, noisy,
		config.DefaultConfig()), OutcomeNotWorse)

	quiet := append([]float64{1500}, make([]float64, 19)...) // 1 of 20 (5%) over 1024
	wantOutcome(t, "raise when quiet", rule(t, k).Judge(1024, 2048, quiet,
		config.DefaultConfig()), OutcomeWorse)

	stillNoisy := []float64{3000, 3000, 100, 100, 100, 100, 100, 100, 100, 100,
		100, 100, 100, 100, 100, 100, 100, 100, 100, 100} // 10% over the candidate
	wantOutcome(t, "raise still noisy", rule(t, k).Judge(1024, 2048, stillNoisy,
		config.DefaultConfig()), OutcomeWorse)

	lowerQuiet := append([]float64{1100}, make([]float64, 19)...) // 5% over 1024
	wantOutcome(t, "lower and quiet", rule(t, k).Judge(4096, 1024, lowerQuiet,
		config.DefaultConfig()), OutcomeNotWorse)
	lowerNoisy := []float64{1100, 1100, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0} // 10% over 1024
	wantOutcome(t, "lower and noisy", rule(t, k).Judge(4096, 1024, lowerNoisy,
		config.DefaultConfig()), OutcomeWorse)
}

// Observations feed the soak samples from the same evidence the rules use.
func TestObservations(t *testing.T) {
	cfg := config.DefaultConfig()
	ev := Evidence{CollectorCycleMs: Known(450), CatalogScanMs: Known(80),
		SequenceScanMs: Known(30), TempBytesPerSecond: Known(float64(1<<20) / 3),
		MaxConnections: Known(100)}
	for key, want := range map[string]float64{
		"collector.interval_seconds":            450,
		"safety.query_timeout_ms":               80,
		"sre.runways.sequence_interval_seconds": 30,
		"sre.detectors.temp_file_mb":            100, // 1/3 MiB/s over a 300 s window
	} {
		got, ok := rule(t, key).Observe(ev, cfg)
		if !ok || got < want-1e-6 || got > want+1e-6 {
			t.Errorf("%s: observed %v (%v), want %v", key, got, ok, want)
		}
		if _, ok := rule(t, key).Observe(Evidence{}, cfg); ok {
			t.Errorf("%s: observed a sample from empty evidence", key)
		}
	}
	if _, ok := rule(t, "sre.detectors.lwlock_waiters").Observe(ev, cfg); ok {
		t.Error("lwlock_waiters has no measurable outcome but observed a sample")
	}
}

package selfconfig

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// Integration (real Postgres, footprint-04): noisy evidence straddling a
// step boundary writes a bounded ledger, and the state round-trips the
// per-pass derivations.

// noisyEvidence is heavyEvidence with every measure moved by up to ±3%:
// each key's derivation straddles its step boundary (180 s, 1200 ms,
// 1500 s, 8192 MiB) without moving two steps.
func noisyEvidence(rng *rand.Rand) Evidence {
	jitter := func(v float64) Measure { return Known(v * (1 + 0.03*(2*rng.Float64()-1))) }
	h := heavyEvidence()
	return Evidence{Relations: h.Relations, Sequences: h.Sequences,
		MaxConnections:     h.MaxConnections,
		CatalogScanMs:      jitter(h.CatalogScanMs.Value),
		SequenceScanMs:     jitter(h.SequenceScanMs.Value),
		TempBytesPerSecond: jitter(h.TempBytesPerSecond.Value),
		CollectorCycleMs:   jitter(h.CollectorCycleMs.Value)}
}

func TestNoisyEvidenceLedgerBounded(t *testing.T) {
	pool, ctx := testPool(t)
	e, clk := newTestEngine(pool)
	cfg := defaults()
	rng := rand.New(rand.NewSource(1108))
	var collectorRaw []float64
	for pass := 0; pass < 30; pass++ {
		ev := noisyEvidence(rng)
		phase := PhaseLive
		if pass == 0 {
			phase = PhaseStartup
		}
		reconcile(t, e, Input{Cfg: cfg, OperatorSet: noOperator(), Evidence: ev,
			Phase: phase})
		if pass < 10 {
			collectorRaw = append(collectorRaw, ev.CollectorCycleMs.Value/1000/collectorBudget)
		}
		if pass == 9 {
			st, err := e.Store.Get(ctx, "collector.interval_seconds")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !closeAll(st.Derivations, collectorRaw) || len(st.Samples) != 9 {
				t.Fatalf("derivations did not round-trip: got %v want %v (samples %v)",
					st.Derivations, collectorRaw, st.Samples)
			}
		}
		clk.Advance(time.Hour)
	}
	for _, r := range Rules() {
		if n := ledgerCount(t, pool, r.Key, ""); n > 3 {
			t.Errorf("%s: %d ledger rows after 30 noisy passes, want at most 3", r.Key, n)
		}
		if n := ledgerCount(t, pool, r.Key, string(EventPromoted)); n != 1 {
			t.Errorf("%s: promoted %d times, want once", r.Key, n)
		}
	}
	if cfg.Collector.IntervalSeconds != 180 && cfg.Collector.IntervalSeconds != 210 {
		t.Fatalf("collector interval %d, want 180 or 210 (p90 of the soak)",
			cfg.Collector.IntervalSeconds)
	}
	st, err := e.Store.Get(ctx, "collector.interval_seconds")
	if err != nil || len(st.Derivations) != 0 {
		t.Fatalf("promoted state keeps the soak's derivations: %+v %v", st, err)
	}
}

func closeAll(got, want []float64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			return false
		}
	}
	return true
}

// A state saved before the derivations column (or by a pin) decodes with
// none, and an empty list round-trips as empty, never null.
func TestDerivationsColumnDefaultsToEmpty(t *testing.T) {
	pool, ctx := testPool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config_derived_setting
		(key, status, value, rule, rule_version) VALUES
		('collector.interval_seconds', 'default', 60, 'r', 1)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	st, err := NewStore(pool).Get(ctx, "collector.interval_seconds")
	if err != nil || len(st.Derivations) != 0 || st.Value != 60 {
		t.Fatalf("legacy row %+v %v", st, err)
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT derivations::text FROM sage.config_derived_setting
		WHERE key = 'collector.interval_seconds'`).Scan(&raw); err != nil || raw != "[]" {
		t.Fatalf("derivations default %q %v", raw, err)
	}
}

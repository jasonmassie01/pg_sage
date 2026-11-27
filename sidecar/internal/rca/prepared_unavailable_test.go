package rca

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// collection-05: when the collector could not read pg_prepared_xacts the
// category is unknown, not empty. The orphaned-2PC detector stays silent
// and an open orphaned_prepared_tx incident is not counted as cleared.
//
// No concurrent access tests: Analyze serialises cycles on cycleMu, which
// the episode tests already exercise; this change adds no shared state.

func preparedSnapshot(xacts []collector.PreparedTransaction, unavailable bool) *collector.Snapshot {
	s := &collector.Snapshot{CollectedAt: time.Now(),
		System:        collector.SystemStats{MaxConnections: 100, CacheHitRatio: 0.999},
		PreparedXacts: xacts}
	if unavailable {
		s.Unavailable = map[string]string{"prepared_xacts": "canceling statement due to " +
			"statement timeout"}
	}
	return s
}

func agedXact() []collector.PreparedTransaction {
	return []collector.PreparedTransaction{{GID: "orphan", Owner: "app", Database: "shop",
		Prepared: time.Now().Add(-2 * time.Hour), XIDAge: 5000}}
}

func openPreparedIncident(incs []Incident) *Incident {
	for i := range incs {
		for _, id := range incs[i].SignalIDs {
			if id == "orphaned_prepared_tx" {
				return &incs[i]
			}
		}
	}
	return nil
}

// Nil/empty vs unknown: an unavailable category gives no signal, even when
// the field holds a stale reading.
func TestDetectOrphanedPreparedTx_UnavailableGivesNoSignal(t *testing.T) {
	eng := newTestEngine()
	if sig := eng.detectOrphanedPreparedTx(preparedSnapshot(agedXact(), true)); sig != nil {
		t.Fatalf("signal %+v from an unavailable category, want none", sig.Metrics)
	}
	if sig := eng.detectOrphanedPreparedTx(preparedSnapshot(agedXact(), false)); sig == nil {
		t.Fatal("no signal for an available aged prepared xact")
	}
}

// CHECK-04 / state transitions: open -> unknown cycles (held open, however
// many) -> read again as none (cleared after the resolution cycles).
func TestOrphanedPreparedTx_UnknownDoesNotResolveTheIncident(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	if inc := openPreparedIncident(eng.Analyze(preparedSnapshot(agedXact(), false), nil,
		cfg, nil)); inc == nil {
		t.Fatal("precondition: no orphaned_prepared_tx incident opened")
	}
	cycles := cfg.RCA.ResolutionCycles*3 + 3 // well past the grace period too
	for i := 0; i < cycles; i++ {
		incs := eng.Analyze(preparedSnapshot(nil, true), nil, cfg, nil)
		if openPreparedIncident(incs) == nil {
			t.Fatalf("unknown cycle %d resolved the orphaned 2PC incident", i+1)
		}
	}
	resolved := false
	for i := 0; i < cfg.RCA.ResolutionCycles+1 && !resolved; i++ {
		incs := eng.Analyze(preparedSnapshot([]collector.PreparedTransaction{}, false), nil,
			cfg, nil)
		resolved = openPreparedIncident(incs) == nil
	}
	if !resolved {
		t.Fatalf("incident still open after %d cycles reading no prepared xacts",
			cfg.RCA.ResolutionCycles+1)
	}
}

// Boundary: one unknown cycle between clear cycles restarts the count, so
// the incident needs ResolutionCycles consecutive known-clear cycles.
func TestOrphanedPreparedTx_UnknownCycleRestartsTheClearCount(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	if cfg.RCA.ResolutionCycles < 2 {
		t.Fatalf("test needs resolution cycles >= 2, have %d", cfg.RCA.ResolutionCycles)
	}
	eng.Analyze(preparedSnapshot(agedXact(), false), nil, cfg, nil)
	for i := 0; i < cfg.RCA.ResolutionCycles+2; i++ { // spend the grace period
		eng.Analyze(preparedSnapshot(nil, true), nil, cfg, nil)
	}
	none := []collector.PreparedTransaction{}
	for i := 0; i < 3; i++ {
		for j := 0; j < cfg.RCA.ResolutionCycles-1; j++ {
			eng.Analyze(preparedSnapshot(none, false), nil, cfg, nil)
		}
		incs := eng.Analyze(preparedSnapshot(nil, true), nil, cfg, nil)
		if openPreparedIncident(incs) == nil {
			t.Fatalf("round %d: resolved without %d consecutive clear cycles", i,
				cfg.RCA.ResolutionCycles)
		}
	}
}

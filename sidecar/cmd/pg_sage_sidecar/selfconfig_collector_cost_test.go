package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
)

// footprint-05: each live derivation pass merges what this database's own
// collector measured (fleet: each runtime meters its own collector); the
// startup pass runs before the collector and keeps the probes.

// No concurrent access tests here: the runner reads the collector through
// CycleCost, whose concurrent reads are tested in internal/collector.

func costRunner(cost func() (selfconfig.CollectorCost, bool)) *selfConfigRunner {
	ev := selfconfig.Evidence{At: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC),
		Source: selfconfig.SourceProbe, CatalogScanMs: selfconfig.Known(40)}
	return &selfConfigRunner{name: "orders", collectorCost: cost,
		gather: func(context.Context) (selfconfig.Evidence, error) { return ev, nil }}
}

func TestSelfConfigEvidenceMergesTheCollectorCost(t *testing.T) {
	r := costRunner(func() (selfconfig.CollectorCost, bool) {
		return selfconfig.CollectorCost{Cycles: 5, CycleMs: 700, SlowestMs: 350,
			SlowestStep: "indexes", Timeouts: 1}, true
	})
	ev, err := r.evidence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Source != selfconfig.SourceCollector || ev.SlowestCatalogMs != selfconfig.Known(350) ||
		ev.MeasuredCycleMs != selfconfig.Known(700) || ev.SlowestCatalogStep != "indexes" ||
		ev.CatalogTimeouts != selfconfig.Known(1) {
		t.Fatalf("evidence %+v, want the collector's measurements", ev)
	}
	if ev.CatalogScanMs != selfconfig.Known(40) {
		t.Fatalf("the probe was dropped: %+v", ev.CatalogScanMs)
	}
}

// State: before the first cycle, and without a collector (the startup
// pass), the evidence is the probes'.
func TestSelfConfigEvidenceKeepsTheProbesBeforeACycle(t *testing.T) {
	for name, r := range map[string]*selfConfigRunner{
		"no collector": costRunner(nil),
		"no cycle yet": costRunner(func() (selfconfig.CollectorCost, bool) {
			return selfconfig.CollectorCost{}, false
		}),
	} {
		ev, _ := r.evidence(t.Context())
		if ev.Source != selfconfig.SourceProbe || ev.SlowestCatalogMs.Known {
			t.Errorf("%s: evidence %+v, want the probes", name, ev)
		}
	}
}

func TestSelfConfigRunnerMetersTheRuntimeCollector(t *testing.T) {
	cfg := config.DefaultConfig()
	startup := (&databaseRuntime{cfg: cfg}).newSelfConfigRunner()
	if startup.collectorCost != nil {
		t.Fatal("the startup runner (no collector yet) has a collector cost")
	}
	rt := &databaseRuntime{cfg: cfg,
		collector: collector.New(nil, cfg, 170000, func(string, string, ...any) {})}
	live := rt.newSelfConfigRunner()
	if live.collectorCost == nil {
		t.Fatal("the live runner does not read the runtime's collector")
	}
	if c, ok := live.collectorCost(); ok || c.Cycles != 0 {
		t.Fatalf("a collector that never ran gave %+v (%v)", c, ok)
	}
}

func TestSelfConfigCostFromTheCollector(t *testing.T) {
	got := selfConfigCost(collector.CycleCost{Cycles: 4, P90CycleMs: 800, P90SlowestMs: 300,
		SlowestMs: 900, SlowestStep: "tables", Timeouts: 2})
	want := selfconfig.CollectorCost{Cycles: 4, CycleMs: 800, SlowestMs: 300,
		SlowestStep: "tables", Timeouts: 2}
	if got != want {
		t.Fatalf("cost %+v, want %+v (the p90s, not the window maximum)", got, want)
	}
}

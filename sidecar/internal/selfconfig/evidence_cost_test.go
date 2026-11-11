package selfconfig

import (
	"context"
	"testing"
)

// footprint-05: evidence from the collector's meter (the statements a
// cycle really ran) replaces the probes once the collector has run; the
// startup pass, before any cycle, keeps the probes and says so.

func probeEvidence() Evidence {
	return Evidence{Source: SourceProbe, Relations: Known(9000), CatalogScanMs: Known(80),
		StatementsScanMs: Known(20), CollectorCycleMs: Known(100)}
}

func costOf(c CollectorCost, ok bool) func() (CollectorCost, bool) {
	return func() (CollectorCost, bool) { return c, ok }
}

func TestWithCollectorCost_MergesAMeasuredWindow(t *testing.T) {
	ev := probeEvidence().WithCollectorCost(costOf(CollectorCost{Cycles: 12, CycleMs: 1400,
		SlowestMs: 610, SlowestStep: "indexes", Timeouts: 2}, true))
	if ev.Source != SourceCollector {
		t.Fatalf("source %q, want collector", ev.Source)
	}
	if ev.MeasuredCycleMs != Known(1400) || ev.SlowestCatalogMs != Known(610) ||
		ev.SlowestCatalogStep != "indexes" || ev.CatalogTimeouts != Known(2) {
		t.Fatalf("merged evidence %+v", ev)
	}
	if ev.CatalogScanMs != Known(80) || ev.CollectorCycleMs != Known(100) {
		t.Fatalf("the probes were dropped: %+v", ev)
	}
}

// Nil/empty/state: no meter, no cycle yet, or an empty window leaves the
// probe evidence (and its source) as it is.
func TestWithCollectorCost_KeepsTheProbesWithoutACycle(t *testing.T) {
	for name, take := range map[string]func() (CollectorCost, bool){
		"nil meter":    nil,
		"no cycle yet": costOf(CollectorCost{}, false),
		"empty window": costOf(CollectorCost{Cycles: 0, CycleMs: 50}, true),
	} {
		ev := probeEvidence().WithCollectorCost(take)
		if ev.Source != SourceProbe || ev.MeasuredCycleMs.Known || ev.SlowestCatalogMs.Known ||
			ev.CatalogTimeouts.Known || ev.SlowestCatalogStep != "" {
			t.Errorf("%s: evidence %+v, want the probes alone", name, ev)
		}
	}
}

// Invalid input: a negative or NaN measurement from the meter is not
// evidence (the rules ignore unusable measures).
func TestWithCollectorCost_NegativeTimeoutsAreUnknown(t *testing.T) {
	ev := probeEvidence().WithCollectorCost(costOf(CollectorCost{Cycles: 1, CycleMs: 10,
		SlowestMs: 5, Timeouts: -1}, true))
	if ev.CatalogTimeouts.Known {
		t.Fatalf("negative timeouts became evidence: %+v", ev.CatalogTimeouts)
	}
}

func citationsByName(cs []Citation) map[string]Citation {
	out := map[string]Citation{}
	for _, c := range cs {
		out[c.Name] = c
	}
	return out
}

func TestCitationsNameTheirSource(t *testing.T) {
	ev := probeEvidence().WithCollectorCost(costOf(CollectorCost{Cycles: 3, CycleMs: 900,
		SlowestMs: 400, SlowestStep: "indexes", Timeouts: 1}, true))
	got := citationsByName(ev.Citations())
	for name, want := range map[string]string{
		"catalog_scan_ms": SourceProbe, "statements_scan_ms": SourceProbe,
		"collector_cycle_ms": SourceProbe, "measured_cycle_ms": SourceCollector,
		"slowest_catalog_ms": SourceCollector, "catalog_timeouts": SourceCollector,
		"timeouts_seen": SourceCollector,
	} {
		if c, ok := got[name]; !ok || c.Source != want {
			t.Errorf("%s cited %+v (%v), want source %q", name, c, ok, want)
		}
	}
	if c := got["slowest_catalog_ms"]; c.Value != 400 || c.Detail != "indexes" || c.Unit != "ms" {
		t.Errorf("slowest citation %+v, want 400 ms naming the indexes step", c)
	}
	if c := got["timeouts_seen"]; c.Value != 1 {
		t.Errorf("timeouts_seen %+v, want 1", c)
	}
	if c := got["relations"]; c.Source != "" {
		t.Errorf("relations is a count, not a timing: source %q", c.Source)
	}
}

// Boundary: timeouts_seen is cited only when a timeout happened.
func TestTimeoutsSeenOnlyWhenATimeoutHappened(t *testing.T) {
	ev := probeEvidence().WithCollectorCost(costOf(CollectorCost{Cycles: 3, CycleMs: 900,
		SlowestMs: 400, SlowestStep: "indexes"}, true))
	got := citationsByName(ev.Citations())
	if _, ok := got["timeouts_seen"]; ok {
		t.Fatalf("timeouts_seen cited without a timeout: %+v", got)
	}
	if c, ok := got["catalog_timeouts"]; !ok || c.Value != 0 {
		t.Fatalf("catalog_timeouts %+v (%v), want a measured 0", c, ok)
	}
}

func TestGatherWithoutADatabaseIsProbeEvidence(t *testing.T) {
	ev, err := Gather(context.Background(), nil)
	if err == nil || ev.Source != SourceProbe || ev.At.IsZero() {
		t.Fatalf("Gather(nil) = %+v, %v; want an error and probe evidence", ev, err)
	}
}

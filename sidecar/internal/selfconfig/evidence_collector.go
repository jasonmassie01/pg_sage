package selfconfig

// Evidence from the collector's own cost meter (footprint-05): once the
// collector has run, the cost rules derive from the statements it really
// ran instead of the probes, which stay the fallback for the startup pass
// (before any cycle) and are still cited.

// Sources of the cost evidence.
const (
	// SourceProbe: the evidence reads timed proxies of a collector cycle.
	SourceProbe = "probe"
	// SourceCollector: the collector's measured statement time is merged.
	SourceCollector = "collector"
)

// CollectorCost is what the collector's catalog statements spent over its
// last cycles, as the runtime hands it over (selfconfig does not import
// the collector).
type CollectorCost struct {
	Cycles int
	// CycleMs is the p90 of each cycle's summed statement time; SlowestMs
	// the p90 of each cycle's slowest statement.
	CycleMs, SlowestMs float64
	// SlowestStep is the step that ran the window's slowest statement.
	SlowestStep string
	// Timeouts counts statements cut off by the server's statement or
	// lock timeout in the window.
	Timeouts int
}

// WithCollectorCost merges the collector's measured cost (take: nil, or
// false or no cycles, when there is none yet); the probes stay.
func (ev Evidence) WithCollectorCost(take func() (CollectorCost, bool)) Evidence {
	if take == nil {
		return ev
	}
	c, ok := take()
	if !ok || c.Cycles <= 0 {
		return ev
	}
	ev.Source = SourceCollector
	ev.MeasuredCycleMs, ev.SlowestCatalogMs = Known(c.CycleMs), Known(c.SlowestMs)
	ev.SlowestCatalogStep = c.SlowestStep
	if c.Timeouts >= 0 {
		ev.CatalogTimeouts = Known(float64(c.Timeouts))
	}
	return ev
}

// timeoutsSeen reports a statement cut off by a server timeout in the
// collector's window.
func (ev Evidence) timeoutsSeen() bool {
	return usable(ev.CatalogTimeouts) && ev.CatalogTimeouts.Value > 0
}

// collectorMeasures are the citable collector measures; timeouts_seen is
// cited only when a timeout happened, flagging the derivation.
func (ev Evidence) collectorMeasures() []namedMeasure {
	out := []namedMeasure{
		{name: "measured_cycle_ms", unit: "ms", m: ev.MeasuredCycleMs,
			source: SourceCollector},
		{name: "slowest_catalog_ms", unit: "ms", m: ev.SlowestCatalogMs,
			source: SourceCollector, detail: ev.SlowestCatalogStep},
		{name: "catalog_timeouts", unit: "timeouts", m: ev.CatalogTimeouts,
			source: SourceCollector},
	}
	if ev.timeoutsSeen() {
		out = append(out, namedMeasure{name: "timeouts_seen", unit: "timeouts",
			m: ev.CatalogTimeouts, source: SourceCollector})
	}
	return out
}

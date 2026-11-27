package selfconfig

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/config"
)

// The cost rules (footprint-05): once the collector has run, the collector
// interval is sized from its measured cycle and the catalog read deadline
// from its slowest statement; before its first cycle (the startup pass)
// the timed probes are the fallback.

const (
	collectorBudget = 0.01 // pg_sage's own DB time: at most 1% of one backend
	// A deadline from the probe covers 4x the catalog scan; one from the
	// collector covers 3x its slowest statement, and after a timeout it is
	// at least 2x the deadline in force.
	timeoutHeadroom         = 4.0
	measuredTimeoutHeadroom = 3.0
	timeoutRaise            = 2.0
	msPerHour               = 3_600_000.0
)

// collectorBudgetFor is the collector's share of one backend: 1%, or the
// tighter self_budget.db_time_ms_per_hour when it is set. It never loosens
// past 1%.
func collectorBudgetFor(cfg *config.Config) float64 {
	if cfg == nil || cfg.SelfBudget.DBTimeMsPerHour <= 0 {
		return collectorBudget
	}
	return math.Min(collectorBudget, float64(cfg.SelfBudget.DBTimeMsPerHour)/msPerHour)
}

// cycleCost is the cost a collector cycle is sized from: the collector's
// measured cycle when known (measured), else the probes' estimate.
func cycleCost(ev Evidence) (ms float64, measured, ok bool) {
	if usable(ev.MeasuredCycleMs) {
		return ev.MeasuredCycleMs.Value, true, true
	}
	return ev.CollectorCycleMs.Value, false, usable(ev.CollectorCycleMs)
}

func collectorIntervalRule() Rule {
	return Rule{
		Key: "collector.interval_seconds", Name: "collector_interval_by_cycle_cost",
		Version: 1, Unit: "s", Summary: "Collector cadence that keeps one collector " +
			"cycle's database time within 1% of one backend (or " +
			"self_budget.db_time_ms_per_hour, when that is tighter).",
		EvidenceDoc: "the collector's measured statement time per cycle (p90 over its last " +
			"60 cycles); before its first cycle, the timed catalog scan plus the " +
			"pg_stat_statements read. Relation count for context.",
		OutcomeDoc: "mean measured cycle cost against the budget",
		Widens:     WidensWhenLower, Cap: CapAtDefault, Step: 30, HardMin: 60, HardMax: 600,
		PromoteP90: true,
		Cites: []string{"measured_cycle_ms", "collector_cycle_ms", "catalog_scan_ms",
			"statements_scan_ms", "relations"},
		Derive: deriveCollectorInterval,
		Observe: func(ev Evidence, _ *config.Config) (float64, bool) {
			ms, _, ok := cycleCost(ev)
			return ms, ok
		},
		Compare: dutyCompare(collectorBudgetFor, "a collector cycle"),
		Get:     func(c *config.Config) float64 { return float64(c.Collector.IntervalSeconds) },
		Set:     func(c *config.Config, v float64) { c.Collector.IntervalSeconds = int(v) },
	}
}

func deriveCollectorInterval(ev Evidence, cfg *config.Config) Derivation {
	cost, measured, ok := cycleCost(ev)
	if !ok {
		return Derivation{Reason: "the collector cycle cost is not measured (needs the " +
			"catalog scan or a collector cycle)"}
	}
	budget := collectorBudgetFor(cfg)
	need := cost / 1000 / budget
	what := "the probes estimate a collector cycle at %.0f ms of catalog and statements"
	if measured {
		what = "the collector's statements measured %.0f ms per cycle (p90)"
	}
	return Derivation{OK: true, Value: need, Reason: fmt.Sprintf(what+"; a %.3g%% budget "+
		"needs at least %.0f s between cycles", cost, budget*100, need)}
}

func queryTimeoutRule() Rule {
	return Rule{
		Key: "safety.query_timeout_ms", Name: "catalog_read_deadline", Version: 1,
		Unit: "ms", Summary: "Deadline of pg_sage's catalog and statistics reads, sized " +
			"to 3x the slowest statement the collector runs (4x the catalog scan before " +
			"its first cycle), and raised to at least 2x the deadline in force after a " +
			"timeout, so a large catalog is still collected.",
		EvidenceDoc: "the collector's slowest statement per cycle (p90 over its last 60 " +
			"cycles) and the statements its deadline cut off; before its first cycle, the " +
			"timed pass over pg_class with its statistics. Relation count.",
		OutcomeDoc: "largest slowest-read sample during the soak needs 2x headroom",
		Widens:     WidensWhenHigher, Cap: CapFixed, Step: 100, HardMin: 500, HardMax: 5000,
		PromoteP90: true,
		CapNote: "A deadline shorter than the catalog scan fails every cycle and wastes " +
			"the work done; a longer one only lets the same read finish. Capped at 10x " +
			"the default.",
		Cites: []string{"slowest_catalog_ms", "catalog_timeouts", "timeouts_seen",
			"catalog_scan_ms", "relations"},
		Derive: deriveQueryTimeout,
		Observe: func(ev Evidence, _ *config.Config) (float64, bool) {
			if usable(ev.SlowestCatalogMs) {
				return ev.SlowestCatalogMs.Value, true
			}
			return ev.CatalogScanMs.Value, usable(ev.CatalogScanMs)
		},
		Compare: func(active, candidate float64, samples []float64, _ *config.Config) Verdict {
			worst := maxOf(samples)
			if candidate > active {
				return verdict(worst*compareHeadroom > active, "the slowest catalog read "+
					"in the soak took %.0f ms; %g ms leaves less than 2x headroom", worst, active)
			}
			return verdict(candidate >= worst*compareHeadroom, "the slowest catalog read "+
				"in the soak took %.0f ms; %g ms must keep 2x headroom", worst, candidate)
		},
		Get: func(c *config.Config) float64 { return float64(c.Safety.QueryTimeoutMs) },
		Set: func(c *config.Config, v float64) { c.Safety.QueryTimeoutMs = int(v) },
	}
}

func deriveQueryTimeout(ev Evidence, cfg *config.Config) Derivation {
	if usable(ev.SlowestCatalogMs) {
		return measuredDeadline(ev, cfg)
	}
	if !usable(ev.CatalogScanMs) {
		return Derivation{Reason: "the catalog scan was not measured"}
	}
	need := ev.CatalogScanMs.Value * timeoutHeadroom
	return Derivation{OK: true, Value: need, Reason: fmt.Sprintf(
		"a catalog scan takes %.0f ms; the read deadline covers 4x that (%.0f ms)",
		ev.CatalogScanMs.Value, need)}
}

// measuredDeadline sizes the deadline from the collector's slowest
// statement; a timeout in the window raises it to at least 2x the deadline
// in force (the slowest reading of a cut-off statement is only the
// deadline itself) and flags the derivation timeouts_seen.
func measuredDeadline(ev Evidence, cfg *config.Config) Derivation {
	slowest := ev.SlowestCatalogMs.Value
	need := slowest * measuredTimeoutHeadroom
	step := ""
	if ev.SlowestCatalogStep != "" {
		step = ", step " + ev.SlowestCatalogStep
	}
	reason := fmt.Sprintf("the collector's slowest statement takes %.0f ms (p90%s); the "+
		"read deadline covers 3x that (%.0f ms)", slowest, step, need)
	if ev.timeoutsSeen() {
		current := 0.0
		if cfg != nil {
			current = float64(cfg.Safety.QueryTimeoutMs)
		}
		need = math.Max(need, current*timeoutRaise)
		reason += fmt.Sprintf("; timeouts_seen: %.0f statements hit the %g ms deadline, so "+
			"it is raised to at least 2x that (%.0f ms)", ev.CatalogTimeouts.Value, current,
			need)
	}
	return Derivation{OK: true, Value: need, Reason: reason}
}

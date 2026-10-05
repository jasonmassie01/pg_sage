package selfconfig

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/config"
)

// The initial derived keys (roadmap phase 3). Each one costs or fails on
// real databases in a way the default cannot know: pg_sage's own load on
// big catalogs (collector cadence, sequence sampling), deadlines too short
// for a big catalog (catalog read timeout), and detector thresholds that
// are noise on a busy or large server (temp files, LWLock waiters).
var registry = []Rule{
	collectorIntervalRule(),
	queryTimeoutRule(),
	sequenceIntervalRule(),
	tempFileRule(),
	lwlockWaitersRule(),
}

func init() {
	if err := ValidateRules(registry); err != nil {
		panic("selfconfig: invalid rule registry: " + err.Error())
	}
}

// Rules returns a copy of the registered rules.
func Rules() []Rule { return append([]Rule(nil), registry...) }

// Lookup finds the rule of a key.
func Lookup(key string) (Rule, bool) {
	for _, r := range registry {
		if r.Key == key {
			return r, true
		}
	}
	return Rule{}, false
}

const (
	collectorBudget = 0.01  // pg_sage's own DB time: 1% of one backend
	sequenceBudget  = 0.001 // sequence sampling: 0.1% of one backend
	timeoutHeadroom = 4.0   // a catalog read deadline covers 4x the scan
	compareHeadroom = 2.0   // promotion needs 2x headroom over observed scans
	noisyShare      = 0.10  // a threshold crossed in >=10% of windows is noise
	quietShare      = 0.05  // a quiet threshold is crossed in <=5% of windows
	lwlockShare     = 50.0  // one waiter per 50 allowed connections (2%)
)

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func maxOf(xs []float64) float64 {
	m := math.Inf(-1)
	for _, x := range xs {
		m = math.Max(m, x)
	}
	return m
}

func shareAbove(xs []float64, limit float64) float64 {
	n := 0
	for _, x := range xs {
		if x > limit {
			n++
		}
	}
	return float64(n) / float64(len(xs))
}

func verdict(ok bool, reason string, args ...any) Verdict {
	v := Verdict{Outcome: OutcomeWorse, Reason: fmt.Sprintf(reason, args...)}
	if ok {
		v.Outcome = OutcomeNotWorse
	}
	return v
}

// dutyCompare judges an interval by the measured cost per run against a
// budget share of one backend: slower only when the active value is over
// budget, faster only when the candidate stays within it.
func dutyCompare(budget float64, what string) func(float64, float64, []float64,
	*config.Config) Verdict {
	return func(active, candidate float64, samples []float64, _ *config.Config) Verdict {
		costMs := mean(samples)
		activeDuty := costMs / 1000 / active
		candidateDuty := costMs / 1000 / candidate
		if candidate > active {
			return verdict(activeDuty > budget,
				"%s costs %.0f ms per run: %.2f%% of one backend at %gs against a "+
					"%.1f%% budget", what, costMs, activeDuty*100, active, budget*100)
		}
		return verdict(candidateDuty <= budget,
			"%s costs %.0f ms per run: %.2f%% of one backend at %gs against a "+
				"%.1f%% budget", what, costMs, candidateDuty*100, candidate, budget*100)
	}
}

func collectorIntervalRule() Rule {
	return Rule{
		Key: "collector.interval_seconds", Name: "collector_interval_by_cycle_cost",
		Version: 1, Unit: "s", Summary: "Collector cadence that keeps one collector " +
			"cycle's database time within 1% of one backend.",
		EvidenceDoc: "measured time of the catalog scan plus the pg_stat_statements read " +
			"a collector cycle makes; relation count for context.",
		OutcomeDoc: "mean measured cycle cost against the 1% budget",
		Widens:     WidensWhenLower, Cap: CapAtDefault, Step: 30, HardMin: 60, HardMax: 600,
		Cites: []string{"collector_cycle_ms", "catalog_scan_ms", "statements_scan_ms",
			"relations"},
		Derive: func(ev Evidence, _ *config.Config) Derivation {
			if !usable(ev.CollectorCycleMs) {
				return Derivation{Reason: "the collector cycle cost is not measured " +
					"(needs the catalog scan)"}
			}
			need := ev.CollectorCycleMs.Value / 1000 / collectorBudget
			return Derivation{OK: true, Value: need, Reason: fmt.Sprintf(
				"a collector cycle reads %.0f ms of catalog and statements; a 1%% "+
					"budget needs at least %.0f s between cycles", ev.CollectorCycleMs.Value,
				need)}
		},
		Observe: func(ev Evidence, _ *config.Config) (float64, bool) {
			return ev.CollectorCycleMs.Value, usable(ev.CollectorCycleMs)
		},
		Compare: dutyCompare(collectorBudget, "a collector cycle"),
		Get:     func(c *config.Config) float64 { return float64(c.Collector.IntervalSeconds) },
		Set:     func(c *config.Config, v float64) { c.Collector.IntervalSeconds = int(v) },
	}
}

func queryTimeoutRule() Rule {
	return Rule{
		Key: "safety.query_timeout_ms", Name: "catalog_read_deadline", Version: 1,
		Unit: "ms", Summary: "Deadline of pg_sage's catalog and statistics reads, sized " +
			"to 4x the measured catalog scan so a large catalog is still collected.",
		EvidenceDoc: "measured time of one pass over pg_class with its statistics; " +
			"relation count.",
		OutcomeDoc: "largest catalog scan during the soak needs 2x headroom",
		Widens:     WidensWhenHigher, Cap: CapFixed, Step: 100, HardMin: 500, HardMax: 5000,
		CapNote: "A deadline shorter than the catalog scan fails every cycle and wastes " +
			"the work done; a longer one only lets the same read finish. Capped at 10x " +
			"the default.",
		Cites: []string{"catalog_scan_ms", "relations"},
		Derive: func(ev Evidence, _ *config.Config) Derivation {
			if !usable(ev.CatalogScanMs) {
				return Derivation{Reason: "the catalog scan was not measured"}
			}
			need := ev.CatalogScanMs.Value * timeoutHeadroom
			return Derivation{OK: true, Value: need, Reason: fmt.Sprintf(
				"a catalog scan takes %.0f ms; the read deadline covers 4x that (%.0f ms)",
				ev.CatalogScanMs.Value, need)}
		},
		Observe: func(ev Evidence, _ *config.Config) (float64, bool) {
			return ev.CatalogScanMs.Value, usable(ev.CatalogScanMs)
		},
		Compare: func(active, candidate float64, samples []float64, _ *config.Config) Verdict {
			worst := maxOf(samples)
			if candidate > active {
				return verdict(worst*compareHeadroom > active, "the slowest catalog scan "+
					"in the soak took %.0f ms; %g ms leaves less than 2x headroom", worst, active)
			}
			return verdict(candidate >= worst*compareHeadroom, "the slowest catalog scan "+
				"in the soak took %.0f ms; %g ms must keep 2x headroom", worst, candidate)
		},
		Get: func(c *config.Config) float64 { return float64(c.Safety.QueryTimeoutMs) },
		Set: func(c *config.Config, v float64) { c.Safety.QueryTimeoutMs = int(v) },
	}
}

func sequenceIntervalRule() Rule {
	return Rule{
		Key: "sre.runways.sequence_interval_seconds", Name: "sequence_sampling_by_cost",
		Version: 1, Unit: "s", Summary: "Sequence runway sampling cadence that keeps " +
			"reading every sequence within 0.1% of one backend.",
		EvidenceDoc: "measured time to read every sequence's last value; sequence count.",
		OutcomeDoc:  "mean measured scan time against the 0.1% budget",
		Widens:      WidensWhenLower, Cap: CapAtDefault, Step: 60, HardMin: 600, HardMax: 3600,
		Cites: []string{"sequence_scan_ms", "sequences"},
		BoundsFor: func(cfg *config.Config) (float64, float64, string) {
			r := cfg.SRE.Runways
			if r.MinSamples <= 1 {
				return 0, math.Inf(1), ""
			}
			hi := float64(r.LookbackHours) * 3600 / float64(r.MinSamples-1)
			return 0, hi, fmt.Sprintf("sre.runways.lookback_hours (%d) must still fit "+
				"sre.runways.min_samples (%d)", r.LookbackHours, r.MinSamples)
		},
		Derive: func(ev Evidence, _ *config.Config) Derivation {
			if !usable(ev.SequenceScanMs) {
				return Derivation{Reason: "the sequence scan was not measured"}
			}
			need := ev.SequenceScanMs.Value / 1000 / sequenceBudget
			return Derivation{OK: true, Value: need, Reason: fmt.Sprintf(
				"reading every sequence takes %.0f ms; a 0.1%% budget needs at least "+
					"%.0f s between samples", ev.SequenceScanMs.Value, need)}
		},
		Observe: func(ev Evidence, _ *config.Config) (float64, bool) {
			return ev.SequenceScanMs.Value, usable(ev.SequenceScanMs)
		},
		Compare: dutyCompare(sequenceBudget, "a sequence sample"),
		Get: func(c *config.Config) float64 {
			return float64(c.SRE.Runways.SequenceIntervalSeconds)
		},
		Set: func(c *config.Config, v float64) { c.SRE.Runways.SequenceIntervalSeconds = int(v) },
	}
}

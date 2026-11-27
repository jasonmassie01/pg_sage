package main

import (
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// writeSLOMetrics exposes every database's SLO error-budget state as of
// its last metrics refresh; a database whose SLO read failed is omitted.
func writeSLOMetrics(b *strings.Builder, views []metricsView) {
	byDB := map[string][]slo.Status{}
	for _, v := range views {
		if v.ok(metricsSourceSLO) {
			byDB[v.name] = v.snap.slo
		}
	}
	writeSLOStatusMetrics(b, byDB)
}

var sloStates = []slo.State{slo.StateOK, slo.StateTicket, slo.StatePage, slo.StateUnknown}

// writeSLOStatusMetrics writes the state (one-hot), each known burn rate
// and the remaining error budget; unknown numbers are not exported.
func writeSLOStatusMetrics(b *strings.Builder, byDB map[string][]slo.Status) {
	if len(byDB) == 0 {
		return
	}
	dbs := make([]string, 0, len(byDB))
	for db := range byDB {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	metricsreg.Header(b, "pg_sage_slo_state")
	forEachSLO(dbs, byDB, func(labels []labelPair, st slo.Status) {
		for _, s := range sloStates {
			v := 0
			if st.State == s {
				v = 1
			}
			writeSample(b, "pg_sage_slo_state", withLabel(labels, "state", string(s)), v)
		}
	})
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_slo_burn_rate")
	forEachSLO(dbs, byDB, func(labels []labelPair, st slo.Status) {
		for _, wb := range knownBurnRates(st) {
			writeSample(b, "pg_sage_slo_burn_rate", withLabel(labels, "window", wb.window),
				wb.burn)
		}
	})
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_slo_error_budget_remaining")
	forEachSLO(dbs, byDB, func(labels []labelPair, st slo.Status) {
		if st.BudgetRemaining != nil {
			writeSample(b, "pg_sage_slo_error_budget_remaining", labels,
				*st.BudgetRemaining)
		}
	})
	b.WriteString("\n")
}

func forEachSLO(dbs []string, byDB map[string][]slo.Status,
	fn func(labels []labelPair, st slo.Status)) {
	for _, db := range dbs {
		for _, st := range byDB[db] {
			fn([]labelPair{{"database", db}, {"slo", st.Name}, {"kind", string(st.Kind)}}, st)
		}
	}
}

type windowBurn struct {
	window string
	burn   float64
}

// knownBurnRates are the windows with a known burn rate, by window name.
func knownBurnRates(st slo.Status) []windowBurn {
	seen := map[string]float64{}
	for _, r := range st.Rules {
		for _, w := range []slo.WindowView{r.Long, r.Short} {
			if w.BurnRate != nil {
				seen[w.Window] = *w.BurnRate
			}
		}
	}
	out := make([]windowBurn, 0, len(seen))
	for w, burn := range seen {
		out = append(out, windowBurn{window: w, burn: burn})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].window < out[j].window })
	return out
}

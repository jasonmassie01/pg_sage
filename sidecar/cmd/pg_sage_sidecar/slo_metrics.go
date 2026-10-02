package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// writeSLOMetrics exposes every database's SLO error-budget state.
func writeSLOMetrics(b *strings.Builder, ctx context.Context, mgr *fleet.DatabaseManager) {
	if mgr == nil {
		return
	}
	byDB := map[string][]slo.Status{}
	for name, inst := range mgr.Instances() {
		if inst.SLO == nil {
			continue
		}
		sts, err := inst.SLO.Statuses(ctx)
		if err != nil {
			logWarn("metrics", "db %q: reading SLO state failed: %v", name, err)
			continue
		}
		byDB[name] = sts
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
	b.WriteString("# HELP pg_sage_slo_state SLO error-budget state (1 = current)\n" +
		"# TYPE pg_sage_slo_state gauge\n")
	forEachSLO(dbs, byDB, func(labels string, st slo.Status) {
		for _, s := range sloStates {
			v := 0
			if st.State == s {
				v = 1
			}
			fmt.Fprintf(b, "pg_sage_slo_state{%s,state=%q} %d\n", labels, s, v)
		}
	})
	b.WriteString("\n# HELP pg_sage_slo_burn_rate SLO burn rate per window " +
		"(1 spends the budget exactly)\n# TYPE pg_sage_slo_burn_rate gauge\n")
	forEachSLO(dbs, byDB, func(labels string, st slo.Status) {
		for _, wb := range knownBurnRates(st) {
			fmt.Fprintf(b, "pg_sage_slo_burn_rate{%s,window=%q} %g\n", labels, wb.window,
				wb.burn)
		}
	})
	b.WriteString("\n# HELP pg_sage_slo_error_budget_remaining Share of the SLO window's " +
		"error budget left\n# TYPE pg_sage_slo_error_budget_remaining gauge\n")
	forEachSLO(dbs, byDB, func(labels string, st slo.Status) {
		if st.BudgetRemaining != nil {
			fmt.Fprintf(b, "pg_sage_slo_error_budget_remaining{%s} %g\n", labels,
				*st.BudgetRemaining)
		}
	})
	b.WriteString("\n")
}

func forEachSLO(dbs []string, byDB map[string][]slo.Status,
	fn func(labels string, st slo.Status)) {
	for _, db := range dbs {
		for _, st := range byDB[db] {
			fn(fmt.Sprintf("database=%q,slo=%q,kind=%q", db, st.Name, st.Kind), st)
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

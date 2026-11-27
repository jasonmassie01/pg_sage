package tuning

import (
	"context"
	"maps"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// keepOpen re-emits the open findings unchanged, so the analyzer never
// resolves one by absence: a finding the agent did not examine this cycle
// (budget, case cap, model error, no case) stays exactly as it was. Only
// deterministic catalog evidence drops one here: its table is gone, the
// index it would create exists or is covered, the index it would drop is
// gone. An open index drop also keeps what binds every tuning drop
// (keepDrop); an open query_tuning finding resolves on its statement's
// state over the interval prev..cur (keepQueryHint). It also returns the
// live cases an open finding already answers; they are not asked again
// while it is open.
func (a *Agent) keepOpen(ctx context.Context, cur, prev *collector.Snapshot,
	open []analyzer.Finding, cases []Case) ([]analyzer.Finding, map[string]bool) {
	stale := a.staleFindings(ctx, open)
	states, quiet := a.queryStates(ctx, open), intervalQuiet(cur, prev)
	kept := make([]analyzer.Finding, 0, len(open))
	for i, f := range open {
		why, ok := stale[i]
		if !ok {
			f, why = keepDrop(f, cur)
		}
		if why == "" {
			f, why = a.keepQueryHint(f, states, quiet)
		}
		if why != "" {
			a.logFn("INFO", "tuning: open finding %s %s resolves: %s", f.Category,
				f.ObjectIdentifier, why)
			continue
		}
		kept = append(kept, f)
	}
	return a.gateInstance(kept), busyCases(kept, cases)
}

// gateInstance makes the instance settings of a database that does not
// own its instance advisory (advisors-08), so an ALTER SYSTEM stored
// before it lost ownership stays open but is never executed here.
func (a *Agent) gateInstance(findings []analyzer.Finding) []analyzer.Finding {
	if a.settings.InstanceOwner == nil {
		return findings
	}
	owner, isOwner := a.settings.InstanceOwner()
	return advisor.GateInstanceSettings(findings, owner, isOwner)
}

// keepDrop applies indexes-03 to an open tuning index drop: one no other
// index covers (a never-scanned drop stored before it) resolves, with why,
// and a covered_by drop carries this cycle's standby gate, once.
func keepDrop(f analyzer.Finding, cur *collector.Snapshot) (analyzer.Finding, string) {
	if f.Category != CategoryIndexDrop {
		return f, ""
	}
	if by, _ := f.Detail["covered_by"].(string); by == "" {
		return f, "no other index covers it; the unused_index rule owns unused drops " +
			"(first-seen clock, FK and standby guards)"
	}
	if f.Detail["replica_index_usage"] == "unknown" {
		return f, ""
	}
	f.Detail = maps.Clone(f.Detail)
	return analyzer.WithStandbyGate(f, analyzer.StandbyUsageUnknown(cur)), ""
}

// busyCases are the live cases an open finding answers: the agent's own
// by case, earlier index advice by its table. An open hint answers only the
// hint question of its case: the tuner's one-hint-per-statement check
// refuses a second hint, while the case's index and statistics answers are
// still asked for (tuning-06).
func busyCases(open []analyzer.Finding, cases []Case) map[string]bool {
	live := map[string]bool{}
	byTable := map[string][]string{}
	for _, c := range cases {
		live[c.ID] = true
		for _, t := range c.Tables {
			byTable[t] = append(byTable[t], c.ID)
		}
	}
	busy := map[string]bool{}
	for _, f := range open {
		if f.Category == categoryQueryTuning {
			continue
		}
		if producer, _ := f.Detail["producer"].(string); producer == Producer {
			if id, _ := f.Detail["case_id"].(string); live[id] {
				busy[id] = true
			}
			continue
		}
		if !isLegacyIndexFinding(f) {
			continue
		}
		for _, id := range byTable[canonicalRef(analyzer.OptimizerFindingTable(f))] {
			busy[id] = true
		}
	}
	return busy
}

// staleFindings are the open findings (by index) the catalog contradicts,
// with the reason. An unreadable catalog contradicts nothing.
func (a *Agent) staleFindings(ctx context.Context, open []analyzer.Finding) map[int]string {
	var tables, indexes []string
	for _, f := range open {
		if t := findingTable(f); t != "" {
			tables = append(tables, t)
		}
		if ix := dropIndex(f); ix != "" {
			indexes = append(indexes, ix)
		}
		if ix := replacedIndex(f); ix != "" {
			indexes = append(indexes, ix)
		}
	}
	if len(tables)+len(indexes) == 0 {
		return nil
	}
	st, err := a.deps.Store.Relations(ctx, tables, indexes)
	if err != nil {
		a.logFn("WARN", "tuning: catalog unreadable, every open finding is kept: %v", err)
		return nil
	}
	out := map[int]string{}
	for i, f := range open {
		if why := staleReason(f, st); why != "" {
			out[i] = why
		}
	}
	return out
}

func staleReason(f analyzer.Finding, st CatalogState) string {
	table := findingTable(f)
	if exists, known := st.Tables[table]; table != "" && known && !exists {
		return "table " + table + " no longer exists"
	}
	if ix := dropIndex(f); ix != "" {
		if exists, known := st.Indexes[ix]; known && !exists {
			return "index " + ix + " no longer exists"
		}
		return ""
	}
	if table == "" || !isIndexCreate(f) {
		return ""
	}
	if why := replacedIndexGone(f, st); why != "" {
		return why
	}
	sql := f.RecommendedSQL
	if create, _, ok := optimizer.SplitIndexReplaceSQL(sql); ok {
		sql = create
	}
	for _, def := range st.IndexDefs[table] {
		if optimizer.CoveredBy(sql, def) {
			return "an existing index covers it: " + def
		}
	}
	return ""
}

// findingTable is the canonical table an open finding is about, or "".
func findingTable(f analyzer.Finding) string {
	if t, _ := f.Detail["table"].(string); t != "" {
		return canonicalRef(t)
	}
	if isLegacyIndexFinding(f) {
		return canonicalRef(analyzer.OptimizerFindingTable(f))
	}
	return ""
}

// dropIndex is the canonical index an index-drop finding would drop.
func dropIndex(f analyzer.Finding) string {
	if f.Category != CategoryIndexDrop {
		return ""
	}
	ix, _ := f.Detail["index"].(string)
	return canonicalRef(ix)
}

func isIndexCreate(f analyzer.Finding) bool {
	return f.Category == CategoryIndexReplace ||
		f.Category != CategoryIndexDrop && isLegacyIndexFinding(f)
}

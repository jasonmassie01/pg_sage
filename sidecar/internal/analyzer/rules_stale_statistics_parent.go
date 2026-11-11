package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// rules-10: autoanalyze never processes a partitioned parent, so the
// parent's own statistics (the ones the planner uses for queries on the
// whole tree) go stale while its partitions change. Its own modification
// count stays 0; the partitions' counts, summed over every leaf, stand in
// for it against the parent's threshold.

// parentAnalyzeApproval is why a parent's ANALYZE needs an operator: it
// samples every partition, and before PostgreSQL 18 (ANALYZE ONLY) it
// analyzes each partition too, a full-tree pass.
const parentAnalyzeApproval = "ANALYZE of a partitioned table samples every " +
	"partition and, without ONLY (PostgreSQL 18+), also analyzes each " +
	"partition: an operator must approve it"

// partitionTree indexes the snapshot's tables and the partition list.
type partitionTree struct {
	tables   map[string]collector.TableStats
	children map[string][]string
}

// treeTotals is what the leaves under a parent add up to.
type treeTotals struct {
	mods, live int64
}

func newPartitionTree(snap *collector.Snapshot) partitionTree {
	tree := partitionTree{tables: make(map[string]collector.TableStats, len(snap.Tables)),
		children: map[string][]string{}}
	for _, t := range snap.Tables {
		tree.tables[t.SchemaName+"."+t.RelName] = t
	}
	for _, p := range snap.Partitions {
		parent := p.ParentSchema + "." + p.ParentTable
		tree.children[parent] = append(tree.children[parent], p.ChildSchema+"."+p.ChildTable)
	}
	return tree
}

// leafTotals sums the leaves below ident, through partitioned children.
// ok is false when the sum is unknown: a listed partition missing from
// the tables (dropped between the reads) or a cycle in the list.
func (tree partitionTree) leafTotals(ident string, visiting map[string]bool) (treeTotals, bool) {
	if visiting[ident] {
		return treeTotals{}, false
	}
	visiting[ident] = true
	defer delete(visiting, ident)
	var sum treeTotals
	for _, child := range tree.children[ident] {
		t, ok := tree.tables[child]
		if !ok {
			return treeTotals{}, false
		}
		if t.Relkind != "p" {
			sum.mods += t.ModSinceAnalyze
			sum.live += t.NLiveTup
			continue
		}
		sub, ok := tree.leafTotals(child, visiting)
		if !ok {
			return treeTotals{}, false
		}
		sum.mods += sub.mods
		sum.live += sub.live
	}
	return sum, true
}

// staleParentFindings raises an info finding for each partitioned parent
// never analyzed (or not within the cutoff) whose partitions changed past
// the parent's threshold since their own last analyze. Without the
// partition list (collection-05) nothing is judged.
func staleParentFindings(snap *collector.Snapshot, w staleWindow) []Finding {
	if len(snap.Partitions) == 0 || !snap.Available("partitions") {
		return nil
	}
	tree := newPartitionTree(snap)
	var findings []Finding
	for _, t := range snap.Tables {
		ident := t.SchemaName + "." + t.RelName
		if t.Relkind != "p" || len(tree.children[ident]) == 0 {
			continue
		}
		sum, ok := tree.leafTotals(ident, map[string]bool{})
		if !ok || sum.live < w.minRows || float64(sum.mods) <= t.AnalyzeThreshold {
			continue
		}
		last := mostRecentAnalyze(t)
		if !last.IsZero() && !last.Before(w.cutoff) {
			continue
		}
		findings = append(findings,
			staleParentFinding(t, sum, len(tree.children[ident]), w.staleDays))
	}
	return findings
}

func staleParentFinding(t collector.TableStats, sum treeTotals, partitions, days int) Finding {
	ident := t.SchemaName + "." + t.RelName
	last := mostRecentAnalyze(t)
	reason := "has never been analyzed"
	if !last.IsZero() {
		reason = fmt.Sprintf("was last analyzed over %d days ago", days)
	}
	q := sanitize.QuoteQualifiedName(t.SchemaName, t.RelName)
	return Finding{
		Category:         "stale_statistics",
		Severity:         "info",
		ObjectType:       "table",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf("Partitioned table %s %s, and its partitions changed %d rows "+
			"since their last analyze; autoanalyze never analyzes a partitioned parent",
			ident, reason, sum.mods),
		Detail: map[string]any{
			"partitioned_parent":    true,
			"partitions":            partitions,
			"mod_since_analyze":     sum.mods,
			"analyze_threshold":     t.AnalyzeThreshold,
			"partitions_n_live_tup": sum.live,
			"never_analyzed":        last.IsZero(),
			"last_analyze":          t.LastAnalyze,
			DetailApprovalRequired:  parentAnalyzeApproval,
		},
		Recommendation: fmt.Sprintf("Run ANALYZE on %s so the planner's whole-tree "+
			"estimates are current. It samples every partition and also analyzes each "+
			"one; on PostgreSQL 18 and later, ANALYZE ONLY %s refreshes only the "+
			"parent's statistics.", ident, q),
		RecommendedSQL: fmt.Sprintf("ANALYZE %s;", q),
		ActionRisk:     "safe",
	}
}

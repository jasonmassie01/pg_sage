package tuner

import (
	"math"

	"github.com/pg-sage/sidecar/internal/config"
)

// Catalog-aware plan heuristics (Phase 0 item 11). Plans captured by
// pg_sage are plain EXPLAIN (no ANALYZE, no VERBOSE), so a heuristic that
// needs to know a table's size or indexes asks CatalogFacts.

// indexHintMinTableRows is the smallest table on which a seq scan is
// worth an index hint: below it a sequential read is cheap anyway.
const indexHintMinTableRows = 10_000

// maxIndexHintSelectivity: an index hint is suggested only when the scan
// returns at most this fraction of the table.
const maxIndexHintSelectivity = 0.10

// nestedLoopMisestimate: a nested loop is suspect only when actual rows
// exceed this multiple of the planner's estimate.
const nestedLoopMisestimate = 10

// scanContext carries the optional catalog facts and thresholds for one
// ScanPlan call.
type scanContext struct {
	facts             *CatalogFacts
	parallelMinRows   int64
	nestedLoopMinRows int64
	// indexFilterMinRemoved and orJoinMinRemoved: the market-10 thresholds;
	// 0 or less means defaultAntipatternMinRemoved.
	indexFilterMinRemoved int64
	orJoinMinRemoved      int64
	// estimatesOnly: the plan has no actuals (plain EXPLAIN), so the
	// rules that need an executed plan are skipped (tuning-08).
	estimatesOnly bool
}

// WithEstimatesOnly skips the spill and nested-loop rules, which need an
// executed plan (EXPLAIN ANALYZE), for a plan without actuals.
func WithEstimatesOnly() ScanOption {
	return func(sc *scanContext) { sc.estimatesOnly = true }
}

// ScanOption configures ScanPlan.
type ScanOption func(*scanContext)

// WithCatalogFacts enables the catalog-dependent heuristics: a seq scan
// with a usable index, and a serial seq scan of a table with at least
// parallelMinTableRows rows (0 disables the parallel check).
func WithCatalogFacts(facts *CatalogFacts, parallelMinTableRows int64) ScanOption {
	return func(sc *scanContext) {
		sc.facts = facts
		sc.parallelMinRows = parallelMinTableRows
	}
}

// WithNestedLoopMinRows sets tuner.nested_loop_row_threshold: the total
// rows (Actual Rows x Actual Loops) the inner side of a misestimated nested
// loop must read before the bad-nested-loop symptom fires. n <= 0 (and no
// option at all) uses config.DefaultTunerNestedLoopRowThresh.
func WithNestedLoopMinRows(n int64) ScanOption {
	return func(sc *scanContext) { sc.nestedLoopMinRows = n }
}

func (sc *scanContext) innerRowThreshold() int64 {
	if sc == nil || sc.nestedLoopMinRows <= 0 {
		return config.DefaultTunerNestedLoopRowThresh
	}
	return sc.nestedLoopMinRows
}

// checkBadNestedLoop flags a nested loop whose row estimate is off by more
// than 10x (on the join or on its outer child, which sets the loop count)
// and whose inner side read at least the threshold in total. A misestimate
// alone is not enough: a 1-row estimate that returned 11 rows is cheap
// whichever join runs it (tuning-09).
func checkBadNestedLoop(n planNode, depth int, sc *scanContext) *PlanSymptom {
	if n.NodeType != "Nested Loop" {
		return nil
	}
	outer, inner := nestedLoopChildren(n)
	if !rowsMisestimated(n) && (outer == nil || !rowsMisestimated(*outer)) {
		return nil
	}
	total, loops, ok := innerRowsTotal(inner)
	if !ok || total < sc.innerRowThreshold() {
		return nil
	}
	detail := map[string]any{
		"plan_rows": n.PlanRows, "inner_rows_total": total, "loops": loops,
		"hashable_join": hashableJoin(n),
	}
	if n.ActualRows != nil {
		detail["actual_rows"] = int64(math.Round(*n.ActualRows))
	}
	return &PlanSymptom{
		Kind: SymptomBadNestedLoop, NodeType: n.NodeType, NodeDepth: depth,
		JoinAliases: joinAliases(n), Detail: detail,
	}
}

// rowsMisestimated reports actual rows above 10x a positive estimate.
func rowsMisestimated(n planNode) bool {
	return n.ActualRows != nil && n.PlanRows > 0 &&
		*n.ActualRows > float64(n.PlanRows*nestedLoopMisestimate)
}

// nestedLoopChildren returns the outer and inner inputs of a nested loop by
// Parent Relationship or, when neither is labelled, by position among the
// join inputs (SubPlan and InitPlan children are separate queries).
func nestedLoopChildren(n planNode) (outer, inner *planNode) {
	var inputs []*planNode
	for i := range n.Plans {
		switch n.Plans[i].ParentRelationship {
		case "Outer":
			outer = &n.Plans[i]
		case "Inner":
			inner = &n.Plans[i]
		case "SubPlan", "InitPlan":
			continue
		}
		inputs = append(inputs, &n.Plans[i])
	}
	if outer == nil && inner == nil && len(inputs) == 2 {
		outer, inner = inputs[0], inputs[1]
	}
	return outer, inner
}

// innerRowsTotal returns the rows the inner side read over all loops
// (Actual Rows is a per-loop average; a missing Actual Loops counts as one
// loop). ok is false without Actual Rows (plain EXPLAIN).
func innerRowsTotal(inner *planNode) (total, loops int64, ok bool) {
	if inner == nil || inner.ActualRows == nil {
		return 0, 0, false
	}
	loops = 1
	if inner.ActualLoops != nil && *inner.ActualLoops > 0 {
		loops = *inner.ActualLoops
	}
	return int64(math.Round(*inner.ActualRows * float64(loops))), loops, true
}

// checkSeqScan flags a Seq Scan only when an index could serve it: the
// table is large, the scan returns a small fraction of it, and a valid
// btree index leads with a column the filter compares (the old check
// flagged every Seq Scan).
func checkSeqScan(n planNode, depth int, sc *scanContext) *PlanSymptom {
	if n.NodeType != "Seq Scan" || n.RelationName == "" || n.Filter == nil ||
		sc == nil || sc.facts == nil {
		return nil
	}
	tableRows, ok := sc.facts.TableRows(n.Schema, n.RelationName)
	if !ok || tableRows < indexHintMinTableRows {
		return nil
	}
	rows := float64(n.PlanRows)
	if n.ActualRows != nil {
		rows = *n.ActualRows
	}
	if rows > maxIndexHintSelectivity*float64(tableRows) {
		return nil
	}
	index, ok := sc.facts.UsableIndex(n.Schema, n.RelationName, *n.Filter)
	if !ok {
		return nil
	}
	return &PlanSymptom{
		Kind: SymptomSeqScanWithIndex, NodeType: n.NodeType, NodeDepth: depth,
		RelationName: n.RelationName, Schema: n.Schema, Alias: n.Alias,
		IndexName: index,
		Detail: map[string]any{"table_rows": tableRows, "filter": *n.Filter,
			"rows": int64(rows)},
	}
}

// checkParallelDisabled flags a serial Seq Scan (not parallel-aware and
// not under a Gather / Gather Merge) of a table with at least
// parallel_min_table_rows rows. Workers Planned lives on Gather nodes,
// so the old check (no Workers Planned on the scan) flagged every scan.
func checkParallelDisabled(
	n planNode, depth int, underGather bool, sc *scanContext,
) *PlanSymptom {
	if n.NodeType != "Seq Scan" || n.RelationName == "" || underGather ||
		n.ParallelAware || sc == nil || sc.facts == nil || sc.parallelMinRows <= 0 {
		return nil
	}
	tableRows, ok := sc.facts.TableRows(n.Schema, n.RelationName)
	if !ok || tableRows < sc.parallelMinRows {
		return nil
	}
	return &PlanSymptom{
		Kind: SymptomParallelDisabled, NodeType: n.NodeType, NodeDepth: depth,
		RelationName: n.RelationName, Schema: n.Schema, Alias: n.Alias,
		Detail: map[string]any{"table_rows": tableRows},
	}
}

// joinAliases returns the aliases of the relations below a join node, in
// plan order, skipping SubPlan and InitPlan subtrees (they are separate
// queries, not inputs of the join).
func joinAliases(n planNode) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(planNode)
	walk = func(node planNode) {
		for _, child := range node.Plans {
			if child.ParentRelationship == "SubPlan" ||
				child.ParentRelationship == "InitPlan" {
				continue
			}
			if child.Alias != "" && !seen[child.Alias] {
				seen[child.Alias] = true
				out = append(out, child.Alias)
			}
			walk(child)
		}
	}
	walk(n)
	return out
}

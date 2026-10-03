package tuner

// Catalog-aware plan heuristics (Phase 0 item 11). Plans captured by
// pg_sage are plain EXPLAIN (no ANALYZE, no VERBOSE), so a heuristic that
// needs to know a table's size or indexes asks CatalogFacts.

// indexHintMinTableRows is the smallest table on which a seq scan is
// worth an index hint: below it a sequential read is cheap anyway.
const indexHintMinTableRows = 10_000

// maxIndexHintSelectivity: an index hint is suggested only when the scan
// returns at most this fraction of the table.
const maxIndexHintSelectivity = 0.10

// scanContext carries the optional catalog facts for one ScanPlan call.
type scanContext struct {
	facts           *CatalogFacts
	parallelMinRows int64
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

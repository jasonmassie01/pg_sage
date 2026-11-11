package analyzer

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// ruleIndexBloat flags btree indexes whose catalog-statistics bloat
// estimate (the collector's index_bloat category) is at least
// analyzer.index_bloat_threshold_pct: info from the threshold, warning
// from twice it (rules-13). The fix is REINDEX INDEX CONCURRENTLY, run
// under the reindex_concurrently contract (moderate, approval required).
// The collector reads only valid leaf btree indexes outside the system and
// sage schemas whose table has statistics: a partitioned index is judged
// through its leaf partitions' indexes, and an invalid one belongs to the
// invalid_index rule.
func ruleIndexBloat(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	threshold := cfg.Analyzer.IndexBloatThreshold()
	var findings []Finding
	for _, ib := range current.IndexBloat {
		// NaN compares false: an implausible estimate is never reported.
		if ib.RelPages <= 0 || !(ib.BloatPct >= float64(threshold)) {
			continue
		}
		findings = append(findings, indexBloatFinding(ib, threshold))
	}
	return findings
}

// hasIndexBloat needs this cycle's complete index_bloat reading: not read,
// failed or truncated at the row cap, a missing index is no proof that it
// shrank, so open findings must not resolve.
func hasIndexBloat(cur, _ *collector.Snapshot) bool {
	return cur.IndexBloat != nil && cur.Available("index_bloat") && !cur.IndexBloatTruncated
}

func indexBloatFinding(ib collector.IndexBloat, threshold int) Finding {
	severity := "info"
	if ib.BloatPct >= 2*float64(threshold) {
		severity = "warning"
	}
	ident := ib.SchemaName + "." + ib.IndexRelName
	pct := math.Round(ib.BloatPct)
	return Finding{
		Category:         "index_bloat",
		Severity:         severity,
		ObjectType:       "index",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf("Index %s is an estimated %.0f%% bloated (%s of %s)",
			ident, pct, humanBytes(ib.BloatBytes), humanBytes(ib.IndexBytes)),
		Detail: map[string]any{
			"table":               ib.SchemaName + "." + ib.RelName,
			"estimated_bloat_pct": ib.BloatPct,
			"bloat_bytes":         ib.BloatBytes,
			"index_bytes":         ib.IndexBytes,
			"relpages":            ib.RelPages,
			"estimated_pages":     ib.EstimatedPages,
			"fillfactor":          ib.Fillfactor,
			"reltuples":           ib.Reltuples,
			"threshold_pct":       threshold,
			"estimate_source":     "catalog_statistics",
		},
		Recommendation: fmt.Sprintf("Rebuild %s online with REINDEX INDEX CONCURRENTLY "+
			"to reclaim about %s. The rebuild needs free disk for a second copy of the "+
			"index while it runs and waits for transactions that use the table. The "+
			"estimate comes from planner statistics (reltuples, pg_stats widths and the "+
			"fillfactor against relpages), not a page scan; pgstattuple's pgstatindex "+
			"confirms it where installed.", ident, humanBytes(ib.BloatBytes)),
		RecommendedSQL: "REINDEX INDEX CONCURRENTLY " +
			sanitize.QuoteQualifiedName(ib.SchemaName, ib.IndexRelName) + ";",
		ActionRisk: "moderate",
	}
}

package analyzer

import (
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Advice is about application workload only (internal/workload): the
// query rules read adviceQueries, and a finding whose detail names
// pg_sage's own statement or diagnostic tooling (EXPLAIN, maintenance, a
// statistics reset, a backup COPY) is dropped before RCA, persistence and
// recommendations. Rules about capacity and I/O read every statement.

// adviceQueries are the snapshot's workload statements (nil-safe).
func adviceQueries(snap *collector.Snapshot) []collector.QueryStats {
	if snap == nil {
		return nil
	}
	return workload.Queries(snap.Queries)
}

// excludedFromAdvice reports a finding about pg_sage itself or about a
// statement that is not workload.
func excludedFromAdvice(f Finding) bool {
	return isSelfMonitoringFinding(f) || workload.FindingExcluded(f.Detail)
}

// dropNonWorkloadFindings keeps the findings advice may carry. Dropped
// findings are absent from the cycle's output, so an open one from before
// this rule (lifeos finding 18021) resolves with its category.
func dropNonWorkloadFindings(findings []Finding) []Finding {
	if findings == nil {
		return nil
	}
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if !workload.FindingExcluded(f.Detail) {
			out = append(out, f)
		}
	}
	return out
}

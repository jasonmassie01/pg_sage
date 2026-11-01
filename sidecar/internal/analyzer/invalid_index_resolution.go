package analyzer

import "github.com/pg-sage/sidecar/internal/collector"

// invalidIndexResolutionReady is evaluated before ruleInvalidIndexes records
// first observations. Restart warmup and an active build withhold findings;
// neither proves an existing invalid-index problem has cleared. Positive
// findings for other confirmed indexes can still be emitted this cycle.
func invalidIndexResolutionReady(cur *collector.Snapshot, extras *RuleExtras) bool {
	if cur == nil || !cur.Available("indexes") || extras == nil || extras.IndexBuildProbeFailed {
		return false
	}
	for _, idx := range cur.Indexes {
		if idx.IsValid || isSystemSchema(idx.SchemaName) {
			continue
		}
		if extras.IndexBuildTables[idx.SchemaName+"."+idx.RelName] {
			return false
		}
		if _, confirmed := extras.InvalidFirstSeen[idx.SchemaName+"."+idx.IndexRelName]; !confirmed {
			return false
		}
	}
	return true
}

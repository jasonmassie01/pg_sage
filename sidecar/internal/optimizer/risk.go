package optimizer

import "strings"

const (
	RiskSafe     = "safe"
	RiskModerate = "moderate"
	RiskHigh     = "high_risk"
)

// RiskTierForRecommendation returns the executor policy risk for an index
// recommendation. ActionLevel is confidence/exposure metadata, not risk.
//
// Index CREATE (including GIN/HNSW/composite/covering) is classified
// deterministically as MODERATE rather than trusting the LLM's self-rated
// action_risk: CREATE INDEX CONCURRENTLY is online and reversible (a plain
// DROP INDEX), so it is appropriate for autonomous execution under the
// moderate gate (31-day ramp + maintenance window). Every other statement
// is high_risk regardless of the LLM's self-rating.
func RiskTierForRecommendation(rec Recommendation) string {
	ddl := strings.TrimSpace(rec.DDL)
	if ddl == "" {
		return ""
	}
	if isIndexCreate(ddl) {
		return RiskModerate
	}
	// Anything else (DROP, REINDEX, ...) is never rated from the LLM's
	// self-assessment or the confidence tier (G3-B24): the optimizer only
	// emits canonical CREATE INDEX recommendations, so this is fail-safe.
	return RiskHigh
}

// isIndexCreate reports whether the DDL creates an index (btree, GIN, GiST,
// HNSW, IVFFlat, unique, partial, covering — any CREATE [UNIQUE] INDEX).
func isIndexCreate(ddl string) bool {
	u := strings.ToUpper(strings.TrimSpace(ddl))
	return strings.HasPrefix(u, "CREATE INDEX") ||
		strings.HasPrefix(u, "CREATE UNIQUE INDEX")
}

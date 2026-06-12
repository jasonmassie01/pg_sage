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
// moderate gate (31-day ramp + maintenance window). Index DROPs and all
// other recommendations keep the LLM/action-level risk — drops stay
// advisory because removing an index can regress queries.
func RiskTierForRecommendation(rec Recommendation) string {
	ddl := strings.TrimSpace(rec.DDL)
	if ddl == "" {
		return ""
	}
	if isIndexCreate(ddl) {
		return RiskModerate
	}
	switch strings.ToLower(strings.TrimSpace(rec.ActionRisk)) {
	case RiskSafe:
		return RiskSafe
	case RiskModerate:
		return RiskModerate
	case RiskHigh:
		return RiskHigh
	case "":
		return riskFromActionLevel(rec.ActionLevel)
	default:
		return RiskHigh
	}
}

// isIndexCreate reports whether the DDL creates an index (btree, GIN, GiST,
// HNSW, IVFFlat, unique, partial, covering — any CREATE [UNIQUE] INDEX).
func isIndexCreate(ddl string) bool {
	u := strings.ToUpper(strings.TrimSpace(ddl))
	return strings.HasPrefix(u, "CREATE INDEX") ||
		strings.HasPrefix(u, "CREATE UNIQUE INDEX")
}

func riskFromActionLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "autonomous", "advisory", "informational", "":
		return RiskModerate
	default:
		return RiskHigh
	}
}

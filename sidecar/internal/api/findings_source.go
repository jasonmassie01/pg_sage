package api

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// buildSourceClause converts a subsystem filter value into a WHERE
// fragment + args. Returns ok=false when the value is empty or
// unrecognised (caller must then emit nothing).
func buildSourceClause(
	source string, startN int,
) (string, []any, bool) {
	switch source {
	case "schema_lint":
		return fmt.Sprintf(" AND category LIKE $%d", startN),
			[]any{"schema_lint:%"}, true
	case "forecaster":
		return fmt.Sprintf(" AND (category LIKE $%d OR category = ANY($%d::text[]))",
			startN, startN+1), []any{"forecast%", forecasterCategories}, true
	case "migration_advisor":
		return " AND 1=0", nil, true // reserved, emits nothing
	}
	if cats, ok := sourceCategories[source]; ok {
		return fmt.Sprintf(" AND category = ANY($%d::text[])", startN),
			[]any{cats}, true
	}
	return "", nil, false
}

var (
	// rulesCategories is the analyzer Tier-1 allowlist. stale_statistics
	// is also emitted by the tuner, so query_tuning lists it too.
	rulesCategories = []string{
		"index_health", "unused_index", "invalid_index",
		"duplicate_index", "missing_fk_index", "slow_query",
		"high_plan_time", "query_regression", "seq_scan_heavy",
		"high_total_time", "temp_spill", "lock_chain", "connection_leak",
		"cache_hit_ratio", "checkpoint_pressure",
		"stat_statements_pressure", "replication_lag",
		"inactive_slot", "slow_replication_slot",
		"sequence_exhaustion", "sort_without_index",
		"work_mem_promotion", "table_bloat", "xid_wraparound",
		"extension_drift", "plan_regression", "wraparound_freeze",
		"autovacuum_tuning", "sage_footprint", "sage_self_cost",
		"sage_self_budget", "clone_schemas", "app_managed_index",
		"stale_statistics",
	}
	// advisorCategories are the configuration advisor's sub-advisor
	// categories (vacuum_tuning and memory_tuning are kept for rows older
	// advisors wrote) and its degraded-mode notice.
	advisorCategories = []string{
		"wal_tuning", "connection_tuning", "query_rewrite",
		"bloat_remediation", "advisor_degraded", "vacuum_tuning",
		"memory_tuning",
	}
	// optimizerCategories is missing_index plus the pre-v1.8.0 labels.
	optimizerCategories = optimizer.Categories()
	// forecasterCategories are the forecaster's categories without the
	// forecast prefix the filter also matches.
	forecasterCategories  = []string{"storage_forecast"}
	queryTuningCategories = []string{
		"query_tuning", "stale_statistics", "runaway_query",
	}
	incidentCategories = []string{
		"incident", "incident_open", "incident_resolved",
	}
	// sourceCategories maps each list-backed source filter to its list.
	sourceCategories = map[string][]string{
		"rules":        rulesCategories,
		"advisor":      advisorCategories,
		"optimizer":    optimizerCategories,
		"query_tuning": queryTuningCategories,
		"incident":     incidentCategories,
	}
	// subsystemOrder is the order subsystemFromCategory consults the
	// lists: a category in two lists (stale_statistics) is shown under
	// the first, rules.
	subsystemOrder = []string{"rules", "query_tuning", "incident", "advisor", "optimizer"}
)

// subsystemFromCategory maps a category value to the subsystem the
// UI shows in the source filter. Kept in lockstep with §16 appendix.
func subsystemFromCategory(category string) string {
	if strings.HasPrefix(category, "schema_lint:") {
		return "schema_lint"
	}
	if strings.HasPrefix(category, "forecast") ||
		slices.Contains(forecasterCategories, category) {
		return "forecaster"
	}
	for _, subsystem := range subsystemOrder {
		if slices.Contains(sourceCategories[subsystem], category) {
			return subsystem
		}
	}
	return ""
}

// riskRank orders the risk tiers a finding row can report.
var riskRank = map[string]int{"none": 0, "read_only": 1, "safe": 2, "moderate": 3, "high": 4}

// highRiskApproval is the approval reason of a high-risk row that has no
// more specific one: the policy gate always queues high risk for approval.
const highRiskApproval = "high-risk actions always need an operator's approval"

// normalizeActionRisk maps a producer's ActionRisk to a riskRank tier:
// "" is none, high_risk is high, and an unrecognised label fails closed
// to high, as the policy gate treats an unknown tier.
func normalizeActionRisk(risk string) string {
	risk = strings.ToLower(strings.TrimSpace(risk))
	switch risk {
	case "":
		return "none"
	case "high_risk":
		return "high"
	}
	if _, ok := riskRank[risk]; ok {
		return risk
	}
	return "high"
}

// withFindingRisk adds findingRiskFields to a finding row and returns it.
func withFindingRisk(row map[string]any, recommendedSQL string, detail any) map[string]any {
	maps.Copy(row, findingRiskFields(recommendedSQL, detail))
	return row
}

// findingRiskFields are a finding row's display-only action fields: the
// executor's action type and contract tier for its SQL, the stricter of
// that tier and the producer's detail.action_risk, and whether (and why)
// it needs an operator's approval. Empty SQL is advisory: tier none.
func findingRiskFields(recommendedSQL string, detail any) map[string]any {
	detailMap, _ := detail.(map[string]any)
	p := executor.FindingActionPreview(recommendedSQL, detailMap)
	risk := "none"
	if strings.TrimSpace(recommendedSQL) != "" {
		producer, _ := detailMap[analyzer.DetailActionRisk].(string)
		risk = normalizeActionRisk(producer)
		if contract := normalizeActionRisk(p.ContractTier); riskRank[contract] > riskRank[risk] {
			risk = contract
		}
	}
	if risk == "high" && !p.ApprovalRequired {
		p.ApprovalRequired, p.ApprovalReason = true, highRiskApproval
	}
	return map[string]any{
		"action_type":       p.ActionType,
		"contract_tier":     p.ContractTier,
		"action_risk":       risk,
		"approval_required": p.ApprovalRequired,
		"approval_reason":   p.ApprovalReason,
	}
}

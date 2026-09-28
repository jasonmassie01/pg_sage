package analyzer

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// optimizerRecommendationToFinding maps one optimizer candidate to a
// finding. Identity is "schema.table|<normalized index definition>" with
// the fixed optimizer category, so several candidates for one table
// persist independently and the optimizer's open-finding lookup matches
// exactly what is emitted (C05, C06, G2-B19, G3-B13). The table is kept
// separately in Detail["table"]; use OptimizerFindingTable to read it.
func optimizerRecommendationToFinding(
	rec optimizer.Recommendation,
	result *optimizer.Result,
) Finding {
	planSource := ""
	if result != nil {
		planSource = result.PlanSource
	}
	indexCategory := rec.IndexCategory
	if indexCategory == "" {
		indexCategory = rec.Category
	}
	ident := rec.FindingIdentifier()
	detail := map[string]any{
		"table":                     rec.Table,
		"index_fingerprint":         strings.TrimPrefix(ident, rec.Table+"|"),
		"ddl":                       rec.DDL,
		"drop_ddl":                  rec.DropDDL,
		"llm_rationale":             rec.Rationale,
		"confidence_score":          rec.Confidence,
		"action_level":              rec.ActionLevel,
		"action_risk":               optimizer.RiskTierForRecommendation(rec),
		"index_type":                rec.IndexType,
		"category":                  optimizer.OptimizerCategory,
		"index_category":            indexCategory,
		"estimated_improvement_pct": rec.EstimatedImprovementPct,
		"hypopg_validated":          rec.Validated,
		"plan_source":               planSource,
		"affected_queries":          rec.AffectedQueries,
		"queryids":                  rec.AffectedQueryIDs,
	}
	if rec.CostEstimate != nil && rec.CostEstimate.EstimatedSizeBytes > 0 {
		detail["estimated_size_bytes"] = rec.CostEstimate.EstimatedSizeBytes
	}
	return Finding{
		Category:         optimizer.OptimizerCategory,
		Severity:         rec.Severity,
		ObjectType:       "index",
		ObjectIdentifier: ident,
		Title:            fmt.Sprintf("Index recommendation for %s", rec.Table),
		Detail:           detail,
		Recommendation:   rec.Rationale,
		RecommendedSQL:   rec.DDL,
		RollbackSQL:      rec.DropDDL,
		ActionRisk:       optimizer.RiskTierForRecommendation(rec),
	}
}

// OptimizerFindingTable returns the "schema.table" an optimizer finding
// targets: Detail["table"] when present, otherwise the identifier prefix
// before '|' (legacy findings used the bare table as identifier).
func OptimizerFindingTable(f Finding) string {
	if t, ok := f.Detail["table"].(string); ok && t != "" {
		return t
	}
	table, _, _ := strings.Cut(f.ObjectIdentifier, "|")
	return table
}

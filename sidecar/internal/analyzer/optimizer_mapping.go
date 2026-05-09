package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

func optimizerRecommendationToFinding(
	rec optimizer.Recommendation,
	result *optimizer.Result,
) Finding {
	planSource := ""
	if result != nil {
		planSource = result.PlanSource
	}
	return Finding{
		Category:         rec.Category,
		Severity:         rec.Severity,
		ObjectType:       "index",
		ObjectIdentifier: rec.Table,
		Title:            fmt.Sprintf("Index recommendation for %s", rec.Table),
		Detail: map[string]any{
			"ddl":                       rec.DDL,
			"drop_ddl":                  rec.DropDDL,
			"llm_rationale":             rec.Rationale,
			"confidence_score":          rec.Confidence,
			"action_level":              rec.ActionLevel,
			"action_risk":               optimizer.RiskTierForRecommendation(rec),
			"index_type":                rec.IndexType,
			"category":                  rec.Category,
			"estimated_improvement_pct": rec.EstimatedImprovementPct,
			"hypopg_validated":          rec.Validated,
			"plan_source":               planSource,
			"affected_queries":          rec.AffectedQueries,
		},
		Recommendation: rec.Rationale,
		RecommendedSQL: rec.DDL,
		RollbackSQL:    rec.DropDDL,
		ActionRisk:     optimizer.RiskTierForRecommendation(rec),
	}
}

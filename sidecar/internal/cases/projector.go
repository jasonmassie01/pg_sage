package cases

import (
	"fmt"
	"strings"
	"time"
)

func ProjectFinding(f SourceFinding) Case {
	return NewCase(CaseInput{
		SourceType:       sourceTypeForFinding(f),
		SourceID:         f.ID,
		DatabaseName:     f.DatabaseName,
		IdentityKey:      IdentityKeyForFinding(f),
		Title:            f.Title,
		Severity:         f.Severity,
		Why:              f.Recommendation,
		WhyNow:           whyNowForFinding(f),
		Evidence:         evidenceForFinding(f),
		ActionCandidates: actionCandidatesForFinding(f),
	})
}

func evidenceForFinding(f SourceFinding) []Evidence {
	return []Evidence{{
		Type:    evidenceTypeForFinding(f),
		Summary: f.Title,
		Detail:  f.Detail,
	}}
}

func sourceTypeForFinding(f SourceFinding) SourceType {
	switch {
	case strings.HasPrefix(f.Category, "forecast_"):
		return SourceForecastType
	case strings.HasPrefix(f.Category, "schema_lint"):
		return SourceSchemaType
	default:
		return SourceFindingType
	}
}

func evidenceTypeForFinding(f SourceFinding) string {
	switch sourceTypeForFinding(f) {
	case SourceForecastType:
		return "forecast"
	case SourceSchemaType:
		return "schema_health"
	default:
		return "finding"
	}
}

func actionCandidatesForFinding(f SourceFinding) []ActionCandidate {
	if f.Category == "migration_safety" {
		return migrationSafetyCandidates(f)
	}

	sql := strings.TrimSpace(f.RecommendedSQL)
	if sql == "" {
		return nil
	}

	actionType := actionTypeForSQL(sql)
	if actionType == "" {
		return nil
	}

	expires := time.Now().UTC().Add(24 * time.Hour)
	return []ActionCandidate{{
		ActionType:       actionType,
		RiskTier:         riskForActionType(actionType),
		Confidence:       0.70,
		ProposedSQL:      sql,
		ExpiresAt:        &expires,
		OutputModes:      []string{"queue_for_approval", "generate_pr_or_script"},
		RollbackClass:    rollbackClassForAction(actionType),
		VerificationPlan: verificationPlanForAction(actionType),
	}}
}

func migrationSafetyCandidates(f SourceFinding) []ActionCandidate {
	sql := strings.TrimSpace(f.RecommendedSQL)
	actionType := actionTypeForSQL(sql)
	if actionType == "" {
		actionType = "ddl_preflight"
	}
	riskTier := riskForActionType(actionType)
	if detailFloat(f.Detail, "risk_score", 0) > 0.7 {
		riskTier = "high"
	}
	expires := time.Now().UTC().Add(24 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       actionType,
		RiskTier:         riskTier,
		Confidence:       migrationSafetyConfidence(f),
		ProposedSQL:      sql,
		ExpiresAt:        &expires,
		OutputModes:      migrationOutputModes(actionType),
		RollbackClass:    rollbackClassForAction(actionType),
		VerificationPlan: verificationPlanForAction(actionType),
	}
	if actionType == "ddl_preflight" {
		candidate.RollbackClass = "forward_fix_only"
		candidate.VerificationPlan = []string{
			"review generated migration plan",
			"run verification SQL in CI or staging",
			"rerun migration safety analyzer after deployment",
		}
	}
	return []ActionCandidate{enrichDDLSafetyCandidate(f, candidate)}
}

func migrationSafetyConfidence(f SourceFinding) float64 {
	if score := detailFloat(f.Detail, "risk_score", 0); score > 0 {
		return score
	}
	return 0.70
}

func migrationOutputModes(actionType string) []string {
	if actionType == "ddl_preflight" {
		return []string{"generate_pr_or_script"}
	}
	return []string{"queue_for_approval", "generate_pr_or_script"}
}

func actionTypeForSQL(sql string) string {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(upper, "ANALYZE "):
		return "analyze_table"
	case strings.HasPrefix(upper, "CREATE INDEX CONCURRENTLY "):
		return "create_index_concurrently"
	case strings.HasPrefix(upper, "DROP INDEX "):
		return "drop_unused_index"
	case strings.HasPrefix(upper, "ALTER TABLE "):
		return "alter_table"
	case upper != "":
		return "ddl_preflight"
	default:
		return ""
	}
}

func riskForActionType(actionType string) string {
	switch actionType {
	case "analyze_table":
		return "safe"
	case "create_index_concurrently", "drop_unused_index":
		return "moderate"
	default:
		return "high"
	}
}

func rollbackClassForAction(actionType string) string {
	switch actionType {
	case "analyze_table":
		return "no_rollback_needed"
	case "create_index_concurrently", "drop_unused_index":
		return "reversible"
	case "ddl_preflight":
		return "forward_fix_only"
	default:
		return "forward_fix_only"
	}
}

func verificationPlanForAction(actionType string) []string {
	if actionType == "analyze_table" {
		return []string{
			"verify last_analyze or analyze_count advanced",
			"rerun analyzer and confirm stale-stat case no longer fires",
			"compare planner row-estimate error for tracked queries",
		}
	}
	if actionType == "ddl_preflight" {
		return []string{
			"run generated verification SQL in CI or staging",
			"confirm lock and rewrite risks are acceptable",
			"rerun migration safety analyzer after deployment",
		}
	}
	return []string{"rerun analyzer and verify case state"}
}

func whyNowForFinding(f SourceFinding) string {
	if f.Detail != nil {
		if v, ok := f.Detail["n_mod_since_analyze"]; ok {
			return fmt.Sprintf("table changed since last analyze: %v rows", v)
		}
		if v, ok := f.Detail["projected_at"]; ok {
			return fmt.Sprintf("forecast threshold projected at %v", v)
		}
	}
	if f.Severity == SeverityCritical {
		return "critical severity requires immediate review"
	}
	return "not urgent"
}

func ResolveIfEvidenceMissing(c Case, evidencePresent bool) Case {
	if evidencePresent {
		return c
	}

	c.State = StateResolvedEphemeral
	c.ActionCandidates = nil
	c.WhyNow = "underlying evidence disappeared before action"
	c.UpdatedAt = time.Now().UTC()
	return c
}

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
	if candidate, ok := vacuumAutopilotCandidate(f); ok {
		return []ActionCandidate{candidate}
	}
	if candidate, ok := queryTuningCandidate(f); ok {
		return []ActionCandidate{candidate}
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
	case strings.HasPrefix(upper, "VACUUM "):
		return "vacuum_table"
	case strings.HasPrefix(upper, "CREATE INDEX CONCURRENTLY "):
		return "create_index_concurrently"
	case strings.HasPrefix(upper, "DROP INDEX "):
		return "drop_unused_index"
	case strings.HasPrefix(upper, "ALTER TABLE ") &&
		strings.Contains(upper, "AUTOVACUUM_"):
		return "set_table_autovacuum"
	case strings.HasPrefix(upper, "ALTER ROLE ") &&
		strings.Contains(upper, "WORK_MEM"):
		return "promote_role_work_mem"
	case strings.HasPrefix(upper, "CREATE STATISTICS "):
		return "create_statistics"
	case strings.HasPrefix(upper, "REINDEX ") &&
		strings.Contains(upper, " CONCURRENTLY "):
		return "reindex_concurrently"
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
	case "analyze_table", "vacuum_table", "diagnose_freeze_blockers":
		return "safe"
	case "set_table_autovacuum":
		return "moderate"
	case "prepare_query_rewrite", "promote_role_work_mem":
		return "moderate"
	case "create_statistics", "prepare_parameterized_query",
		"reindex_concurrently":
		return "moderate"
	case "retire_query_hint":
		return "safe"
	case "create_index_concurrently", "drop_unused_index":
		return "moderate"
	default:
		return "high"
	}
}

func rollbackClassForAction(actionType string) string {
	switch actionType {
	case "analyze_table", "vacuum_table":
		return "no_rollback_needed"
	case "diagnose_freeze_blockers":
		return "not_applicable"
	case "create_index_concurrently", "drop_unused_index":
		return "reversible"
	case "set_table_autovacuum":
		return "forward_fix_only"
	case "prepare_query_rewrite":
		return "application_rollback"
	case "promote_role_work_mem", "retire_query_hint":
		return "reversible"
	case "create_statistics", "reindex_concurrently":
		return "reversible"
	case "prepare_parameterized_query":
		return "application_rollback"
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
	if actionType == "vacuum_table" {
		return []string{
			"verify last_vacuum or vacuum_count advanced",
			"confirm n_dead_tup decreases or dead tuple ratio improves",
			"rerun bloat analyzer and confirm case no longer fires",
		}
	}
	if actionType == "diagnose_freeze_blockers" {
		return []string{
			"identify oldest xmin holders and freeze blockers",
			"confirm XID runway and table age after remediation",
		}
	}
	if actionType == "set_table_autovacuum" {
		return []string{
			"verify reloptions contain expected autovacuum settings",
			"monitor future autovacuum cadence and dead tuple ratio",
			"rerun vacuum tuning analyzer after one churn window",
		}
	}
	if actionType == "prepare_query_rewrite" {
		return queryRewriteVerificationPlan()
	}
	if actionType == "promote_role_work_mem" {
		return roleWorkMemVerificationPlan()
	}
	if actionType == "retire_query_hint" {
		return []string{"verify hint no longer appears in active hints"}
	}
	if actionType == "create_statistics" {
		return []string{
			"verify pg_statistic_ext contains the new statistics object",
			"run ANALYZE on the affected table",
			"compare planner row estimates for the affected query",
		}
	}
	if actionType == "prepare_parameterized_query" {
		return []string{
			"verify query semantics match the literal-heavy version",
			"compare pg_stat_statements queryid churn before and after",
			"monitor latency and plan stability after deployment",
		}
	}
	if actionType == "reindex_concurrently" {
		return []string{
			"verify replacement index is valid",
			"compare index size before and after reindex",
			"rerun bloat analyzer and confirm case no longer fires",
		}
	}
	if actionType == "diagnose_vacuum_pressure" {
		return []string{"identify vacuum blockers and dead tuple pressure"}
	}
	if actionType == "plan_bloat_remediation" {
		return []string{
			"review online rebuild or pg_repack plan",
			"verify table size and bloat ratio after remediation",
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

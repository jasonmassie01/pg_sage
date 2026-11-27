package cases

import (
	"strings"
	"time"
)

// SQLClassifier is the executor's classification of a recommended
// statement: the action type whose contract would run it and that
// contract's base risk tier, or ("", "") when no contract runs it. The api
// passes executor.ClassifySQL; cases cannot import the executor (the
// executor depends on cases), and must not keep a table of its own that
// can disagree with it (cases-04).
type SQLClassifier func(sql string) (actionType, riskTier string)

// ProjectFinding projects a finding into a case, classifying its SQL with
// classify. A nil classifier classifies nothing (fail closed): a change is
// a high-risk, script-only ddl_preflight and anything else has no
// candidate.
func ProjectFinding(f SourceFinding, classify SQLClassifier) Case {
	candidates := actionCandidatesForFinding(f, classify)
	source := f
	for i := range candidates {
		candidates[i].Source = &source
	}
	urgency := readUrgency(f)
	c := NewCase(CaseInput{
		SourceType:       sourceTypeForFinding(f),
		SourceID:         f.ID,
		DatabaseName:     f.DatabaseName,
		IdentityKey:      IdentityKeyForFinding(f),
		Title:            f.Title,
		Severity:         f.Severity,
		Why:              f.Recommendation,
		WhyNow:           urgency.whyNow,
		Evidence:         evidenceForFinding(f),
		ActionCandidates: candidates,
		ObservedAt:       f.ObservedAt,
	})
	c.Urgency = urgency.urgency
	return c
}

// evidenceTime is when the source finding was last observed, falling back
// to now when unknown. Candidate expiry is anchored to it so a stale
// proposal expires instead of sliding forward on every read (G2-B24).
func evidenceTime(f SourceFinding) time.Time {
	if f.ObservedAt.IsZero() {
		return time.Now().UTC()
	}
	return f.ObservedAt.UTC()
}

func evidenceForFinding(f SourceFinding) []Evidence {
	return []Evidence{{
		Type:    evidenceTypeForFinding(f),
		Summary: f.Title,
		Detail:  f.Detail,
		QueryID: detailQueryID(f),
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

// upgradeReadinessCategory is upgradecheck.Category (market-05); cases
// does not import upgradecheck, which imports the analyzer.
const upgradeReadinessCategory = "upgrade_readiness"

func actionCandidatesForFinding(f SourceFinding, classify SQLClassifier) []ActionCandidate {
	if f.Category == upgradeReadinessCategory {
		return nil // advisory: the upgrade is the operator's, never an executor action
	}
	if f.Category == "migration_safety" {
		return migrationSafetyCandidates(f, classify)
	}
	if candidates, ok := findingPlaybookCandidates(f); ok {
		return candidates
	}
	if candidate, ok := vacuumAutopilotCandidate(f, classify); ok {
		return []ActionCandidate{candidate}
	}
	if candidate, ok := queryTuningCandidate(f); ok {
		return []ActionCandidate{candidate}
	}

	sql := strings.TrimSpace(f.RecommendedSQL)
	if sql == "" {
		return nil
	}

	actionType, riskTier := classifySQL(sql, classify)
	if actionType == "" {
		return nil
	}

	expires := evidenceTime(f).Add(24 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       actionType,
		RiskTier:         riskTier,
		Confidence:       0.70,
		ProposedSQL:      sql,
		ExpiresAt:        &expires,
		OutputModes:      candidateOutputModes(actionType),
		RollbackClass:    rollbackClassForAction(actionType),
		VerificationPlan: verificationPlanForAction(actionType),
	}
	if scriptableSQLAction(actionType) {
		return []ActionCandidate{enrichDDLSafetyCandidate(f, candidate)}
	}
	return []ActionCandidate{candidate}
}

// migrationSafetyCandidates is the DDL candidate plus, when the lock
// queue behind the DDL escalated the finding, the approval-only cancel of
// the DDL backend; that cancel's evidence is good for an hour.
func migrationSafetyCandidates(f SourceFinding, classify SQLClassifier) []ActionCandidate {
	out := migrationDDLCandidates(f, classify)
	if c, ok := lockQueueCancelCandidate(f, evidenceTime(f).Add(time.Hour)); ok {
		out = append(out, c)
	}
	return out
}

func migrationDDLCandidates(f SourceFinding, classify SQLClassifier) []ActionCandidate {
	sql := strings.TrimSpace(f.RecommendedSQL)
	actionType, riskTier := classifySQL(sql, classify)
	if actionType == "" {
		actionType, riskTier = ddlPreflightType, ddlPreflightRisk
	}
	if detailFloat(f.Detail, "risk_score", 0) > 0.7 {
		riskTier = "high"
	}
	expires := evidenceTime(f).Add(24 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       actionType,
		RiskTier:         riskTier,
		Confidence:       migrationSafetyConfidence(f),
		ProposedSQL:      sql,
		ExpiresAt:        &expires,
		OutputModes:      candidateOutputModes(actionType),
		RollbackClass:    rollbackClassForAction(actionType),
		VerificationPlan: verificationPlanForAction(actionType),
	}
	if scriptableSQLAction(actionType) {
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
	return []ActionCandidate{candidate}
}

func scriptableSQLAction(actionType string) bool {
	switch actionType {
	case "create_index_concurrently", "drop_unused_index",
		"set_table_autovacuum", "promote_role_work_mem",
		"create_statistics", "reindex_concurrently", "alter_table",
		"ddl_preflight":
		return true
	default:
		return false
	}
}

func migrationSafetyConfidence(f SourceFinding) float64 {
	if score := detailFloat(f.Detail, "risk_score", 0); score > 0 {
		return score
	}
	return 0.70
}

func candidateOutputModes(actionType string) []string {
	if actionType == "ddl_preflight" {
		return []string{"generate_pr_or_script"}
	}
	return []string{"queue_for_approval", "generate_pr_or_script"}
}

// RiskTierForSQL is the risk tier the case projector gives a candidate
// running sql, as classify (the executor) has it; a statement the executor
// runs under no contract fails closed to "high". Blank SQL has no tier.
func RiskTierForSQL(sql string, classify SQLClassifier) string {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return ""
	}
	if _, riskTier := classifySQL(sql, classify); riskTier != "" {
		return riskTier
	}
	return ddlPreflightRisk
}

// ddlPreflightType is the overlay for a change the executor runs under no
// contract: a high-risk, script-only migration plan.
const (
	ddlPreflightType = "ddl_preflight"
	ddlPreflightRisk = "high"
)

// classifySQL is the executor's action type and risk tier for sql, or the
// ddl_preflight overlay for an unclassified change, or ("", "").
func classifySQL(sql string, classify SQLClassifier) (string, string) {
	if classify != nil {
		if actionType, riskTier := classify(sql); actionType != "" {
			return actionType, riskTier
		}
	}
	if unclassifiedChange(sql) {
		return ddlPreflightType, ddlPreflightRisk
	}
	return "", ""
}

// unclassifiedChange reports a statement that changes the database. One
// the executor has no contract for (VACUUM FULL, a plain DROP INDEX, a
// non-concurrent REINDEX, ALTER ROLE, an unlisted setting) is only ever a
// script to review.
func unclassifiedChange(sql string) bool {
	upper := strings.ToUpper(strings.Join(strings.Fields(sql), " "))
	for _, prefix := range []string{"CREATE ", "ALTER ", "DROP ", "GRANT ", "REVOKE ",
		"COMMENT ", "TRUNCATE ", "VACUUM", "REINDEX "} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func rollbackClassForAction(actionType string) string {
	switch actionType {
	case "analyze_table", "vacuum_table":
		return "no_rollback_needed"
	case "diagnose_freeze_blockers", "investigate_query_plan":
		return "not_applicable"
	case "create_index_concurrently", "drop_unused_index":
		return "reversible"
	case "set_table_autovacuum":
		return "unknown" // the previous reloptions must be captured
	case "prepare_query_rewrite":
		return "application_rollback"
	case "apply_query_hint":
		return "reversible"
	case "create_statistics":
		return "reversible"
	case "promote_role_work_mem", "retire_query_hint", "reindex_concurrently":
		return "unknown"
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
	if actionType == "apply_query_hint" {
		return []string{
			"verify hint row exists in hint_plan.hints",
			"review the hinted vs unhinted EXPLAIN in detail.hint_evidence",
			"monitor latency and plan stability after deployment",
		}
	}
	if actionType == "investigate_query_plan" {
		return []string{
			"capture EXPLAIN plan for the query",
			"classify whether remediation is index, stats, memory, or rewrite",
			"rerun analyzer after follow-up remediation",
		}
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

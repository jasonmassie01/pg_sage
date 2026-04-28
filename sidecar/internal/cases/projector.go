package cases

import (
	"fmt"
	"strings"
	"time"
)

func ProjectFinding(f SourceFinding) Case {
	return NewCase(CaseInput{
		SourceType:       SourceFindingType,
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
		Type:    "finding",
		Summary: f.Title,
		Detail:  f.Detail,
	}}
}

func actionCandidatesForFinding(f SourceFinding) []ActionCandidate {
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
	return []string{"rerun analyzer and verify case state"}
}

func whyNowForFinding(f SourceFinding) string {
	if f.Detail != nil {
		if v, ok := f.Detail["n_mod_since_analyze"]; ok {
			return fmt.Sprintf("table changed since last analyze: %v rows", v)
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

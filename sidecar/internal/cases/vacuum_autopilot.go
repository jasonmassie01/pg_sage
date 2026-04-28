package cases

import (
	"strings"
	"time"
)

func vacuumAutopilotCandidate(f SourceFinding) (ActionCandidate, bool) {
	switch f.Category {
	case "table_bloat", "schema_lint:lint_bloated_table":
		return tableBloatCandidate(f), true
	case "xid_wraparound", "schema_lint:lint_txid_age",
		"schema_lint:lint_mxid_age":
		return freezeDiagnosticCandidate(f), true
	case "vacuum_tuning":
		return autovacuumTuningCandidate(f), true
	default:
		return ActionCandidate{}, false
	}
}

func tableBloatCandidate(f SourceFinding) ActionCandidate {
	sql := strings.TrimSpace(f.RecommendedSQL)
	if sql == "" {
		sql = "VACUUM " + f.ObjectIdentifier + ";"
	}
	expires := time.Now().UTC().Add(24 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       "vacuum_table",
		RiskTier:         "safe",
		Confidence:       0.74,
		ProposedSQL:      sql,
		ExpiresAt:        &expires,
		OutputModes:      []string{"execute", "queue_for_approval", "generate_pr_or_script"},
		RollbackClass:    "no_rollback_needed",
		VerificationPlan: verificationPlanForAction("vacuum_table"),
	}
	if detailBool(f.Detail, "io_saturated", false) {
		candidate.BlockedReason = "IO is saturated; wait for maintenance window or lower load"
		candidate.OutputModes = []string{"generate_pr_or_script"}
	}
	return candidate
}

func freezeDiagnosticCandidate(f SourceFinding) ActionCandidate {
	expires := time.Now().UTC().Add(30 * time.Minute)
	return ActionCandidate{
		ActionType:       "diagnose_freeze_blockers",
		RiskTier:         "safe",
		Confidence:       0.78,
		ProposedSQL:      diagnoseFreezeBlockersSQL(),
		ExpiresAt:        &expires,
		OutputModes:      []string{"execute", "script"},
		RollbackClass:    "not_applicable",
		VerificationPlan: verificationPlanForAction("diagnose_freeze_blockers"),
	}
}

func autovacuumTuningCandidate(f SourceFinding) ActionCandidate {
	sql := strings.TrimSpace(f.RecommendedSQL)
	expires := time.Now().UTC().Add(72 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       "set_table_autovacuum",
		RiskTier:         "moderate",
		Confidence:       0.72,
		ProposedSQL:      sql,
		ExpiresAt:        &expires,
		OutputModes:      []string{"queue_for_approval", "generate_pr_or_script"},
		RollbackClass:    "forward_fix_only",
		VerificationPlan: verificationPlanForAction("set_table_autovacuum"),
	}
	candidate.ScriptOutput = scriptOutputFromFinding(f, candidate)
	return candidate
}

func diagnoseFreezeBlockersSQL() string {
	return `SELECT datname,
       age(datfrozenxid) AS database_xid_age
FROM pg_database
ORDER BY database_xid_age DESC;

SELECT pid,
       usename,
       application_name,
       backend_xmin,
       age(backend_xmin) AS xmin_age,
       state,
       now() - xact_start AS transaction_age,
       left(query, 1000) AS query
FROM pg_stat_activity
WHERE backend_xmin IS NOT NULL
ORDER BY age(backend_xmin) DESC
LIMIT 20;`
}

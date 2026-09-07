package cases

import (
	"fmt"
	"strings"
	"time"
)

func prepareQueryRewriteCandidate(h SourceQueryHint) ActionCandidate {
	expires := time.Now().UTC().Add(7 * 24 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       "prepare_query_rewrite",
		RiskTier:         "moderate",
		Confidence:       queryRewriteConfidence(h),
		ExpiresAt:        &expires,
		OutputModes:      []string{"generate_pr_or_script"},
		RollbackClass:    "application_rollback",
		VerificationPlan: queryRewriteVerificationPlan(),
		ScriptOutput:     queryRewriteScriptOutput(h),
	}
	return candidate
}

func retireQueryHintCandidate(h SourceQueryHint) ActionCandidate {
	expires := time.Now().UTC().Add(24 * time.Hour)
	return ActionCandidate{
		ActionType:    "retire_query_hint",
		RiskTier:      "safe",
		Confidence:    0.80,
		ExpiresAt:     &expires,
		OutputModes:   []string{"execute", "script"},
		RollbackClass: "reversible",
		VerificationPlan: []string{
			"verify hint no longer appears in active hints",
			"rerun query without forced planner directive",
		},
		ScriptOutput: &ScriptOutput{
			Filename:     fmt.Sprintf("retire_query_hint_%d.sql", h.QueryID),
			MigrationSQL: retireQueryHintSQL(h),
			RollbackSQL:  "-- Re-enable from action audit if the hint is still beneficial.",
			VerificationSQL: []string{
				fmt.Sprintf(
					"SELECT status FROM sage.query_hints WHERE queryid = %d;",
					h.QueryID,
				),
			},
			PRTitle: "Retire broken query hint",
			PRBody: "Generated from pg_sage query hint case. " +
				"The hint regressed during verification.",
			RiskLabels: []string{"safe", "reversible", "query_hint"},
			Format:     "sql",
		},
	}
}

func roleWorkMemCandidate(f SourceFinding) ActionCandidate {
	expires := time.Now().UTC().Add(7 * 24 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       "promote_role_work_mem",
		RiskTier:         "moderate",
		Confidence:       0.76,
		ProposedSQL:      strings.TrimSpace(f.RecommendedSQL),
		ExpiresAt:        &expires,
		OutputModes:      []string{"queue_for_approval", "generate_pr_or_script"},
		RollbackClass:    "reversible",
		VerificationPlan: roleWorkMemVerificationPlan(),
	}
	candidate.ScriptOutput = roleWorkMemScriptOutput(f, candidate)
	return candidate
}

func createStatisticsCandidate(f SourceFinding) ActionCandidate {
	expires := time.Now().UTC().Add(7 * 24 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       "create_statistics",
		RiskTier:         "moderate",
		Confidence:       0.74,
		ProposedSQL:      strings.TrimSpace(f.RecommendedSQL),
		ExpiresAt:        &expires,
		OutputModes:      []string{"queue_for_approval", "generate_pr_or_script"},
		RollbackClass:    "reversible",
		VerificationPlan: verificationPlanForAction("create_statistics"),
	}
	candidate.ScriptOutput = scriptOutputFromFinding(f, candidate)
	return candidate
}

func parameterizedQueryCandidate(f SourceFinding) ActionCandidate {
	expires := time.Now().UTC().Add(7 * 24 * time.Hour)
	candidate := ActionCandidate{
		ActionType:       "prepare_parameterized_query",
		RiskTier:         "moderate",
		Confidence:       0.68,
		ExpiresAt:        &expires,
		OutputModes:      []string{"generate_pr_or_script"},
		RollbackClass:    "application_rollback",
		VerificationPlan: verificationPlanForAction("prepare_parameterized_query"),
		ScriptOutput:     parameterizedQueryScriptOutput(f),
	}
	return candidate
}

func applyQueryHintCandidate(f SourceFinding) ActionCandidate {
	expires := time.Now().UTC().Add(24 * time.Hour)
	queryID := detailQueryID(f)
	candidate := ActionCandidate{
		ActionType:    "apply_query_hint",
		RiskTier:      "moderate",
		Confidence:    0.70,
		ProposedSQL:   strings.TrimSpace(f.RecommendedSQL),
		ExpiresAt:     &expires,
		OutputModes:   []string{"queue_for_approval", "generate_pr_or_script"},
		RollbackClass: "reversible",
		VerificationPlan: []string{
			"verify hint row exists in hint_plan.hints",
			"compare hinted and unhinted EXPLAIN costs",
			"monitor query latency and error rate after deployment",
		},
		BlockedReason: "planner hints require approval and revalidation",
		ScriptOutput: &ScriptOutput{
			Filename:     "apply_query_hint_" + queryID + ".sql",
			MigrationSQL: strings.TrimSpace(f.RecommendedSQL),
			RollbackSQL:  strings.TrimSpace(f.RollbackSQL),
			VerificationSQL: []string{
				"SELECT query_id FROM hint_plan.hints WHERE query_id = " +
					queryID + ";",
				"-- Rerun EXPLAIN for the affected query with pg_hint_plan enabled.",
			},
			PRTitle: "Review query hint for query " + queryID,
			PRBody: "Generated from pg_sage query tuner case " + f.ID +
				". Apply only after plan review and rollback readiness.",
			RiskLabels: []string{"moderate", "reversible", "query_tuning"},
			Format:     "sql",
		},
	}
	return candidate
}

func investigateQueryCandidate(f SourceFinding) ActionCandidate {
	expires := time.Now().UTC().Add(24 * time.Hour)
	query := detailString(f.Detail, "query", "")
	queryID := detailQueryID(f)
	candidate := ActionCandidate{
		ActionType:    "investigate_query_plan",
		RiskTier:      "safe",
		Confidence:    0.65,
		ExpiresAt:     &expires,
		OutputModes:   []string{"generate_pr_or_script"},
		RollbackClass: "not_applicable",
		VerificationPlan: []string{
			"capture EXPLAIN plan for the query",
			"identify missing index, stale stats, memory spill, or query rewrite path",
			"rerun pg_sage after remediation to confirm the case clears",
		},
		ScriptOutput: queryInvestigationScriptOutput(f, query, queryID),
	}
	return candidate
}

func queryRewriteConfidence(h SourceQueryHint) float64 {
	if h.BeforeCost != nil && h.AfterCost != nil && *h.BeforeCost > *h.AfterCost {
		return 0.78
	}
	return 0.65
}

func queryRewriteScriptOutput(h SourceQueryHint) *ScriptOutput {
	queryID := fmt.Sprintf("%d", h.QueryID)
	return &ScriptOutput{
		Filename:     "query_rewrite_" + queryID + ".sql",
		MigrationSQL: queryRewriteBody(h),
		RollbackSQL:  "-- Revert the application query change from version control.",
		VerificationSQL: []string{
			"-- Run EXPLAIN (ANALYZE, BUFFERS) for old and rewritten query.",
			"-- Compare latency, row estimates, temp IO, and plan stability.",
		},
		PRTitle: "Review query rewrite for query " + queryID,
		PRBody: "Generated from pg_sage query tuning case. " +
			"Review the application query rewrite before deployment.",
		RiskLabels: []string{"moderate", "application_rollback", "query_rewrite"},
		Format:     "sql",
	}
}

func queryRewriteBody(h SourceQueryHint) string {
	var b strings.Builder
	b.WriteString("-- Query rewrite candidate generated by pg_sage\n")
	if h.HintText != "" {
		b.WriteString("-- Existing hint: ")
		b.WriteString(h.HintText)
		b.WriteString("\n")
	}
	if h.RewriteRationale != "" {
		b.WriteString("-- Rationale: ")
		b.WriteString(strings.ReplaceAll(h.RewriteRationale, "\n", " "))
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimSpace(h.SuggestedRewrite))
	b.WriteString("\n")
	return b.String()
}

func retireQueryHintSQL(h SourceQueryHint) string {
	return fmt.Sprintf(
		"UPDATE sage.query_hints SET status = 'retired' "+
			"WHERE queryid = %d AND hint_text = %s;",
		h.QueryID,
		sqlLiteral(h.HintText),
	)
}

func roleWorkMemScriptOutput(
	f SourceFinding,
	candidate ActionCandidate,
) *ScriptOutput {
	return &ScriptOutput{
		Filename:     scriptFilename(f, candidate),
		MigrationSQL: candidate.ProposedSQL,
		RollbackSQL:  "ALTER ROLE " + f.ObjectIdentifier + " RESET work_mem;",
		VerificationSQL: []string{
			"SELECT rolconfig FROM pg_roles WHERE rolname = " +
				sqlLiteral(f.ObjectIdentifier) + ";",
			"-- Confirm representative query temp IO decreases without memory pressure.",
		},
		PRTitle: "Review role work_mem promotion: " + f.ObjectIdentifier,
		PRBody: "Generated from pg_sage query tuning case. " +
			"Promotes repeated per-query work_mem hints to a reviewed role default.",
		RiskLabels: []string{"moderate", "reversible", "query_tuning"},
		Format:     "sql",
	}
}

func parameterizedQueryScriptOutput(f SourceFinding) *ScriptOutput {
	query := detailString(f.Detail, "normalized_query", f.ObjectIdentifier)
	return &ScriptOutput{
		Filename: "parameterize_query_" + sanitizeScriptPart(f.ID) + ".sql",
		MigrationSQL: "-- Application query parameterization plan generated by pg_sage\n" +
			"-- Normalized query shape:\n-- " +
			strings.ReplaceAll(query, "\n", "\n-- ") + "\n",
		RollbackSQL: "-- Revert the application query change from version control.",
		VerificationSQL: []string{
			"-- Compare pg_stat_statements entries before and after deployment.",
			"-- Confirm queryid churn decreases and latency remains stable.",
		},
		PRTitle: "Review parameterized query change",
		PRBody: "Generated from pg_sage query tuning case " + f.ID +
			". Verify semantics and plan stability before deployment.",
		RiskLabels: []string{"moderate", "application_rollback", "query_tuning"},
		Format:     "sql",
	}
}

func queryInvestigationScriptOutput(
	f SourceFinding,
	query string,
	queryID string,
) *ScriptOutput {
	if query == "" {
		query = "-- Query text was not captured; use pg_stat_statements queryid " +
			queryID + " to locate it."
	}
	return &ScriptOutput{
		Filename: "investigate_query_" + queryID + ".sql",
		MigrationSQL: "-- Read-only query investigation generated by pg_sage\n" +
			"EXPLAIN (ANALYZE, BUFFERS, VERBOSE)\n" +
			strings.TrimSpace(query) + ";\n",
		RollbackSQL: "-- No rollback required for read-only investigation.",
		VerificationSQL: []string{
			"-- Attach EXPLAIN output to the case.",
			"-- Confirm follow-up action is index, stats, memory, or query rewrite.",
		},
		PRTitle: "Investigate query plan for query " + queryID,
		PRBody: "Generated from pg_sage case " + f.ID +
			". This is a read-only investigation artifact.",
		RiskLabels: []string{"safe", "not_applicable", "query_investigation"},
		Format:     "sql",
	}
}

func detailQueryID(f SourceFinding) string {
	switch v := f.Detail["queryid"].(type) {
	case int64:
		return fmt.Sprintf("%d", v)
	case int:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%.0f", v)
	case string:
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	id := strings.TrimPrefix(f.ObjectIdentifier, "queryid:")
	if id == "" {
		return sanitizeScriptPart(f.ID)
	}
	return id
}

func queryRewriteVerificationPlan() []string {
	return []string{
		"compare old and rewritten EXPLAIN plans in CI or staging",
		"verify row counts and semantics match",
		"monitor latency, temp IO, and error rate after deployment",
	}
}

func roleWorkMemVerificationPlan() []string {
	return []string{
		"verify role relconfig contains expected work_mem",
		"confirm representative queries stop spilling to temp",
		"monitor role-level memory pressure after deployment",
	}
}

func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

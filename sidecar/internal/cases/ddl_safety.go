package cases

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

func enrichDDLSafetyCandidate(
	f SourceFinding,
	candidate ActionCandidate,
) ActionCandidate {
	if candidate.ActionType == "create_index_concurrently" && indexNameFromCreateIndex(candidate.ProposedSQL) == "" {
		candidate.RollbackClass = "unknown"
		candidate.RecoveryDetail = "The created index identity is not established. Capture the actual schema and index name before preparing recovery or catalog verification; no name has been guessed."
	}
	if candidate.ActionType == "drop_unused_index" && !validatedIndexRecreation(f.RollbackSQL, candidate.ProposedSQL) {
		candidate.RollbackClass = "unknown"
		candidate.RecoveryDetail = "No validated index recreation procedure is attached. Capture and review the complete original definition before dropping this index."
	}
	candidate.DDLPreflight = ddlPreflightFromFinding(f)
	candidate.ScriptOutput = scriptOutputFromFinding(f, candidate)
	if candidate.BlockedReason == "" && candidate.ActionType == "ddl_preflight" {
		candidate.BlockedReason = "direct execution blocked; generate PR or script"
	}
	if candidate.BlockedReason == "" && hasLiveDDLWarnings(candidate.DDLPreflight) {
		candidate.BlockedReason = "live DDL preflight has warnings; review required"
	}
	return candidate
}

func ddlPreflightFromFinding(f SourceFinding) *DDLPreflightReport {
	d := f.Detail
	ruleID := detailString(d, "rule_id", f.RuleID)
	lockLevel := lockLevelForRule(ruleID, f.RecommendedSQL)
	requiresRewrite := detailBool(d, "requires_rewrite",
		strings.Contains(ruleID, "rewrite"))
	riskScore := detailFloat(d, "risk_score", 0)
	table := detailString(d, "affected_table",
		detailString(d, "table_name", f.ObjectIdentifier))
	report := &DDLPreflightReport{
		Summary:         ddlPreflightSummary(lockLevel, requiresRewrite),
		RiskScore:       riskScore,
		LockLevel:       lockLevel,
		RequiresRewrite: requiresRewrite,
		TableName:       table,
		TableSizeBytes:  detailInt64(d, "table_size_bytes", 0),
		EstimatedRows:   detailInt64(d, "estimated_rows", 0),
		ActiveQueries:   int(detailInt64(d, "active_queries", 0)),
		PendingLocks:    int(detailInt64(d, "pending_locks", 0)),
		ReplicationLag:  detailFloat(d, "replication_lag", 0),
		EstimatedLockMs: detailInt64(d, "lock_timeout_ms",
			detailInt64(d, "estimated_lock_ms", 0)),
	}
	report.Checks = ddlPreflightChecks(report)
	applyPreflightEvidencePresence(report, f, ruleID)
	return report
}

func applyPreflightEvidencePresence(report *DDLPreflightReport, f SourceFinding, ruleID string) {
	_, rewriteReported := f.Detail["requires_rewrite"].(bool)
	concurrentPath := concurrentIndexDDL.MatchString(f.RecommendedSQL) ||
		(strings.HasPrefix(strings.ToUpper(strings.TrimSpace(f.RecommendedSQL)), "REINDEX ") && strings.Contains(strings.ToUpper(f.RecommendedSQL), " CONCURRENTLY "))
	rewriteKnown := rewriteReported || strings.Contains(ruleID, "rewrite") || concurrentPath
	if !rewriteKnown {
		report.Summary = "Lock and rewrite risks require assessment"
		if report.LockLevel != "" {
			report.Summary = report.LockLevel + " lock estimate; rewrite not assessed"
		}
	}
	measurementKeys := map[string][]string{
		"activity": {"active_queries"}, "pending_locks": {"pending_locks"},
		"replica_lag": {"replication_lag"}, "table_size": {"table_size_bytes"},
		"lock_timeout": {"lock_timeout_ms", "estimated_lock_ms"},
	}
	for i := range report.Checks {
		check := &report.Checks[i]
		switch check.Name {
		case "lock_level":
			if report.LockLevel == "" {
				check.Status, check.Detail = "unknown", "Lock level has not been established."
			} else {
				check.Detail += " (estimated from proposed DDL; live lock conditions require separate measurement)"
			}
		case "rewrite":
			switch {
			case !rewriteKnown:
				check.Status, check.Detail = "unknown", "Table rewrite risk has not been assessed."
			case !report.RequiresRewrite && rewriteReported:
				check.Detail = "Reported as not requiring a table rewrite."
			case !report.RequiresRewrite && concurrentPath:
				check.Detail = "Concurrent index operation; a table rewrite is not expected."
			}
		default:
			keys, ok := measurementKeys[check.Name]
			if !ok {
				continue
			}
			measured := false
			for _, key := range keys {
				value := detailFloat(f.Detail, key, math.NaN())
				if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
					measured = true
					break
				}
			}
			if !measured {
				check.Status, check.Detail = "unknown", "Not measured in the source evidence."
			} else if check.Name == "table_size" && report.TableSizeBytes == 0 {
				check.Status, check.Detail = "pass", "0 bytes"
			}
		}
	}
}

func hasLiveDDLWarnings(report *DDLPreflightReport) bool {
	if report == nil {
		return false
	}
	for _, check := range report.Checks {
		switch check.Name {
		case "activity", "pending_locks", "replica_lag", "table_size",
			"lock_timeout":
			if check.Status == "warn" {
				return true
			}
		}
	}
	return false
}

func ddlPreflightSummary(lockLevel string, rewrite bool) string {
	if lockLevel == "" {
		if rewrite {
			return "Table rewrite likely; lock level not assessed"
		}
		return "Lock level not assessed"
	}
	if rewrite {
		return fmt.Sprintf("%s rewrite risk", lockLevel)
	}
	return fmt.Sprintf("%s lock preflight", lockLevel)
}

func ddlPreflightChecks(r *DDLPreflightReport) []PreflightCheck {
	checks := []PreflightCheck{
		{Name: "lock_level", Status: checkStatus(r.LockLevel != ""),
			Detail: r.LockLevel},
		{Name: "rewrite", Status: rewriteStatus(r.RequiresRewrite),
			Detail: rewriteDetail(r.RequiresRewrite)},
		{Name: "activity", Status: activityStatus(r.ActiveQueries),
			Detail: fmt.Sprintf("%d active queries", r.ActiveQueries)},
		{Name: "pending_locks", Status: activityStatus(r.PendingLocks),
			Detail: fmt.Sprintf("%d pending locks", r.PendingLocks)},
		{Name: "replica_lag", Status: lagStatus(r.ReplicationLag),
			Detail: fmt.Sprintf("%.0fs", r.ReplicationLag)},
		{Name: "table_size", Status: tableSizeStatus(r.TableSizeBytes),
			Detail: formatBytes(r.TableSizeBytes)},
		{Name: "lock_timeout", Status: checkStatus(r.EstimatedLockMs > 0),
			Detail: fmt.Sprintf("%dms", r.EstimatedLockMs)},
	}
	return checks
}

func checkStatus(ok bool) string {
	if ok {
		return "pass"
	}
	return "warn"
}

func rewriteStatus(rewrite bool) string {
	if rewrite {
		return "warn"
	}
	return "pass"
}

func rewriteDetail(rewrite bool) string {
	if rewrite {
		return "table rewrite likely"
	}
	return "metadata-only or concurrent path"
}

func activityStatus(value int) string {
	if value > 0 {
		return "warn"
	}
	return "pass"
}

func lagStatus(value float64) string {
	if value > 30 {
		return "warn"
	}
	return "pass"
}

func tableSizeStatus(value int64) string {
	if value <= 0 {
		return "unknown"
	}
	if value > 10*1024*1024*1024 {
		return "warn"
	}
	return "pass"
}

func formatBytes(value int64) string {
	if value <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d bytes", value)
}

func scriptOutputFromFinding(
	f SourceFinding,
	candidate ActionCandidate,
) *ScriptOutput {
	d := f.Detail
	sql := strings.TrimSpace(candidate.ProposedSQL)
	if sql == "" {
		sql = forwardFixScript(f)
	}
	return &ScriptOutput{
		Filename:        scriptFilename(f, candidate),
		MigrationSQL:    sql,
		RollbackSQL:     rollbackSQLForFinding(f, candidate),
		VerificationSQL: verificationSQLForFinding(f, candidate),
		PRTitle:         prTitleForFinding(f),
		PRBody:          prBodyForFinding(f, candidate),
		RiskLabels:      riskLabelsForFinding(d, candidate),
		Format:          "sql",
	}
}

func rollbackSQLForFinding(f SourceFinding, candidate ActionCandidate) string {
	if candidate.ActionType == "drop_unused_index" && candidate.RollbackClass == "reversible" && validatedIndexRecreation(f.RollbackSQL, candidate.ProposedSQL) {
		return strings.TrimSpace(f.RollbackSQL)
	}
	return rollbackSQLForCandidate(candidate)
}

func validatedIndexRecreation(create, drop string) bool {
	return indexNameFromCreateIndex(create) != "" && recommendation.InverseDropsCreatedIndex(create, drop)
}

func forwardFixScript(f SourceFinding) string {
	safe := detailString(f.Detail, "safe_alternative", "")
	if safe == "" {
		safe = "Write an online, forward-fix migration for this DDL."
	}
	original := detailString(f.Detail, "original_sql", "")
	var b strings.Builder
	b.WriteString("-- Forward-fix migration plan generated by pg_sage\n")
	if original != "" {
		b.WriteString("-- Original unsafe DDL:\n-- ")
		b.WriteString(original)
		b.WriteString("\n")
	}
	b.WriteString("-- Safe rollout plan:\n")
	b.WriteString("-- ")
	b.WriteString(strings.ReplaceAll(safe, "\n", "\n-- "))
	b.WriteString("\n")
	return b.String()
}

func scriptFilename(f SourceFinding, candidate ActionCandidate) string {
	id := strings.TrimSpace(f.ID)
	if id == "" {
		id = "case"
	}
	action := strings.TrimSpace(candidate.ActionType)
	if action == "" {
		action = "ddl_preflight"
	}
	return id + "_" + action + ".sql"
}

func rollbackSQLForCandidate(candidate ActionCandidate) string {
	if candidate.RollbackClass != "reversible" {
		return ""
	}
	sql := strings.TrimSpace(candidate.ProposedSQL)
	if strings.HasPrefix(strings.ToUpper(sql), "CREATE INDEX CONCURRENTLY ") {
		if name := indexNameFromCreateIndex(sql); name != "" {
			return "DROP INDEX CONCURRENTLY " + name + ";"
		}
	}
	if c, err := extstats.ParseCreate(sql); err == nil {
		return c.Rollback() + ";"
	}
	return ""
}

func indexNameFromCreateIndex(sql string) string {
	// IF NOT EXISTS may leave a pre-existing object untouched; neither its
	// ownership nor its definition is evidence that this action created it.
	if createIndexIfNotExists.MatchString(sql) || strings.Contains(strings.TrimSuffix(strings.TrimSpace(sql), ";"), ";") {
		return ""
	}
	spec, err := optimizer.ParseIndexDDL(sql)
	if err != nil || spec.Name == "" {
		return ""
	}
	if spec.TableSchema != "" {
		return sanitize.QuoteQualifiedName(spec.TableSchema, spec.Name)
	}
	return sanitize.QuoteIdentifier(spec.Name)
}

var createIndexIfNotExists = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?IF\s+NOT\s+EXISTS\b`)
var concurrentIndexDDL = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\b`)

func verificationSQLForFinding(
	f SourceFinding,
	candidate ActionCandidate,
) []string {
	switch candidate.ActionType {
	case "create_index_concurrently":
		if name := indexNameFromCreateIndex(candidate.ProposedSQL); name != "" {
			out := []string{"SELECT indisvalid, indisready FROM pg_index WHERE indexrelid = " + sanitize.QuoteLiteral(name) + "::regclass;"}
			if sample := detailString(f.Detail, "sample_query", ""); sample != "" {
				out = append(out, "EXPLAIN "+sample)
			} else {
				out = append(out, "-- Capture a representative affected query before comparing plans.")
			}
			return out
		}
		return []string{"-- Capture the actual created index schema and name before catalog verification.", "-- Compare a representative affected query plan and latency; improvement is not yet established."}
	case "drop_unused_index":
		return []string{
			"SELECT to_regclass(" + sanitize.QuoteLiteral(f.ObjectIdentifier) + ");",
			"-- Confirm application error rate and query latency stay normal.",
		}
	}
	return []string{
		"-- Verify catalog shape matches the intended migration outcome.",
		"-- Rerun pg_sage analyzers and confirm the case no longer fires.",
	}
}

func prTitleForFinding(f SourceFinding) string {
	if f.Title != "" {
		return "Review DDL safety plan: " + f.Title
	}
	return "Review DDL safety plan"
}

func prBodyForFinding(f SourceFinding, candidate ActionCandidate) string {
	rollback := candidate.RollbackClass
	if rollback == "" {
		rollback = "unknown"
	}
	return fmt.Sprintf(
		"Generated from pg_sage case %s. Risk tier: %s. Rollback class: %s.",
		f.ID, candidate.RiskTier, rollback,
	)
}

func riskLabelsForFinding(
	detail map[string]any,
	candidate ActionCandidate,
) []string {
	labels := []string{candidate.RiskTier, candidate.RollbackClass}
	if rule := detailString(detail, "rule_id", ""); rule != "" {
		labels = append(labels, rule)
	}
	return labels
}

func lockLevelForRule(ruleID string, sql string) string {
	upperSQL := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case concurrentIndexDDL.MatchString(sql):
		return "SHARE UPDATE EXCLUSIVE"
	case strings.HasPrefix(upperSQL, "REINDEX ") &&
		strings.Contains(upperSQL, " CONCURRENTLY "):
		return "SHARE UPDATE EXCLUSIVE"
	case strings.Contains(ruleID, "index_not_concurrent"):
		return "SHARE"
	case strings.Contains(ruleID, "constraint_not_valid"):
		return "SHARE ROW EXCLUSIVE"
	case strings.HasPrefix(upperSQL, "ALTER TABLE "):
		return "ACCESS EXCLUSIVE"
	default:
		return ""
	}
}

func detailString(detail map[string]any, key, fallback string) string {
	if detail == nil {
		return fallback
	}
	if v, ok := detail[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func detailBool(detail map[string]any, key string, fallback bool) bool {
	if detail == nil {
		return fallback
	}
	if v, ok := detail[key].(bool); ok {
		return v
	}
	return fallback
}

func detailFloat(detail map[string]any, key string, fallback float64) float64 {
	if detail == nil {
		return fallback
	}
	switch v := detail[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return fallback
	}
}

func detailInt64(detail map[string]any, key string, fallback int64) int64 {
	if detail == nil {
		return fallback
	}
	switch v := detail[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return fallback
	}
}

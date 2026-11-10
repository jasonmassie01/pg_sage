package cases

import (
	"math"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/migration/ddlrules"
)

// Where a preflight's lock level or rewrite came from.
const (
	fromCandidate = "candidate" // the candidate's own concurrent SQL
	fromDetail    = "detail"    // classified by the migration advisor at detection
	fromCatalog   = "catalog"   // the rule catalog or the proposed DDL's shape
)

// measurementKeys are the detail keys that measure each live check.
var measurementKeys = map[string][]string{
	"activity": {"active_queries"}, "pending_locks": {"pending_locks"},
	"replica_lag": {"replication_lag"}, "table_size": {"table_size_bytes"},
	"lock_timeout": {"lock_timeout_ms", "estimated_lock_ms"},
}

func ddlPreflightFromFinding(f SourceFinding) *DDLPreflightReport {
	d := f.Detail
	ruleID := detailString(d, "rule_id", f.RuleID)
	lockLevel, lockSource := preflightLockLevel(f, ruleID)
	requiresRewrite, rewriteSource := preflightRewrite(f, ruleID)
	table := detailString(d, "affected_table",
		detailString(d, "table_name", f.ObjectIdentifier))
	report := &DDLPreflightReport{
		Summary:         ddlPreflightSummary(lockLevel, requiresRewrite),
		RiskScore:       detailFloat(d, "risk_score", 0),
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
		MeasuredAt: measuredAtFromDetail(d),
	}
	report.Checks = ddlPreflightChecks(report)
	applyLockEvidence(report, lockSource, rewriteSource)
	applyMeasurementEvidence(report, d)
	return report
}

// preflightLockLevel is the lock the candidate takes: its own concurrent
// SQL's lock, else the level the migration advisor classified at
// detection, else the rule catalog's (migration-01).
func preflightLockLevel(f SourceFinding, ruleID string) (string, string) {
	if level := concurrentLockLevel(f.RecommendedSQL); level != "" {
		return level, fromCandidate
	}
	if level := detailString(f.Detail, "lock_level", ""); knownLockLevel(level) {
		return level, fromDetail
	}
	return lockLevelForRule(ruleID, f.RecommendedSQL), fromCatalog
}

// preflightRewrite is whether the candidate rewrites the table and where
// that came from; "" means it has not been assessed.
func preflightRewrite(f SourceFinding, ruleID string) (bool, string) {
	if concurrentLockLevel(f.RecommendedSQL) != "" {
		return false, fromCandidate
	}
	if v, ok := f.Detail["requires_rewrite"].(bool); ok {
		return v, fromDetail
	}
	if r, ok := ddlrules.Lookup(ruleID); ok {
		return r.RequiresRewrite, fromCatalog
	}
	return false, ""
}

// lockLevelForRule is the lock for a candidate whose source finding did
// not classify one: the concurrent forms' own lock, else the migration
// rule catalog's level, else ACCESS EXCLUSIVE for any other ALTER TABLE.
func lockLevelForRule(ruleID string, sql string) string {
	if level := concurrentLockLevel(sql); level != "" {
		return level
	}
	if r, ok := ddlrules.Lookup(ruleID); ok && r.LockLevel != "" {
		return r.LockLevel
	}
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "ALTER TABLE ") {
		return "ACCESS EXCLUSIVE"
	}
	return ""
}

func concurrentLockLevel(sql string) string {
	upperSQL := strings.ToUpper(strings.TrimSpace(sql))
	if concurrentIndexDDL.MatchString(sql) ||
		(strings.HasPrefix(upperSQL, "REINDEX ") &&
			strings.Contains(upperSQL, " CONCURRENTLY ")) {
		return "SHARE UPDATE EXCLUSIVE"
	}
	return ""
}

func knownLockLevel(level string) bool {
	switch level {
	case "ACCESS SHARE", "ROW SHARE", "ROW EXCLUSIVE", "SHARE UPDATE EXCLUSIVE",
		"SHARE", "SHARE ROW EXCLUSIVE", "EXCLUSIVE", "ACCESS EXCLUSIVE":
		return true
	}
	return false
}

// measuredAtFromDetail is the detail's measured_at, normalised to RFC
// 3339 UTC; "" when absent or not a time.
func measuredAtFromDetail(d map[string]any) string {
	at, err := time.Parse(time.RFC3339, detailString(d, "measured_at", ""))
	if err != nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func applyLockEvidence(report *DDLPreflightReport, lockSource, rewriteSource string) {
	if rewriteSource == "" {
		report.Summary = "Lock and rewrite risks require assessment"
		if report.LockLevel != "" {
			report.Summary = report.LockLevel + " lock estimate; rewrite not assessed"
		}
	}
	for i := range report.Checks {
		check := &report.Checks[i]
		switch check.Name {
		case "lock_level":
			switch {
			case report.LockLevel == "":
				check.Status, check.Detail = "unknown", "Lock level has not been established."
			case lockSource == fromDetail:
				check.Detail += " (classified at detection)"
			default:
				check.Detail += " (estimated from proposed DDL; live lock conditions " +
					"require separate measurement)"
			}
		case "rewrite":
			applyRewriteEvidence(check, report.RequiresRewrite, rewriteSource)
		}
	}
}

func applyRewriteEvidence(check *PreflightCheck, rewrite bool, source string) {
	switch {
	case source == "":
		check.Status, check.Detail = "unknown", "Table rewrite risk has not been assessed."
	case rewrite:
	case source == fromDetail:
		check.Detail = "Reported as not requiring a table rewrite."
	case source == fromCandidate:
		check.Detail = "Concurrent index operation; a table rewrite is not expected."
	}
}

// applyMeasurementEvidence marks each live check unknown unless the
// detail measured it, and notes that measured values are from detection.
func applyMeasurementEvidence(report *DDLPreflightReport, d map[string]any) {
	known, reported := d["stats_known"].(bool)
	statsUnknown := reported && !known
	for i := range report.Checks {
		check := &report.Checks[i]
		keys, ok := measurementKeys[check.Name]
		if !ok {
			continue
		}
		switch {
		case !measuredIn(d, keys):
			check.Status, check.Detail = "unknown", "Not measured in the source evidence."
			continue
		case check.Name == "table_size" && statsUnknown:
			check.Status, check.Detail = "unknown", "Table statistics were unknown at "+
				"detection (never analyzed, or an ambiguous table name)."
			continue
		case check.Name == "table_size" && report.TableSizeBytes == 0:
			check.Status, check.Detail = "pass", "0 bytes"
		}
		check.Detail += evidenceSuffix(check.Name, d, report.MeasuredAt != "")
	}
}

// evidenceSuffix says where a live check's value came from. A lock_timeout
// check read only from estimated_lock_ms rests on the advisor's heuristic
// lock-hold estimate, which is never a measurement.
func evidenceSuffix(name string, d map[string]any, atDetection bool) string {
	estimate := name == "lock_timeout" && !measuredIn(d, []string{"lock_timeout_ms"})
	switch {
	case estimate && atDetection:
		return " (estimated at detection)"
	case estimate:
		return " (estimated)"
	case atDetection:
		return " (measured at detection)"
	}
	return ""
}

func measuredIn(d map[string]any, keys []string) bool {
	for _, key := range keys {
		value := detailFloat(d, key, math.NaN())
		if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
			return true
		}
	}
	return false
}

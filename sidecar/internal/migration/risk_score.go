package migration

import "math"

// lockLevelWeight returns the weight [0,1] for the given lock level.
func lockLevelWeight(level string) float64 {
	switch level {
	case "ACCESS EXCLUSIVE":
		return 1.0
	case "SHARE ROW EXCLUSIVE":
		return 0.7
	case "SHARE":
		return 0.5
	case "SHARE UPDATE EXCLUSIVE":
		return 0.3
	default:
		return 0.0
	}
}

// rewriteWeight classifies the operation impact.
func rewriteWeight(risk *DDLRisk) float64 {
	if risk.RequiresRewrite {
		return 1.0
	}
	// Metadata-only operations: DROP COLUMN, SET NOT NULL (PG12+),
	// ATTACH PARTITION. Everything else that takes a heavy lock scans.
	switch risk.RuleID {
	case "ddl_drop_column", "ddl_drop_table", "ddl_missing_lock_timeout",
		"ddl_attach_partition_no_check":
		return 0.2
	default:
		return 0.6
	}
}

// computeRiskScore implements the risk formula from the spec.
func computeRiskScore(risk *DDLRisk) float64 {
	llw := lockLevelWeight(risk.LockLevel)
	rw := rewriteWeight(risk)
	baseRisk := llw * rw

	tableFactor := 0.0
	if risk.EstimatedRows > 0 {
		tableFactor = math.Min(
			math.Log10(float64(risk.EstimatedRows))/10.0, 1.0)
	}
	activityFactor := math.Min(float64(risk.ActiveQueries)/100.0, 1.0)
	replFactor := math.Min(risk.ReplicationLag/30.0, 1.0)
	lockQueueFactor := math.Min(float64(risk.PendingLocks)/10.0, 1.0)

	combined := 0.4*tableFactor +
		0.3*activityFactor +
		0.2*lockQueueFactor +
		0.1*replFactor

	return baseRisk * math.Max(0.1, combined)
}

// estimateLockDuration provides a rough lock duration estimate in ms.
// This is a heuristic — actual duration depends on many factors.
func estimateLockDuration(risk *DDLRisk) int64 {
	if risk.RequiresRewrite && risk.TableSizeBytes > 0 {
		// Rough estimate: 50MB/s rewrite speed
		ms := (float64(risk.TableSizeBytes) / (50 * 1024 * 1024)) * 1000
		return int64(math.Max(ms, 100))
	}
	// Metadata-only: near instant
	if risk.LockLevel == "ACCESS EXCLUSIVE" {
		return 100
	}
	return 50
}

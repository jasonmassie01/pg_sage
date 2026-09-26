package migration

import "math"

// incidentThreshold is the score an assessment must exceed to be
// reported. A rule whose intrinsic hazard equals the threshold (plain
// CREATE INDEX) is reported only once size or activity escalates it.
const incidentThreshold = 0.3

// defaultDDLRowThreshold is used when migration.ddl_row_threshold is
// unset (config doc: "Default: 10000").
const defaultDDLRowThreshold = 10000

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
	// Metadata-only operations: DROP COLUMN, binary-coercible ALTER TYPE,
	// ATTACH PARTITION. Everything else that takes a heavy lock scans.
	switch risk.RuleID {
	case "ddl_drop_column", "ddl_drop_table", "ddl_missing_lock_timeout",
		"ddl_attach_partition_no_check", "ddl_alter_type_rewrite":
		return 0.2
	default:
		return 0.6
	}
}

// computeRiskScore scores a classified DDL in [0,1]. The rule's
// intrinsic hazard (lock weight x rewrite weight) is the floor; table
// size, concurrent activity, lock queue and replication lag only
// escalate it toward 1.0 (G7-B03). The previous multiplicative formula
// capped non-concurrent CREATE INDEX at exactly the 0.3 cut-off and
// scored VACUUM FULL / CLUSTER / REINDEX / REFRESH at <= 0.06.
func computeRiskScore(risk *DDLRisk) float64 {
	base := lockLevelWeight(risk.LockLevel) * rewriteWeight(risk)
	if base <= 0 {
		return 0
	}
	return math.Min(1.0, base+(1-base)*escalationFactor(risk))
}

// escalationFactor combines the live signals into [0,1].
func escalationFactor(risk *DDLRisk) float64 {
	tableFactor := 0.0
	if risk.EstimatedRows > 0 {
		tableFactor = math.Min(
			math.Log10(float64(risk.EstimatedRows))/10.0, 1.0)
	}
	activityFactor := math.Min(float64(risk.ActiveQueries)/100.0, 1.0)
	replFactor := math.Min(math.Max(risk.ReplicationLag, 0)/30.0, 1.0)
	lockQueueFactor := math.Min(float64(risk.PendingLocks)/10.0, 1.0)
	return 0.4*tableFactor +
		0.3*activityFactor +
		0.2*lockQueueFactor +
		0.1*replFactor
}

// capSmallIdleTable keeps DDL on a known-small, idle table at or below
// the reporting threshold. The DDL's own backend may be the one active
// query touching the table, so up to one active query counts as idle.
func capSmallIdleTable(risk *DDLRisk, rowThreshold int64) {
	if !risk.StatsKnown || risk.EstimatedRows >= rowThreshold {
		return
	}
	if risk.PendingLocks > 0 || risk.ActiveQueries > 1 {
		return
	}
	risk.RiskScore = math.Min(risk.RiskScore, incidentThreshold)
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

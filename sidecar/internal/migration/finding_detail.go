package migration

import "time"

// addRiskMeasurements writes risk's classification and detection-time
// measurements into a finding detail under the keys the case preflight
// reads (migration-01). A live read that failed is left out, so the
// preflight reports it unknown instead of a passing zero; nil writes
// nothing.
func addRiskMeasurements(detail map[string]any, risk *DDLRisk) {
	if risk == nil {
		return
	}
	if risk.LockLevel != "" {
		detail["lock_level"] = risk.LockLevel
	}
	detail["requires_rewrite"] = risk.RequiresRewrite
	detail["stats_known"] = risk.StatsKnown
	if table := qualifiedName(risk.SchemaName, risk.TableName); table != "" {
		detail["affected_table"] = table
	}
	m := risk.Measured
	if m.TableStats {
		detail["table_size_bytes"] = risk.TableSizeBytes
		detail["estimated_rows"] = risk.EstimatedRows
	}
	if m.Activity {
		detail["active_queries"] = risk.ActiveQueries
		detail["longest_query_sec"] = risk.LongestQuerySec
	}
	if m.PendingLocks {
		detail["pending_locks"] = risk.PendingLocks
	}
	if m.ReplicationLag {
		detail["replication_lag"] = risk.ReplicationLag
	}
	if lockEstimateGrounded(risk) {
		detail["estimated_lock_ms"] = risk.EstimatedLockMs
	}
	if !risk.MeasuredAt.IsZero() {
		detail["measured_at"] = risk.MeasuredAt.UTC().Format(time.RFC3339)
	}
}

// lockEstimateGrounded reports whether the lock-duration estimate rests on
// a live assessment: a rewrite's estimate needs the measured, analysed
// table size; a metadata-only estimate needs only the live assessment.
func lockEstimateGrounded(risk *DDLRisk) bool {
	if risk.MeasuredAt.IsZero() {
		return false
	}
	if !risk.RequiresRewrite {
		return true
	}
	return risk.Measured.TableStats && risk.StatsKnown
}

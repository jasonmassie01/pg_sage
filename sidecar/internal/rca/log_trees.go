package rca

import "fmt"

// ---------------------------------------------------------------------------
// Log-based decision trees: map log-originated signals to Incidents
// ---------------------------------------------------------------------------

// runLogDecisionTrees processes log-based signals and produces Incidents.
// Each signal maps to a straightforward incident (no complex branching).
func (e *Engine) runLogDecisionTrees(signals []*Signal) []Incident {
	sigMap := make(map[string]*Signal, len(signals))
	for _, s := range signals {
		sigMap[s.ID] = s
	}

	var incidents []Incident
	incidents = append(incidents, e.logTreesCriticalP0(sigMap)...)
	incidents = append(incidents, e.logTreesWarningP1(sigMap)...)
	return incidents
}

// logTreesCriticalP0 handles the P0 critical log signals.
func (e *Engine) logTreesCriticalP0(
	sigMap map[string]*Signal,
) []Incident {
	var out []Incident
	if s, ok := sigMap["log_deadlock_detected"]; ok {
		out = append(out, logTreeDeadlock(s))
	}
	if s, ok := sigMap["log_connection_refused"]; ok {
		out = append(out, logTreeSimple(s, "critical",
			"Connection refused: too many clients already", "high_risk"))
	}
	if s, ok := sigMap["log_out_of_memory"]; ok {
		out = append(out, logTreeOOM(s))
	}
	if s, ok := sigMap["log_disk_full"]; ok {
		out = append(out, logTreeSimple(s, "critical",
			"Disk full: no space left on device", "high_risk"))
	}
	if s, ok := sigMap["log_panic_server_crash"]; ok {
		out = append(out, logTreeSimple(s, "critical",
			"PostgreSQL server crash (PANIC or signal)", "high_risk"))
	}
	if s, ok := sigMap["log_data_corruption"]; ok {
		out = append(out, logTreeSimple(s, "critical",
			"Data corruption detected (class XX error)", "high_risk"))
	}
	if s, ok := sigMap["log_txid_wraparound_warning"]; ok {
		out = append(out, logTreeTxidWraparound(s))
	}
	if s, ok := sigMap["log_archive_failed"]; ok {
		out = append(out, logTreeSimple(s, "critical",
			"WAL archive command failed", "moderate"))
	}
	if s, ok := sigMap["log_temp_file_created"]; ok {
		out = append(out, logTreeTempFile(s))
	}
	if s, ok := sigMap["log_checkpoint_too_frequent"]; ok {
		out = append(out, logTreeSimple(s, "warning",
			"Checkpoints occurring too frequently "+
				"-- consider increasing checkpoint_completion_target "+
				"or max_wal_size", "safe"))
	}
	return out
}

// logTreesWarningP1 handles the P1 warning log signals.
func (e *Engine) logTreesWarningP1(
	sigMap map[string]*Signal,
) []Incident {
	var out []Incident
	if s, ok := sigMap["log_lock_timeout"]; ok {
		out = append(out, logTreeSimple(s, "warning",
			"Lock wait timeout exceeded", "safe"))
	}
	if s, ok := sigMap["log_statement_timeout"]; ok {
		out = append(out, logTreeSimple(s, "warning",
			"Statement timeout exceeded", "safe"))
	}
	if s, ok := sigMap["log_replication_conflict"]; ok {
		out = append(out, logTreeSimple(s, "warning",
			"Replication conflict with recovery on standby",
			"moderate"))
	}
	if s, ok := sigMap["log_wal_segment_removed"]; ok {
		out = append(out, logTreeSimple(s, "critical",
			"WAL segment removed before replica could consume "+
				"it -- replica may need rebuild", "high_risk"))
	}
	if s, ok := sigMap["log_autovacuum_cancel"]; ok {
		out = append(out, logTreeSimple(s, "warning",
			"Autovacuum task cancelled due to lock conflict",
			"safe"))
	}
	if s, ok := sigMap["log_replication_slot_inactive"]; ok {
		out = append(out, logTreeReplicationSlot(s))
	}
	if s, ok := sigMap["log_authentication_failure"]; ok {
		out = append(out, logTreeSimple(s, "warning",
			"Authentication failure", "safe"))
	}
	if s, ok := sigMap["log_slow_query"]; ok {
		out = append(out, logTreeSlowQuery(s))
	}
	return out
}

// ---------------------------------------------------------------------------
// Individual log tree handlers
// ---------------------------------------------------------------------------

// logTreeSimple builds a basic log incident for signals that need no
// special metric extraction beyond the log message.
func logTreeSimple(
	s *Signal,
	severity, rootCause, risk string,
) Incident {
	evidence := stringMetric(s, "message")
	if evidence == "" {
		evidence = rootCause
	}
	if len(evidence) > 200 {
		evidence = evidence[:200] + "..."
	}
	return buildLogIncident(s.FiredAt, severity, []string{s.ID},
		rootCause,
		[]ChainLink{{
			Order: 1, Signal: s.ID,
			Description: rootCause, Evidence: evidence,
		}},
		dbAffected(s), "", risk,
	)
}

// logTreeDeadlock handles log_deadlock_detected with self-inflicted
// detection and PID extraction.
func logTreeDeadlock(s *Signal) Incident {
	evidence := fmt.Sprintf("database=%s",
		stringMetric(s, "database"))
	if pids := stringMetric(s, "pids"); pids != "" {
		evidence += fmt.Sprintf(", pids=%s", pids)
	}
	if boolMetric(s, "self_inflicted") {
		evidence += " [self-inflicted deadlock]"
	}
	return buildLogIncident(s.FiredAt, "critical",
		[]string{s.ID},
		"Deadlock detected between concurrent transactions",
		[]ChainLink{{
			Order: 1, Signal: s.ID,
			Description: "Deadlock detected between concurrent transactions",
			Evidence:    evidence,
		}},
		dbAffected(s), "", "moderate",
	)
}

// logTreeOOM handles log_out_of_memory. An actual OOM means the
// combined memory pressure from work_mem * active queries (or
// max_connections) exceeded available RAM. Temp file spills are the
// PREVENTION mechanism — if OOM fires, PG could not spill fast enough
// or the operation was not spillable (e.g. hash aggregates pre-PG15).
func logTreeOOM(s *Signal) Incident {
	evidence := "Out of memory during query execution"
	if db := stringMetric(s, "database"); db != "" {
		evidence = fmt.Sprintf("database=%s: %s", db, evidence)
	}
	rootCause := "Out of memory -- work_mem × active sessions " +
		"exceeds available RAM"
	sql := "SELECT name, setting, unit FROM pg_settings " +
		"WHERE name IN ('work_mem','shared_buffers'," +
		"'max_connections','maintenance_work_mem') " +
		"ORDER BY name;"
	return buildLogIncident(s.FiredAt, "critical",
		[]string{s.ID}, rootCause,
		[]ChainLink{
			{Order: 1, Signal: s.ID,
				Description: rootCause,
				Evidence:    evidence},
			{Order: 2, Signal: s.ID,
				Description: "Reduce work_mem or max_connections " +
					"to fit within available RAM",
				Evidence: "work_mem × max_connections must be " +
					"significantly less than total RAM"},
		},
		dbAffected(s), sql, "high_risk",
	)
}

// logTreeTxidWraparound handles log_txid_wraparound_warning.
// Autovacuum is already trying to freeze — the root cause is what
// holds the xmin horizon back: stale backend_xmin, replication slots
// (catalog_xmin), or orphaned prepared transactions.
func logTreeTxidWraparound(s *Signal) Incident {
	rootCause := "Transaction ID wraparound imminent " +
		"-- identify what holds the xmin horizon"
	// Diagnostic: show the three sources that can hold back xmin.
	sql := "-- 1. Oldest backend xmin holders\n" +
		"SELECT pid, usename, state, " +
		"age(backend_xmin) AS xmin_age, " +
		"now()-xact_start AS tx_duration, " +
		"left(query,80) AS query " +
		"FROM pg_stat_activity " +
		"WHERE backend_xmin IS NOT NULL " +
		"ORDER BY age(backend_xmin) DESC LIMIT 5;\n" +
		"-- 2. Replication slots holding xmin\n" +
		"SELECT slot_name, slot_type, active, " +
		"age(catalog_xmin) AS catalog_xmin_age, " +
		"age(xmin) AS xmin_age, " +
		"pg_size_pretty(pg_wal_lsn_diff(" +
		"pg_current_wal_lsn(), restart_lsn)) AS retained " +
		"FROM pg_replication_slots " +
		"ORDER BY age(xmin) DESC NULLS LAST;\n" +
		"-- 3. Orphaned prepared transactions\n" +
		"SELECT gid, prepared, owner, database " +
		"FROM pg_prepared_xacts ORDER BY prepared;"
	return buildLogIncident(s.FiredAt, "critical",
		[]string{s.ID}, rootCause,
		[]ChainLink{
			{Order: 1, Signal: s.ID,
				Description: "XID wraparound warning from PostgreSQL",
				Evidence:    stringMetric(s, "message")},
			{Order: 2, Signal: s.ID,
				Description: "Autovacuum cannot freeze because " +
					"something holds the xmin horizon back",
				Evidence: "Check: stale backend_xmin, " +
					"replication slot catalog_xmin, " +
					"orphaned prepared transactions"},
		},
		dbAffected(s), sql, "high_risk",
	)
}

// logTreeTempFile handles log_temp_file_created. Temp files are
// NORMAL — PostgreSQL spills sort/hash operations to disk when they
// exceed work_mem. This PREVENTS OOM, not causes it. Large temp files
// indicate potential I/O pressure, not memory exhaustion.
func logTreeTempFile(s *Signal) Incident {
	rootCause := "Query sort/hash spilling to disk " +
		"(work_mem exceeded, preventing OOM)"
	evidence := rootCause
	if bytes := intMetric(s, "temp_file_bytes"); bytes > 0 {
		evidence = fmt.Sprintf("temp_file_bytes=%d -- "+
			"disk spill is normal but may cause I/O pressure",
			bytes)
	}
	return buildLogIncident(s.FiredAt, "info",
		[]string{s.ID}, rootCause,
		[]ChainLink{{
			Order: 1, Signal: s.ID,
			Description: "Query operation exceeded work_mem, " +
				"spilled to temp file on disk",
			Evidence: evidence,
		}},
		dbAffected(s), "", "safe",
	)
}

// logTreeReplicationSlot handles log_replication_slot_inactive with
// a diagnostic SQL recommendation. Shows ALL slots (active and
// inactive) since active slots with slow consumers are equally
// dangerous for WAL retention and xmin horizon.
func logTreeReplicationSlot(s *Signal) Incident {
	rootCause := "Replication slot accumulating WAL"
	sql := "SELECT slot_name, slot_type, active, " +
		"pg_size_pretty(pg_wal_lsn_diff(" +
		"pg_current_wal_lsn(), restart_lsn)) AS retained " +
		"FROM pg_replication_slots " +
		"ORDER BY pg_wal_lsn_diff(" +
		"pg_current_wal_lsn(), restart_lsn) DESC;"
	evidence := stringMetric(s, "message")
	if evidence == "" {
		evidence = rootCause
	}
	return buildLogIncident(s.FiredAt, "warning",
		[]string{s.ID}, rootCause,
		[]ChainLink{{
			Order: 1, Signal: s.ID,
			Description: rootCause, Evidence: evidence,
		}},
		dbAffected(s), sql, "moderate",
	)
}

// logTreeSlowQuery handles log_slow_query with duration and query
// extraction from metrics.
func logTreeSlowQuery(s *Signal) Incident {
	rootCause := "Slow query detected via log_min_duration_statement"
	evidence := rootCause
	if dur := floatMetric(s, "duration_ms"); dur > 0 {
		q := stringMetric(s, "query")
		if len(q) > 120 {
			q = q[:120] + "..."
		}
		if q != "" {
			evidence = fmt.Sprintf("duration=%.0fms query=%s",
				dur, q)
		} else {
			evidence = fmt.Sprintf("duration=%.0fms", dur)
		}
	}
	return buildLogIncident(s.FiredAt, "info",
		[]string{s.ID}, rootCause,
		[]ChainLink{{
			Order: 1, Signal: s.ID,
			Description: rootCause, Evidence: evidence,
		}},
		dbAffected(s), "", "safe",
	)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// dbAffected extracts the database name from signal metrics and
// returns it as a single-element affected objects slice, or nil.
func dbAffected(s *Signal) []string {
	if db := stringMetric(s, "database"); db != "" {
		return []string{db}
	}
	return nil
}

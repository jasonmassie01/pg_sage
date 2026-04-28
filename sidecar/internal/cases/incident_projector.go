package cases

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

type IncidentChainLink struct {
	Order       int
	Signal      string
	Description string
	Evidence    string
}

type SourceIncident struct {
	ID              string
	DatabaseName    string
	Severity        Severity
	RootCause       string
	CausalChain     []IncidentChainLink
	AffectedObjects []string
	SignalIDs       []string
	RecommendedSQL  string
	ActionRisk      string
	Source          string
	Confidence      float64
	DetectedAt      time.Time
	LastDetectedAt  time.Time
	ResolvedAt      *time.Time
	OccurrenceCount int
}

func ProjectIncident(i SourceIncident) Case {
	c := NewCase(CaseInput{
		SourceType:       SourceIncidentType,
		SourceID:         i.ID,
		DatabaseName:     i.DatabaseName,
		IdentityKey:      incidentIdentityKey(i),
		Title:            incidentTitle(i),
		Severity:         i.Severity,
		Why:              i.RootCause,
		WhyNow:           incidentWhyNow(i),
		Evidence:         incidentEvidence(i),
		ActionCandidates: actionCandidatesForIncident(i),
		ObservedAt:       incidentObservedAt(i),
	})
	if i.ResolvedAt != nil {
		c.State = StateResolved
		c.ActionCandidates = nil
		c.UpdatedAt = *i.ResolvedAt
	}
	return c
}

func incidentEvidence(i SourceIncident) []Evidence {
	evidence := []Evidence{{
		Type:    "incident",
		Summary: i.RootCause,
		Detail: map[string]any{
			"source":           i.Source,
			"signal_ids":       i.SignalIDs,
			"affected_objects": i.AffectedObjects,
			"confidence":       i.Confidence,
			"occurrence_count": i.OccurrenceCount,
			"action_risk":      i.ActionRisk,
			"recommended_sql":  i.RecommendedSQL,
		},
	}}
	for _, link := range i.CausalChain {
		evidence = append(evidence, Evidence{
			Type:    "causal_chain",
			Summary: link.Signal,
			Detail: map[string]any{
				"order":       link.Order,
				"description": link.Description,
				"evidence":    link.Evidence,
			},
		})
	}
	return evidence
}

func actionCandidatesForIncident(i SourceIncident) []ActionCandidate {
	if i.ResolvedAt != nil {
		return nil
	}
	if idleTxnOrLockIncident(i) {
		return idleTxnPlaybookCandidates(i)
	}
	if runawayQueryIncident(i) {
		return runawayQueryPlaybookCandidates(i)
	}
	if connectionExhaustionIncident(i) {
		return []ActionCandidate{diagnosticIncidentCandidate(
			"diagnose_connection_exhaustion",
			diagnoseConnectionExhaustionSQL(),
			i.Confidence,
			[]string{"identify connection pressure by role, app, and state"},
		)}
	}
	if walOrReplicationIncident(i) {
		if standbyConflictIncident(i) {
			return []ActionCandidate{diagnosticIncidentCandidate(
				"diagnose_standby_conflicts",
				diagnoseStandbyConflictsSQL(),
				i.Confidence,
				[]string{"verify standby conflict type and replay pressure"},
			)}
		}
		return []ActionCandidate{diagnosticIncidentCandidate(
			"diagnose_wal_replication",
			diagnoseWALReplicationSQL(),
			i.Confidence,
			[]string{"verify WAL retention, slot status, and replay lag"},
		)}
	}
	if sequenceExhaustionIncident(i) {
		return []ActionCandidate{sequenceCapacityMigrationCandidate(i)}
	}
	if autovacuumFallingBehindIncident(i) {
		return autovacuumIncidentCandidates(i)
	}
	return nil
}

func idleTxnPlaybookCandidates(i SourceIncident) []ActionCandidate {
	expires := time.Now().UTC().Add(15 * time.Minute)
	pid, ok := extractSinglePID(i)
	candidates := []ActionCandidate{
		{
			ActionType:       "diagnose_lock_blockers",
			RiskTier:         "safe",
			Confidence:       i.Confidence,
			ProposedSQL:      diagnoseLockBlockersSQL(),
			ExpiresAt:        &expires,
			OutputModes:      []string{"execute", "script"},
			RollbackClass:    "not_applicable",
			VerificationPlan: []string{"refresh lock graph"},
		},
		cancelBackendCandidate(pid, ok, expires, i.Confidence),
	}
	if ok && severeLockIncident(i) {
		candidates = append(candidates,
			terminateBackendCandidate(pid, expires, i.Confidence))
	}
	return candidates
}

func runawayQueryPlaybookCandidates(i SourceIncident) []ActionCandidate {
	expires := time.Now().UTC().Add(15 * time.Minute)
	pid, ok := extractSinglePID(i)
	return []ActionCandidate{
		diagnosticIncidentCandidate(
			"diagnose_runaway_query",
			diagnoseRunawayQuerySQL(pid, ok),
			i.Confidence,
			[]string{"confirm query age, wait state, and temp spill evidence"},
		),
		cancelBackendCandidate(pid, ok, expires, i.Confidence),
	}
}

func diagnosticIncidentCandidate(
	actionType string,
	sql string,
	confidence float64,
	verification []string,
) ActionCandidate {
	expires := time.Now().UTC().Add(15 * time.Minute)
	return ActionCandidate{
		ActionType:       actionType,
		RiskTier:         "safe",
		Confidence:       confidence,
		ProposedSQL:      sql,
		ExpiresAt:        &expires,
		OutputModes:      []string{"execute", "script"},
		RollbackClass:    "not_applicable",
		VerificationPlan: verification,
	}
}

func sequenceCapacityMigrationCandidate(i SourceIncident) ActionCandidate {
	expires := time.Now().UTC().Add(24 * time.Hour)
	object := incidentPrimaryObject(i, "affected_sequence")
	return ActionCandidate{
		ActionType:       "prepare_sequence_capacity_migration",
		RiskTier:         "high",
		Confidence:       i.Confidence,
		ExpiresAt:        &expires,
		OutputModes:      []string{"generate_pr_or_script"},
		RollbackClass:    "forward_fix_only",
		VerificationPlan: []string{"run verification SQL before and after migration"},
		BlockedReason:    "direct sequence capacity changes require migration review",
		ScriptOutput: &ScriptOutput{
			Filename:     "sequence_capacity_" + sanitizeScriptPart(object) + ".sql",
			MigrationSQL: sequenceCapacityMigrationSQL(object),
			VerificationSQL: []string{
				"SELECT last_value, max_value FROM " + object + ";",
				"-- Confirm dependent column type can hold future generated values.",
			},
			PRTitle: "Review sequence capacity migration: " + object,
			PRBody: "Generated from pg_sage incident " + i.ID +
				". Sequence exhaustion is forward-fix only and must be reviewed.",
			RiskLabels: []string{"high", "forward_fix_only", "sequence_exhaustion"},
			Format:     "sql",
		},
	}
}

func autovacuumIncidentCandidates(i SourceIncident) []ActionCandidate {
	object := incidentPrimaryObject(i, "")
	candidates := []ActionCandidate{diagnosticIncidentCandidate(
		"diagnose_vacuum_pressure",
		diagnoseVacuumPressureSQL(object),
		i.Confidence,
		[]string{"identify blockers, dead tuples, and oldest xmin holders"},
	)}
	if object != "" {
		expires := time.Now().UTC().Add(24 * time.Hour)
		candidates = append(candidates, ActionCandidate{
			ActionType:       "vacuum_table",
			RiskTier:         "safe",
			Confidence:       i.Confidence,
			ProposedSQL:      "VACUUM " + object + ";",
			ExpiresAt:        &expires,
			OutputModes:      []string{"execute", "queue_for_approval"},
			RollbackClass:    "no_rollback_needed",
			VerificationPlan: verificationPlanForAction("vacuum_table"),
		})
	}
	return candidates
}

func cancelBackendCandidate(
	pid int,
	ok bool,
	expires time.Time,
	confidence float64,
) ActionCandidate {
	c := ActionCandidate{
		ActionType:       "cancel_backend",
		RiskTier:         "moderate",
		Confidence:       confidence,
		ExpiresAt:        &expires,
		OutputModes:      []string{"queue_for_approval", "script"},
		RollbackClass:    "not_reversible",
		VerificationPlan: []string{"verify blocker PID no longer blocks waiters"},
	}
	if !ok {
		c.BlockedReason = "blocker PID unavailable"
		return c
	}
	c.ProposedSQL = fmt.Sprintf("SELECT pg_cancel_backend(%d)", pid)
	return c
}

func terminateBackendCandidate(
	pid int,
	expires time.Time,
	confidence float64,
) ActionCandidate {
	return ActionCandidate{
		ActionType:       "terminate_backend",
		RiskTier:         "high",
		Confidence:       confidence,
		ProposedSQL:      fmt.Sprintf("SELECT pg_terminate_backend(%d)", pid),
		ExpiresAt:        &expires,
		OutputModes:      []string{"queue_for_approval", "script"},
		RollbackClass:    "not_reversible",
		VerificationPlan: []string{"verify blocker PID is gone"},
	}
}

func incidentWhyNow(i SourceIncident) string {
	if i.Severity == SeverityCritical {
		return "critical incident requires immediate review"
	}
	if i.OccurrenceCount > 1 {
		return fmt.Sprintf("incident has occurred %d times", i.OccurrenceCount)
	}
	if idleTxnOrLockIncident(i) {
		return "lock blocker can stall writes, vacuum, or connection capacity"
	}
	return "active incident needs triage"
}

func incidentIdentityKey(i SourceIncident) string {
	signal := i.Source
	if len(i.SignalIDs) > 0 {
		signal = i.SignalIDs[0]
	}
	return fmt.Sprintf("incident:%s:%s:%s", i.DatabaseName, signal, i.ID)
}

func extractSinglePID(i SourceIncident) (int, bool) {
	text := strings.Join(incidentSearchParts(i), "\n")
	matches := pidPattern.FindAllStringSubmatch(text, -1)
	seen := map[string]bool{}
	var pidText string
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		seen[match[1]] = true
		pidText = match[1]
	}
	if len(seen) != 1 {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(pidText, "%d", &pid); err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

var pidPattern = regexp.MustCompile(`(?i)\bpid\D+([1-9][0-9]*)\b`)

func idleTxnOrLockIncident(i SourceIncident) bool {
	signals := signalSet(i.SignalIDs)
	if signals["idle_in_tx_elevated"] && signals["connections_high"] {
		return true
	}
	if signals["vacuum_blocked"] &&
		strings.Contains(strings.ToLower(i.RootCause), "idle-in-transaction pid") {
		return true
	}
	if signals["lock_contention"] &&
		strings.Contains(strings.ToLower(strings.Join(incidentSearchParts(i), " ")), "block") {
		return true
	}
	return false
}

func runawayQueryIncident(i SourceIncident) bool {
	signals := signalSet(i.SignalIDs)
	if signals["runaway_query"] || signals["long_running_query"] {
		return true
	}
	text := strings.ToLower(strings.Join(incidentSearchParts(i), " "))
	return strings.Contains(text, "runaway query") ||
		strings.Contains(text, "query ran for")
}

func connectionExhaustionIncident(i SourceIncident) bool {
	signals := signalSet(i.SignalIDs)
	if signals["connections_high"] && !idleTxnOrLockIncident(i) {
		return true
	}
	text := strings.ToLower(strings.Join(incidentSearchParts(i), " "))
	return strings.Contains(text, "max_connections") ||
		strings.Contains(text, "connection exhaustion")
}

func walOrReplicationIncident(i SourceIncident) bool {
	signals := signalSet(i.SignalIDs)
	return signals["replication_lag"] ||
		signals["replica_lag"] ||
		signals["standby_conflict"] ||
		signals["wal_growth"] ||
		signals["inactive_slot"]
}

func standbyConflictIncident(i SourceIncident) bool {
	signals := signalSet(i.SignalIDs)
	return signals["standby_conflict"] || signals["replica_conflict"]
}

func autovacuumFallingBehindIncident(i SourceIncident) bool {
	signals := signalSet(i.SignalIDs)
	return signals["autovacuum_falling_behind"] ||
		signals["autovacuum_blocked"] ||
		signals["vacuum_falling_behind"]
}

func sequenceExhaustionIncident(i SourceIncident) bool {
	signals := signalSet(i.SignalIDs)
	if signals["sequence_exhaustion"] {
		return true
	}
	return strings.Contains(
		strings.ToLower(strings.Join(incidentSearchParts(i), " ")),
		"sequence",
	) && strings.Contains(
		strings.ToLower(strings.Join(incidentSearchParts(i), " ")),
		"exhaust",
	)
}

func severeLockIncident(i SourceIncident) bool {
	text := strings.ToLower(strings.Join(incidentSearchParts(i), " "))
	return i.Severity == SeverityCritical ||
		strings.Contains(text, "vacuum") ||
		strings.Contains(text, "xid") ||
		strings.Contains(text, "connection")
}

func incidentPrimaryObject(i SourceIncident, fallback string) string {
	for _, object := range i.AffectedObjects {
		if strings.TrimSpace(object) != "" {
			return strings.TrimSpace(object)
		}
	}
	return fallback
}

func sanitizeScriptPart(value string) string {
	replacer := strings.NewReplacer(".", "_", "\"", "", "'", "", " ", "_")
	return replacer.Replace(value)
}

func signalSet(signals []string) map[string]bool {
	out := make(map[string]bool, len(signals))
	for _, signal := range signals {
		out[signal] = true
	}
	return out
}

func incidentSearchParts(i SourceIncident) []string {
	parts := []string{i.RootCause, i.RecommendedSQL, i.ActionRisk, i.Source}
	parts = append(parts, i.SignalIDs...)
	parts = append(parts, i.AffectedObjects...)
	for _, link := range i.CausalChain {
		parts = append(parts, link.Signal, link.Description, link.Evidence)
	}
	sort.Strings(parts)
	return parts
}

func incidentObservedAt(i SourceIncident) time.Time {
	if !i.LastDetectedAt.IsZero() {
		return i.LastDetectedAt
	}
	return i.DetectedAt
}

func incidentTitle(i SourceIncident) string {
	title := strings.TrimSpace(i.RootCause)
	if title == "" {
		return "Database incident"
	}
	if len(title) <= 140 {
		return title
	}
	return title[:137] + "..."
}

func diagnoseLockBlockersSQL() string {
	return `SELECT blocked.pid AS blocked_pid,
       blocker.pid AS blocker_pid,
       blocked.query AS blocked_query,
       blocker.query AS blocker_query,
       blocker.state AS blocker_state
FROM pg_stat_activity blocked
JOIN pg_locks blocked_locks
  ON blocked_locks.pid = blocked.pid
JOIN pg_locks blocker_locks
  ON blocker_locks.locktype = blocked_locks.locktype
 AND blocker_locks.database IS NOT DISTINCT FROM blocked_locks.database
 AND blocker_locks.relation IS NOT DISTINCT FROM blocked_locks.relation
 AND blocker_locks.page IS NOT DISTINCT FROM blocked_locks.page
 AND blocker_locks.tuple IS NOT DISTINCT FROM blocked_locks.tuple
 AND blocker_locks.transactionid IS NOT DISTINCT FROM blocked_locks.transactionid
 AND blocker_locks.classid IS NOT DISTINCT FROM blocked_locks.classid
 AND blocker_locks.objid IS NOT DISTINCT FROM blocked_locks.objid
 AND blocker_locks.objsubid IS NOT DISTINCT FROM blocked_locks.objsubid
 AND blocker_locks.pid != blocked_locks.pid
JOIN pg_stat_activity blocker
  ON blocker.pid = blocker_locks.pid
WHERE NOT blocked_locks.granted
  AND blocker_locks.granted`
}

func diagnoseRunawayQuerySQL(pid int, ok bool) string {
	where := "WHERE state = 'active'"
	if ok {
		where = fmt.Sprintf("WHERE pid = %d", pid)
	}
	return `SELECT pid,
       usename,
       application_name,
       state,
       wait_event_type,
       wait_event,
       now() - query_start AS query_age,
       temp_bytes,
       left(query, 1000) AS query
FROM pg_stat_activity
LEFT JOIN pg_stat_database
  ON pg_stat_database.datname = pg_stat_activity.datname
` + where + `
ORDER BY query_age DESC
LIMIT 20`
}

func diagnoseConnectionExhaustionSQL() string {
	return `WITH limits AS (
  SELECT setting::int AS max_connections
  FROM pg_settings
  WHERE name = 'max_connections'
)
SELECT state,
       usename,
       application_name,
       count(*) AS connections,
       round(100.0 * count(*) / limits.max_connections, 2) AS pct_of_limit
FROM pg_stat_activity, limits
GROUP BY state, usename, application_name, limits.max_connections
ORDER BY connections DESC`
}

func diagnoseWALReplicationSQL() string {
	return `SELECT 'replication' AS source,
       application_name,
       state,
       sync_state,
       replay_lag,
       write_lag,
       flush_lag
FROM pg_stat_replication
UNION ALL
SELECT 'slot' AS source,
       slot_name AS application_name,
       active::text AS state,
       slot_type AS sync_state,
       NULL AS replay_lag,
       NULL AS write_lag,
       NULL AS flush_lag
FROM pg_replication_slots`
}

func diagnoseStandbyConflictsSQL() string {
	return `SELECT datname,
       confl_tablespace,
       confl_lock,
       confl_snapshot,
       confl_bufferpin,
       confl_deadlock
FROM pg_stat_database_conflicts
ORDER BY confl_lock + confl_snapshot + confl_bufferpin + confl_deadlock DESC`
}

func sequenceCapacityMigrationSQL(sequence string) string {
	return `-- Forward-fix sequence capacity migration generated by pg_sage
-- Inspect the owning table and column before editing this migration.
SELECT pg_get_serial_sequence('', '') AS owning_sequence;
SELECT last_value, max_value FROM ` + sequence + `;
-- Preferred migration is usually widening the owning column to bigint
-- or replacing the sequence with an identity column under a reviewed rollout.`
}

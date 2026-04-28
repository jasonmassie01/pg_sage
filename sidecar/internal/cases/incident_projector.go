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

func severeLockIncident(i SourceIncident) bool {
	text := strings.ToLower(strings.Join(incidentSearchParts(i), " "))
	return i.Severity == SeverityCritical ||
		strings.Contains(text, "vacuum") ||
		strings.Contains(text, "xid") ||
		strings.Contains(text, "connection")
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

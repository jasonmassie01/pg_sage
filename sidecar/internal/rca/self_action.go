package rca

import (
	"fmt"
	"time"
)

// SageAction represents a recently executed pg_sage action from the
// action log. Populated by the caller (main loop reads from
// sage.action_log table).
type SageAction struct {
	ID           string
	Family       string // e.g. "set_work_mem", "create_index", "vacuum_full", "drop_index"
	ExecutedAt   time.Time
	Database     string
	Description  string
	RolledBack   bool
	RolledBackAt *time.Time
}

// IncidentAnnotation attaches recent pg_sage actions to an incident
// and optionally identifies a deterministic causal link.
type IncidentAnnotation struct {
	IncidentID   string
	SageActions  []SageAction // actions within lookback window
	CausalAction *SageAction  // non-nil if a deterministic causal path matched
	CausalReason string       // explanation of the causal path
}

// causalPaths maps "action_family:signal_id" to a human-readable
// explanation of why that action likely caused that signal.
var causalPaths = map[string]string{
	"set_work_mem:log_out_of_memory":    "Increasing work_mem may have caused out-of-memory condition",
	"create_index:log_disk_full":         "Index creation consumed remaining disk space",
	"vacuum_full:log_lock_timeout":       "VACUUM FULL holds AccessExclusive lock, causing lock timeouts",
	"create_index:log_temp_file_created": "Index build operation spilled to disk",
	"drop_index:log_slow_query":          "Dropping index may have caused query performance regression",
}

// SelfActionCorrelator checks whether recent pg_sage actions may
// have caused detected incidents.
type SelfActionCorrelator struct {
	lookbackWindow    time.Duration
	rollbackLookback  time.Duration
	rollbackThreshold int
	logFn             func(string, string, ...any)
}

// NewSelfActionCorrelator creates a correlator with default windows:
// 30-minute lookback, 30-day rollback history, threshold of 2.
func NewSelfActionCorrelator(
	logFn func(string, string, ...any),
) *SelfActionCorrelator {
	return &SelfActionCorrelator{
		lookbackWindow:    30 * time.Minute,
		rollbackLookback:  30 * 24 * time.Hour,
		rollbackThreshold: 2,
		logFn:             logFn,
	}
}

// Correlate takes incidents and recent actions, returns:
//   - annotations: each incident annotated with recent sage actions
//   - selfCaused: new incidents for deterministic causal matches
//   - manualReview: incidents requiring human review (anti-oscillation)
func (c *SelfActionCorrelator) Correlate(
	incidents []Incident,
	recentActions []SageAction,
	rollbackHistory []SageAction,
) ([]IncidentAnnotation, []Incident, []Incident) {
	var annotations []IncidentAnnotation
	var selfCaused []Incident
	var manualReview []Incident

	rollbackCounts := countRollbacksByFamily(rollbackHistory)

	for i := range incidents {
		inc := &incidents[i]
		nearby := c.actionsInWindow(inc, recentActions)
		ann := IncidentAnnotation{
			IncidentID:  inc.ID,
			SageActions: nearby,
		}

		causal := c.findCausalMatch(inc, nearby)
		if causal != nil {
			ann.CausalAction = causal.action
			ann.CausalReason = causal.reason

			if rollbackCounts[causal.action.Family] >= c.rollbackThreshold {
				manualReview = append(manualReview,
					c.buildManualReviewIncident(inc, causal))
			} else {
				selfCaused = append(selfCaused,
					c.buildSelfCausedIncident(inc, causal))
			}
		}

		annotations = append(annotations, ann)
	}

	return annotations, selfCaused, manualReview
}

// causalMatch holds a matched action and the reason it was flagged.
type causalMatch struct {
	action *SageAction
	reason string
}

// actionsInWindow returns actions executed within the lookback window
// before the incident and matching the incident's database.
func (c *SelfActionCorrelator) actionsInWindow(
	inc *Incident,
	actions []SageAction,
) []SageAction {
	var matched []SageAction
	cutoff := inc.DetectedAt.Add(-c.lookbackWindow)
	for i := range actions {
		a := &actions[i]
		if a.ExecutedAt.Before(cutoff) || a.ExecutedAt.After(inc.DetectedAt) {
			continue
		}
		if !databaseMatches(inc.DatabaseName, a.Database) {
			continue
		}
		matched = append(matched, *a)
	}
	return matched
}

// findCausalMatch checks the 5 deterministic causal paths against
// each (action, signal) pair from the nearby actions and incident.
func (c *SelfActionCorrelator) findCausalMatch(
	inc *Incident,
	nearby []SageAction,
) *causalMatch {
	for i := range nearby {
		a := &nearby[i]
		for _, sid := range inc.SignalIDs {
			key := a.Family + ":" + sid
			if reason, ok := causalPaths[key]; ok {
				c.logFn("warn",
					"rca: self-action causal match: %s -> %s (action %s)",
					a.Family, sid, a.ID)
				return &causalMatch{action: a, reason: reason}
			}
		}
	}
	return nil
}

// buildSelfCausedIncident creates an incident attributing the problem
// to a specific pg_sage action.
func (c *SelfActionCorrelator) buildSelfCausedIncident(
	original *Incident,
	match *causalMatch,
) Incident {
	severity := escalateSeverity(original.Severity)
	inc := buildIncident(
		original.DetectedAt,
		severity,
		original.SignalIDs,
		fmt.Sprintf("Self-caused: %s (action %s: %s)",
			match.reason, match.action.ID, match.action.Description),
		[]ChainLink{
			{Order: 1, Signal: match.action.Family,
				Description: fmt.Sprintf("pg_sage executed %s",
					match.action.Family),
				Evidence: fmt.Sprintf("Action %s at %s: %s",
					match.action.ID,
					match.action.ExecutedAt.Format(time.RFC3339),
					match.action.Description)},
			{Order: 2, Signal: original.SignalIDs[0],
				Description: match.reason,
				Evidence: fmt.Sprintf("Incident detected at %s",
					original.DetectedAt.Format(time.RFC3339))},
		},
		original.AffectedObjects,
		original.RollbackSQL,
		"high_risk",
	)
	inc.Source = "self_action"
	inc.Confidence = 0.9
	inc.DatabaseName = original.DatabaseName
	return inc
}

// buildManualReviewIncident creates an incident that blocks automated
// retry and requires human review (anti-oscillation).
func (c *SelfActionCorrelator) buildManualReviewIncident(
	original *Incident,
	match *causalMatch,
) Incident {
	inc := buildIncident(
		original.DetectedAt,
		"critical",
		original.SignalIDs,
		fmt.Sprintf("Manual review required: %s has been rolled back %d+ "+
			"times in 30 days. %s",
			match.action.Family, c.rollbackThreshold, match.reason),
		[]ChainLink{
			{Order: 1, Signal: match.action.Family,
				Description: "Repeated rollback detected (anti-oscillation)",
				Evidence: fmt.Sprintf("Action family %s exceeds rollback "+
					"threshold of %d", match.action.Family,
					c.rollbackThreshold)},
		},
		original.AffectedObjects,
		"", // no recommended SQL -- human must decide
		"high_risk",
	)
	inc.Source = "manual_review_required"
	inc.Confidence = 1.0
	inc.DatabaseName = original.DatabaseName
	return inc
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// countRollbacksByFamily counts rolled-back actions per family.
func countRollbacksByFamily(history []SageAction) map[string]int {
	counts := make(map[string]int, len(history))
	for _, a := range history {
		if a.RolledBack {
			counts[a.Family]++
		}
	}
	return counts
}

// databaseMatches returns true if the incident and action refer to
// the same database. Empty incident database matches any action.
func databaseMatches(incidentDB, actionDB string) bool {
	if incidentDB == "" {
		return true
	}
	return incidentDB == actionDB
}

// escalateSeverity bumps severity one level for self-caused incidents.
func escalateSeverity(original string) string {
	switch original {
	case "info":
		return "warning"
	case "warning":
		return "critical"
	default:
		return "critical"
	}
}

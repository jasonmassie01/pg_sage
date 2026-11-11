package cases

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/signalid"
)

type IncidentChainLink struct {
	Order       int
	Signal      string
	Description string
	Evidence    string
	// Blocker is the structured root-blocker identity RCA records on
	// lock_contention links; nil on every other link and on rows written
	// before it was recorded.
	Blocker *IncidentBlocker
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
	// PreviousIncidentID is the resolved incident this one recurs
	// (sage.incidents.previous_incident_id); empty for a first occurrence.
	PreviousIncidentID string
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
	if i.PreviousIncidentID != "" {
		c.Recurrence = &Recurrence{PreviousID: i.PreviousIncidentID}
	}
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

// actionCandidatesForIncident selects the playbook from the incident's
// signal ids only (playbooks-02): the idle-tx and lock predicate first
// (playbooks-01), then the signalPlaybooks table. Prose never selects one.
func actionCandidatesForIncident(i SourceIncident) []ActionCandidate {
	if i.ResolvedAt != nil {
		return nil
	}
	if idleTxnOrLockIncident(i) {
		return idleTxnPlaybookCandidates(i)
	}
	return incidentPlaybookCandidates(i, playbookForSignals(i.SignalIDs))
}

// idleTxnPlaybookCandidates is the lock / idle-in-transaction playbook:
// the blocker diagnostic plus backend signals built only from structured
// blocker identity (lockBlockerCandidates). Idle-tx saturation and
// vacuum_blocked incidents carry none, so their cancel stays blocked.
func idleTxnPlaybookCandidates(i SourceIncident) []ActionCandidate {
	expires := incidentExpiry(i, 15*time.Minute)
	candidates := []ActionCandidate{
		{
			ActionType:       "diagnose_lock_blockers",
			RiskTier:         "safe",
			Confidence:       i.Confidence,
			ProposedSQL:      diagnosticSQL("diagnose_lock_blockers"),
			ExpiresAt:        &expires,
			OutputModes:      []string{"execute", "script"},
			RollbackClass:    "not_applicable",
			VerificationPlan: []string{"refresh lock graph"},
		},
	}
	return append(candidates, lockBlockerCandidates(i, expires)...)
}

// runawayQueryPlaybookCandidates diagnoses active queries. Runaway
// incidents carry no structured backend identity, so the cancel is blocked
// (the executor's runaway tracker owns identity-checked cancels).
func runawayQueryPlaybookCandidates(i SourceIncident) []ActionCandidate {
	expires := incidentExpiry(i, 15*time.Minute)
	return []ActionCandidate{
		diagnosticIncidentCandidate(i, "diagnose_runaway_query",
			runawayVerificationPlan()),
		blockedSignalCandidate("cancel_backend", noBackendIdentityReason, expires,
			i.Confidence),
	}
}

// diagnosticIncidentCandidate proposes the action's probe script
// (diagnosticSQL).
func diagnosticIncidentCandidate(
	i SourceIncident,
	actionType string,
	verification []string,
) ActionCandidate {
	expires := incidentExpiry(i, 15*time.Minute)
	return ActionCandidate{
		ActionType:       actionType,
		RiskTier:         "safe",
		Confidence:       i.Confidence,
		ProposedSQL:      diagnosticSQL(actionType),
		ExpiresAt:        &expires,
		OutputModes:      []string{"execute", "script"},
		RollbackClass:    "not_applicable",
		VerificationPlan: verification,
	}
}

func autovacuumIncidentCandidates(i SourceIncident) []ActionCandidate {
	object := incidentPrimaryObject(i, "")
	candidates := []ActionCandidate{vacuumPressureDiagnostic(i)}
	if object != "" {
		expires := incidentExpiry(i, 24*time.Hour)
		candidates = append(candidates, ActionCandidate{
			ActionType:       "vacuum_table",
			RiskTier:         "safe",
			Confidence:       i.Confidence,
			ProposedSQL:      "VACUUM " + sanitize.QuoteQualifiedString(object) + ";",
			ExpiresAt:        &expires,
			OutputModes:      []string{"execute", "queue_for_approval"},
			RollbackClass:    "no_rollback_needed",
			VerificationPlan: verificationPlanForAction("vacuum_table"),
		})
	}
	return candidates
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

// IncidentIdentityKey is the case id ProjectIncident gives an incident,
// so other surfaces (Sage SRE investigations) can link to the case.
func IncidentIdentityKey(i SourceIncident) string { return incidentIdentityKey(i) }

func incidentIdentityKey(i SourceIncident) string {
	signal := i.Source
	if len(i.SignalIDs) > 0 {
		signal = i.SignalIDs[0]
	}
	return fmt.Sprintf("incident:%s:%s:%s", i.DatabaseName, signal, i.ID)
}

func idleTxnOrLockIncident(i SourceIncident) bool {
	signals := signalSet(i.SignalIDs)
	if signals[signalid.IdleInTxElevated] && signals[signalid.ConnectionsHigh] {
		return true
	}
	if signals[signalid.VacuumBlocked] &&
		strings.Contains(strings.ToLower(i.RootCause), "idle-in-transaction pid") {
		return true
	}
	if signals[signalid.LockContention] &&
		strings.Contains(strings.ToLower(strings.Join(incidentSearchParts(i), " ")), "block") {
		return true
	}
	return false
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

// incidentExpiry anchors a candidate's expiry to the incident's latest
// detection, so a stale incident's proposal expires instead of sliding
// forward on every read (G2-B24). Unknown detection time falls back to now.
func incidentExpiry(i SourceIncident, ttl time.Duration) time.Time {
	anchor := incidentObservedAt(i)
	if anchor.IsZero() {
		anchor = time.Now()
	}
	return anchor.UTC().Add(ttl)
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

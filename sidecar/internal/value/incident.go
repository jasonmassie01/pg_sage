package value

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Incident kinds and severities. They mirror the CHECK constraints on
// sage.incident_avoided.
const (
	IncidentXIDWraparound = "xid_wraparound"
	IncidentDiskFullSlot  = "disk_full_slot"
	IncidentLockStorm     = "lock_storm"

	SeverityNearMiss  = "near_miss"
	SeverityPrevented = "prevented"
)

// Credited minutes are deliberately conservative and versioned, so a later
// model can change them without rewriting history.
const (
	NearMissMinutes      = 120.0
	PreventedMinutes     = 480.0
	incidentModelVersion = 1
)

var (
	ErrInvalidIncident         = errors.New("invalid avoided incident")
	ErrIncidentAlreadyCredited = errors.New("incident is already credited")
)

// AvoidedIncident is a claim that one verified action moved an invariant
// back out of danger. The caller must have measured both sides.
type AvoidedIncident struct {
	ActionID   int64
	Kind       string
	Severity   string
	OccurredAt time.Time
}

// IncidentCandidate is the evidence graph behind an action: the decision
// that authorized it and the verification that confirmed it.
type IncidentCandidate struct {
	ActionID          int64
	DatabaseID        *int64
	DecisionID        int64
	VerificationID    int64
	Outcome           string
	VerificationState string
}

func incidentMinutes(severity string) (float64, bool) {
	switch severity {
	case SeverityNearMiss:
		return NearMissMinutes, true
	case SeverityPrevented:
		return PreventedMinutes, true
	}
	return 0, false
}

func knownIncidentKind(kind string) bool {
	return kind == IncidentXIDWraparound || kind == IncidentDiskFullSlot ||
		kind == IncidentLockStorm
}

func (i AvoidedIncident) validate() error {
	if i.ActionID <= 0 {
		return fmt.Errorf("%w: action id %d", ErrInvalidIncident, i.ActionID)
	}
	if !knownIncidentKind(i.Kind) {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidIncident, i.Kind)
	}
	if _, ok := incidentMinutes(i.Severity); !ok {
		return fmt.Errorf("%w: unknown severity %q", ErrInvalidIncident, i.Severity)
	}
	if i.OccurredAt.IsZero() {
		return fmt.Errorf("%w: occurred time is required", ErrInvalidIncident)
	}
	return nil
}

// CreditAvoidedIncident records incident credit for a verified, successful
// action. One action earns at most one credit per kind: a repeat returns
// ErrIncidentAlreadyCredited.
func (s *Service) CreditAvoidedIncident(
	ctx context.Context, incident AvoidedIncident,
) (IncidentRecord, error) {
	if s == nil || s.repository == nil {
		return IncidentRecord{}, ErrRepositoryUnavailable
	}
	if err := incident.validate(); err != nil {
		return IncidentRecord{}, err
	}
	candidate, err := s.repository.IncidentCandidate(ctx, incident.ActionID)
	if err != nil {
		return IncidentRecord{}, fmt.Errorf("load incident candidate: %w", err)
	}
	if candidate.Outcome != "success" || candidate.VerificationState != "verified" ||
		candidate.DecisionID <= 0 || candidate.VerificationID <= 0 {
		return IncidentRecord{}, ErrCreditNotEligible
	}
	minutes, _ := incidentMinutes(incident.Severity)
	record, err := s.repository.RecordIncident(ctx, IncidentCredit{
		DatabaseID: candidate.DatabaseID, Kind: incident.Kind,
		Severity: incident.Severity, CreditedMinutes: minutes,
		EvidenceID:   fmt.Sprintf("incident:%s:action:%d", incident.Kind, incident.ActionID),
		ModelVersion: incidentModelVersion, DecisionID: candidate.DecisionID,
		ActionLogID: incident.ActionID, VerificationID: candidate.VerificationID,
		OccurredAt: incident.OccurredAt,
	})
	if err != nil {
		return IncidentRecord{}, fmt.Errorf("record avoided incident: %w", err)
	}
	return record, nil
}

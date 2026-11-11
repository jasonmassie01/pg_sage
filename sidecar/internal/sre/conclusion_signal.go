package sre

import (
	"context"
	"strings"
	"time"
	"unicode"
)

// Conclusion follow-up (sre-08). The engineer who was paged for an
// incident gets its diagnosis. sre owns the signal: a typed, redacted
// summary of a final diagnosis of an incident, never SQL, query text,
// application or user names, or evidence payloads. The notify stack owns
// delivery (only where the page went). The notifier hands the newest
// settled conclusion of each incident over at most once (see
// conclusion_notifier.go).

// ConclusionSignal is what an investigation tells the notify stack.
type ConclusionSignal struct {
	InvestigationID UUID
	IncidentID      string
	CaseID          string
	Database        string
	Family          string
	State           State // concluded or inconclusive
	// RootLabel is the root cause's label; empty when inconclusive.
	RootLabel string
	// Reason is one redacted line of at most maxSignalRunes runes.
	Reason string
	// Severity is the incident's (info, warning or critical).
	Severity    string
	ConcludedAt time.Time
}

// ConclusionSink delivers a conclusion follow-up; the sidecar backs it
// with the notification dispatcher.
type ConclusionSink interface {
	DeliverConclusion(ctx context.Context, s ConclusionSignal) error
}

// TrackedIncident is the incident a conclusion answers, as RCA records it.
type TrackedIncident struct {
	Database string
	Severity string
}

// ConclusionIncidents reads the incident of a conclusion: ok is false when
// the incident is not (or no longer) recorded for this database.
type ConclusionIncidents interface {
	TrackedIncident(ctx context.Context, id string) (TrackedIncident, bool, error)
}

// ConclusionCandidate is an incident's newest settled conclusion, as the
// store selects it.
type ConclusionCandidate struct {
	InvestigationID UUID
	IncidentID      string
	CaseID          string
	Kind            TriggerKind
	State           State
	Family          string
	Root            string // the root node id
	RootLabel       string
	Reason          string
	ConcludedAt     time.Time
}

// maxSignalRunes bounds the reason and the root label of a signal.
const maxSignalRunes = 200

// buildConclusionSignal builds the signal of a final diagnosis of an
// incident; ok is false for any other run (failed, expired, cancelled,
// paused, live, or a state this version does not know) and for runs
// without an incident.
func buildConclusionSignal(c ConclusionCandidate, inc TrackedIncident) (ConclusionSignal,
	bool) {
	if c.State != StateConcluded && c.State != StateInconclusive {
		return ConclusionSignal{}, false
	}
	if strings.TrimSpace(c.IncidentID) == "" || c.InvestigationID == "" {
		return ConclusionSignal{}, false
	}
	sig := ConclusionSignal{InvestigationID: c.InvestigationID, IncidentID: c.IncidentID,
		CaseID: c.CaseID, Database: inc.Database, Family: signalLine(c.Family),
		State: c.State, Reason: signalLine(c.Reason), Severity: signalSeverity(inc.Severity),
		ConcludedAt: c.ConcludedAt}
	if sig.Family == "" {
		sig.Family = string(c.Kind)
	}
	if c.State == StateConcluded {
		sig.RootLabel = signalLine(c.RootLabel)
		if sig.RootLabel == "" {
			sig.RootLabel = signalLine(c.Root)
		}
	}
	return sig, true
}

// signalLine redacts untrusted text, folds it to one line and clips it at
// a rune boundary.
func signalLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, RedactText(s))
	return truncateRunes(strings.Join(strings.Fields(s), " "), maxSignalRunes)
}

func signalSeverity(sev string) string {
	switch sev {
	case "info", "warning", "critical":
		return sev
	}
	return "warning"
}

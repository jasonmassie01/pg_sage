package rca

import (
	"context"

	"github.com/pg-sage/sidecar/internal/notify"
)

// applyPersistResults folds database results back into memory and returns
// the notifications that became due. Durably resolved incidents leave
// memory here, which bounds the in-memory incident list (substrate-B7).
func (e *Engine) applyPersistResults(
	results []persistResult,
) ([]pendingEvent, EventDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()

	index := make(map[string]int, len(e.incidents))
	for i := range e.incidents {
		index[e.incidents[i].ID] = i
	}
	var events []pendingEvent
	remove := make(map[string]bool)
	for _, r := range results {
		i, ok := index[r.id]
		if !ok {
			continue
		}
		inc := &e.incidents[i]
		if r.gone {
			e.logFn("warn", "rca: incident %s was deleted externally; "+
				"no longer tracked", r.id)
			remove[r.id] = true
			continue
		}
		events = append(events, e.applyOne(inc, r)...)
		if r.resolved {
			remove[r.id] = true
		}
	}
	e.removeIncidents(remove)
	return events, e.dispatcher
}

func (e *Engine) applyOne(inc *Incident, r persistResult) []pendingEvent {
	ts := e.trackFor(inc.ID)
	ts.persisted = true
	if r.fingerprint != "" {
		ts.written = r.fingerprint
	}
	if r.inserted && r.previousID != "" {
		inc.PreviousIncidentID = r.previousID
	}
	if r.external != nil {
		inc.ResolvedAt = r.external.ResolvedAt
		inc.ResolvedBy = r.external.ResolvedBy
		inc.ResolutionReason = r.external.ResolutionReason
	}
	var events []notify.Event
	if ts.notifyDetected && r.inserted {
		events = append(events, notify.IncidentDetectedEvent(incidentInfo(inc)))
		ts.notifyDetected = false
	}
	if ts.notifyEscalated {
		events = append(events,
			notify.IncidentEscalatedEvent(incidentInfo(inc)))
		ts.notifyEscalated = false
	}
	if r.resolved && !ts.quiet {
		events = append(events, notify.IncidentResolvedEvent(incidentInfo(inc)))
	}
	pending := make([]pendingEvent, 0, len(events))
	for _, ev := range events {
		pending = append(pending, pendingEvent{event: ev, incident: *inc})
	}
	return pending
}

func incidentInfo(inc *Incident) notify.IncidentInfo {
	return notify.IncidentInfo{
		ID:                 inc.ID,
		Database:           inc.DatabaseName,
		Severity:           inc.Severity,
		RootCause:          inc.RootCause,
		Source:             inc.Source,
		SignalIDs:          append([]string(nil), inc.SignalIDs...),
		PreviousIncidentID: inc.PreviousIncidentID,
		ResolvedBy:         inc.ResolvedBy,
		Reason:             inc.ResolutionReason,
	}
}

// dispatchEvents sends notifications outside the engine lock. Delivery
// failures are logged; the incident state change is already durable.
func (e *Engine) dispatchEvents(
	ctx context.Context, d EventDispatcher, events []notify.Event,
) {
	if d == nil {
		return
	}
	for _, ev := range events {
		if err := d.Dispatch(ctx, ev); err != nil {
			e.logFn("warn", "rca: dispatch %s for incident %v: %v",
				ev.Type, ev.Data["incident_id"], err)
		}
	}
}

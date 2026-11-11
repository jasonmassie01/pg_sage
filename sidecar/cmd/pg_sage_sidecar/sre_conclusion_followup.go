package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/sre"
)

// conclusionSink delivers Sage SRE diagnosis follow-ups (sre-08) through
// the notification dispatcher: only to the channels the incident's page
// reached, as a reply to it where the channel threads.
type conclusionSink struct {
	dispatcher *notify.Dispatcher
}

// newConclusionSink is the follow-up sink of a dispatcher; nil (no
// follow-ups) without one.
func newConclusionSink(d *notify.Dispatcher) sre.ConclusionSink {
	if d == nil {
		return nil
	}
	return conclusionSink{dispatcher: d}
}

// DeliverConclusion sends the signal's follow-up for its incident.
func (s conclusionSink) DeliverConclusion(ctx context.Context, sig sre.ConclusionSignal) error {
	return s.dispatcher.FollowUp(ctx, sig.IncidentID,
		notify.InvestigationConcludedEvent(notify.ConclusionInfo{
			IncidentID: sig.IncidentID, InvestigationID: string(sig.InvestigationID),
			CaseID: sig.CaseID, Database: sig.Database, Family: sig.Family,
			State: string(sig.State), RootLabel: sig.RootLabel, Reason: sig.Reason,
			Severity: sig.Severity}))
}

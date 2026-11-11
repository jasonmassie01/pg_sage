package sre

import (
	"context"
	"time"
)

// The conclusion notifier (sre-08) runs in the coordinator loop each
// trigger interval. It selects the incidents of this database whose newest
// conclusion is at least ConclusionSettle old and at most ConclusionWindow
// old, with no run of the incident queued or running and no follow-up
// claimed. For each it reads the incident, claims the follow-up (one row
// per incident; the first sidecar to insert it wins) and hands the signal
// to the sink. A failed delivery keeps its claim: it is logged, recorded
// in the notification log, and never retried. Nothing here can fail an
// investigation.

// Notifier bounds.
const (
	ConclusionSettle  = 60 * time.Second
	ConclusionWindow  = time.Hour
	conclusionBatch   = 50
	conclusionTimeout = 30 * time.Second
	// conclusionTickBudget bounds the time one tick spends on follow-ups,
	// so slow channels never hold up new investigations: once it is spent,
	// the remaining (unclaimed) candidates wait for the next interval.
	conclusionTickBudget = 30 * time.Second
)

// ConclusionStore is the store surface the notifier needs.
type ConclusionStore interface {
	ConclusionCandidates(ctx context.Context, scope Scope, limit int) ([]ConclusionCandidate,
		error)
	ClaimFollowUp(ctx context.Context, incidentID string, id UUID) (bool, error)
	MarkFollowUpDelivered(ctx context.Context, incidentID string, id UUID) error
}

var _ ConclusionStore = (*PostgresStore)(nil)

// notifyConclusions hands each settled, unclaimed conclusion of this
// database to the follow-up sink once.
func (c *Coordinator) notifyConclusions(ctx context.Context, scope Scope) {
	if c.followUps == nil || ctx.Err() != nil {
		return
	}
	list, err := c.conclusions.ConclusionCandidates(ctx, scope, conclusionBatch)
	c.durability.Observe(err)
	if err != nil {
		c.logFn("WARN", "sre: listing concluded investigations to follow up failed: %v", err)
		return
	}
	start := c.now()
	for i, cand := range list {
		if ctx.Err() != nil {
			return
		}
		if c.now().Sub(start) >= conclusionTickBudget {
			c.logFn("INFO", "sre: %d diagnosis follow-ups wait for the next interval "+
				"(this interval's %s budget is spent)", len(list)-i, conclusionTickBudget)
			return
		}
		c.followUp(ctx, cand)
	}
}

func (c *Coordinator) followUp(ctx context.Context, cand ConclusionCandidate) {
	inc, tracked, err := c.trackedIncidents.TrackedIncident(ctx, cand.IncidentID)
	if err != nil {
		c.logFn("WARN", "sre: reading incident %s for its diagnosis follow-up failed "+
			"(retried next interval): %v", cand.IncidentID, err)
		return
	}
	sig, ok := buildConclusionSignal(cand, inc)
	if !tracked || !ok {
		return
	}
	claimed, err := c.conclusions.ClaimFollowUp(ctx, cand.IncidentID, cand.InvestigationID)
	c.durability.Observe(err)
	if err != nil || !claimed {
		if err != nil {
			c.logFn("WARN", "sre: claiming the follow-up of incident %s failed: %v",
				cand.IncidentID, err)
		}
		return
	}
	dctx, cancel := context.WithTimeout(ctx, conclusionTimeout)
	defer cancel()
	if err := c.followUps.DeliverConclusion(dctx, sig); err != nil {
		c.logFn("WARN", "sre: diagnosis follow-up of incident %s (investigation %s) "+
			"failed and is not retried: %v", cand.IncidentID, cand.InvestigationID, err)
		return
	}
	err = c.conclusions.MarkFollowUpDelivered(ctx, cand.IncidentID, cand.InvestigationID)
	c.durability.Observe(err)
	if err != nil {
		c.logFn("WARN", "sre: recording the delivered follow-up of incident %s failed: %v",
			cand.IncidentID, err)
	}
}

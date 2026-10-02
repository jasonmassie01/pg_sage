package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// RequestExecution queues a proposal in the existing approval flow:
// exactly one approval item per proposal, however often and by whomever
// it is requested (CHECK-39). It never executes anything. A proposal the
// standing policy would refuse even after approval is ErrPolicyBlocked
// and queues nothing.
func (a *ActionService) RequestExecution(ctx context.Context, id sre.UUID,
	actor string) (Proposal, error) {
	if !a.handoffAllowed() {
		return Proposal{}, ErrHandoffBlocked
	}
	p, err := a.Get(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	switch {
	case p.State == ProposalRequested:
		return p, nil
	case p.State != ProposalProposed || p.Target == nil:
		return p, fmt.Errorf("%w: proposal %s is %s", ErrProposalState, id, p.State)
	}
	verdict := verdictOf(a.exec.PreviewBackendCancel(ctx, p.Target.InRecovery))
	if !verdict.allows() {
		a.refreshPolicy(ctx, p, verdict)
		return p, fmt.Errorf("%w: %s", ErrPolicyBlocked, verdict)
	}
	item, err := a.queue.Enqueue(ctx, ApprovalItem{ProposalID: p.ID,
		InvestigationID: p.InvestigationID, PID: p.Target.PID, SQL: p.SQL,
		Title: cancelTitle(p.Target.PID), Detail: p.summary(a.svc.Name()),
		Database: a.svc.Name(), ExpiresAt: time.Now().Add(a.cfg.ApprovalTTL)})
	if err != nil {
		return p, err
	}
	out, err := a.markRequested(ctx, p, item, verdict, actor)
	if errors.Is(err, ErrProposalState) {
		// A concurrent request won; it queued the same item and notified.
		return a.Get(ctx, id)
	}
	if err != nil {
		return out, err
	}
	a.notify(ctx, out)
	return out, nil
}

func (a *ActionService) markRequested(ctx context.Context, p Proposal, item QueuedItem,
	verdict PolicyVerdict, actor string) (Proposal, error) {
	st, err := a.queue.Status(ctx, item.QueueID)
	if err != nil {
		return p, err
	}
	out, err := a.ps.mutate(ctx, p.Scope, p.ID,
		[]ProposalState{ProposalProposed}, actor, func(q *Proposal) ([]actionEvent, error) {
			now := time.Now()
			q.State, q.QueueID, q.FindingID, q.Policy = ProposalRequested, item.QueueID,
				item.FindingID, verdict
			q.RequestedBy, q.RequestedAt, q.ExpiresAt = actor, &now, st.ExpiresAt
			return []actionEvent{{typ: "action_requested", payload: map[string]any{
				"proposal_id": string(q.ID), "queue_id": item.QueueID,
				"finding_id": item.FindingID, "expires_at": st.ExpiresAt}}}, nil
		})
	return out, a.observe(err)
}

// refreshPolicy stores a changed policy verdict (no event: nothing
// happened to the proposal).
func (a *ActionService) refreshPolicy(ctx context.Context, p Proposal,
	verdict PolicyVerdict) {
	if p.Policy == verdict {
		return
	}
	_, err := a.ps.mutate(ctx, p.Scope, p.ID,
		[]ProposalState{ProposalProposed}, systemActor,
		func(q *Proposal) ([]actionEvent, error) {
			q.Policy = verdict
			return nil, nil
		})
	if err != nil {
		a.logFn("WARN", "sre: proposal %s: storing the policy verdict failed: %v", p.ID,
			a.observe(err))
	}
}

// notify sends the approval request to chat; a failed notification never
// undoes the request (the queue item is visible in the UI).
func (a *ActionService) notify(ctx context.Context, p Proposal) {
	if a.notifier == nil {
		return
	}
	err := a.notifier.ApprovalRequested(ctx, ApprovalRequest{Database: a.svc.Name(),
		ProposalID: p.ID, InvestigationID: p.InvestigationID, QueueID: p.QueueID,
		Title: cancelTitle(p.Target.PID), Summary: p.summary(a.svc.Name()),
		Risk: p.Contract.BaseRiskTier, ExpiresAt: p.ExpiresAt})
	if err != nil {
		a.logFn("WARN", "sre: proposal %s: sending the approval request failed: %v",
			p.ID, err)
	}
}

package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// RunApproved runs a proposal's approved queue item for its approver
// (executor.ApprovedActionRunner). It claims the proposal once (a second
// run, concurrent or replayed, is ErrProposalState), samples the target
// again and compares the whole identity, then hands exactly one cancel to
// the executor, which authorizes it at the policy gate and rechecks the
// identity in the signalling statement. A changed or missing target is
// refused with the reason on the timeline and never signalled; what
// happened instead is attributed and verified (CHECK-19, CHECK-23).
func (a *ActionService) RunApproved(ctx context.Context, action store.QueuedAction,
	approvedBy int) (executor.ApprovedRun, error) {
	if !a.handoffAllowed() {
		return executor.ApprovedRun{}, ErrHandoffBlocked
	}
	p, err := a.approvedProposal(ctx, action)
	if err != nil {
		return executor.ApprovedRun{}, err
	}
	p, err = a.claim(ctx, p, action, approvedBy)
	if err != nil {
		return executor.ApprovedRun{}, err
	}
	fresh, diff, why := a.recheck(ctx, *p.Target)
	if err := a.recordRecheck(ctx, p, fresh, diff, why); err != nil {
		return executor.ApprovedRun{}, err
	}
	if why != nil {
		return executor.ApprovedRun{}, a.refuse(ctx, p, why)
	}
	actionID, err := a.exec.CancelBackend(ctx, executor.BackendCancel{
		Target: fresh.Identity(), ObservedAt: fresh.ObservedAt,
		MaxEvidenceAge: a.cfg.MaxEvidenceAge, FindingID: p.FindingID,
		ApprovedBy: approvedBy, IsReplica: fresh.InRecovery,
		ProtectedRoles: a.cfg.ProtectedRoles, ProtectedApplications: a.cfg.ProtectedApplications,
		Evidence: map[string]any{"proposal_id": string(p.ID),
			"investigation_id": string(p.InvestigationID), "queue_id": p.QueueID,
			"evidence_ids": evidenceText(p.EvidenceIDs)}})
	if err != nil {
		return executor.ApprovedRun{}, a.executionFailed(ctx, p, err)
	}
	if err := a.executed(ctx, p, actionID); err != nil {
		return executor.ApprovedRun{ActionLogID: actionID}, err
	}
	return executor.ApprovedRun{ActionLogID: actionID, VerificationStatus: "monitoring"}, nil
}

// approvedProposal is the proposal an approved item belongs to.
func (a *ActionService) approvedProposal(ctx context.Context,
	action store.QueuedAction) (Proposal, error) {
	id, ok := ProposalIDFromIdentityKey(action.IdentityKey)
	if !ok {
		return Proposal{}, fmt.Errorf("%w: queue item %d is not a Sage SRE proposal",
			ErrProposalNotFound, action.ID)
	}
	p, err := a.Get(ctx, id)
	switch {
	case err != nil:
		return Proposal{}, err
	case p.QueueID != action.ID || p.Target == nil:
		return Proposal{}, fmt.Errorf("%w: queue item %d is not proposal %s's approval",
			ErrProposalState, action.ID, id)
	case action.Status != "approved":
		return Proposal{}, fmt.Errorf("%w: queue item %d is %s, not approved",
			ErrProposalState, action.ID, action.Status)
	}
	return p, nil
}

// claim moves a requested proposal to executing for its approver, once.
func (a *ActionService) claim(ctx context.Context, p Proposal, action store.QueuedAction,
	approvedBy int) (Proposal, error) {
	out, err := a.ps.mutate(ctx, p.Scope, p.ID,
		[]ProposalState{ProposalRequested}, userActor(approvedBy),
		func(q *Proposal) ([]actionEvent, error) {
			now := time.Now()
			q.State, q.DecidedBy, q.DecidedAt = ProposalExecuting, approvedBy, &now
			return []actionEvent{{typ: "action_decided", payload: map[string]any{
				"proposal_id": string(q.ID), "decision": "approved",
				"approved_by": approvedBy, "queue_id": action.ID}}}, nil
		})
	return out, a.observe(err)
}

func userActor(id int) string { return fmt.Sprintf("user:%d", id) }

// recordRecheck puts the fresh identity sample on the timeline.
func (a *ActionService) recordRecheck(ctx context.Context, p Proposal,
	fresh BackendTarget, diff []string, why *Ineligible) error {
	payload := map[string]any{"proposal_id": string(p.ID), "match": why == nil,
		"observed_at": fresh.ObservedAt, "state": fresh.State, "blocking": fresh.Blocking,
		"query_start": fresh.QueryStart, "mismatch": diff}
	if why != nil {
		payload["reason"] = string(why.Reason)
	}
	_, err := a.ps.mutate(ctx, p.Scope, p.ID,
		[]ProposalState{ProposalExecuting}, systemActor,
		func(*Proposal) ([]actionEvent, error) {
			return []actionEvent{{typ: "action_recheck", payload: payload}}, nil
		})
	return a.observe(err)
}

// refuse ends a run whose target changed, went away or stopped blocking:
// nothing is signalled, and recovery is still verified, attributed to an
// external or natural change.
func (a *ActionService) refuse(ctx context.Context, p Proposal, why *Ineligible) error {
	verify := why.Reason == ReasonTargetGone || why.Reason == ReasonTargetChanged ||
		why.Reason == ReasonNotBlocking || why.Reason == ReasonTargetNotActive
	_, err := a.ps.mutate(ctx, p.Scope, p.ID,
		[]ProposalState{ProposalExecuting}, systemActor,
		func(q *Proposal) ([]actionEvent, error) {
			q.State, q.Reason, q.Detail = ProposalRefused, why.Reason, why.Detail
			if verify {
				q.Recovery = a.startRecovery(AttributionExternal)
			}
			return []actionEvent{{typ: "action_refused", payload: map[string]any{
				"proposal_id": string(q.ID), "reason": string(why.Reason),
				"detail": why.Detail, "signalled": false}}}, nil
		})
	if err != nil {
		a.logFn("ERROR", "sre: proposal %s: recording the refusal failed: %v", p.ID,
			a.observe(err))
	}
	a.resolveItem(ctx, p)
	return fmt.Errorf("%w: %s", executor.ErrBackendEvidenceStale, why.Detail)
}

// startRecovery begins a recovery verification now.
func (a *ActionService) startRecovery(attribution string) RecoveryRecord {
	now := a.now()
	return RecoveryRecord{State: RecoveryObserving, Attribution: attribution,
		StartedAt: time.Now(), Deadline: now.Add(a.cfg.RecoveryDeadline),
		NextSampleAt: now.Add(a.cfg.RecoveryInterval)}
}

// executionFailed records an executor refusal or failure and returns the
// executor's error.
func (a *ActionService) executionFailed(ctx context.Context, p Proposal, cause error) error {
	state, reason := ProposalFailed, ReasonExecutionError
	var withheld *executor.WithheldError
	switch {
	case errors.As(cause, &withheld):
		state, reason = ProposalRefused, ReasonPolicyWithheld
	case errors.Is(cause, executor.ErrBackendEvidenceStale):
		state, reason = ProposalRefused, ReasonEvidenceStale
	}
	_, err := a.ps.mutate(ctx, p.Scope, p.ID,
		[]ProposalState{ProposalExecuting}, systemActor,
		func(q *Proposal) ([]actionEvent, error) {
			q.State, q.Reason, q.Detail = state, reason, cause.Error()
			typ := "action_refused"
			if state == ProposalFailed {
				typ = "action_failed"
			}
			return []actionEvent{{typ: typ, payload: map[string]any{
				"proposal_id": string(q.ID), "reason": string(reason),
				"detail": cause.Error()}}}, nil
		})
	if err != nil {
		a.logFn("ERROR", "sre: proposal %s: recording the %s outcome failed: %v", p.ID,
			state, a.observe(err))
	}
	a.resolveItem(ctx, p)
	return cause
}

// executed records the sent cancel and starts recovery verification.
func (a *ActionService) executed(ctx context.Context, p Proposal, actionID int64) error {
	_, err := a.ps.mutate(ctx, p.Scope, p.ID,
		[]ProposalState{ProposalExecuting}, systemActor,
		func(q *Proposal) ([]actionEvent, error) {
			now := time.Now()
			q.State, q.ActionLogID, q.ExecutedAt = ProposalExecuted, actionID, &now
			q.Recovery = a.startRecovery(AttributionSage)
			return []actionEvent{{typ: "action_executed", payload: map[string]any{
				"proposal_id": string(q.ID), "action_log_id": actionID,
				"pid": q.Target.PID, "recovery_deadline": q.Recovery.Deadline}}}, nil
		})
	if err != nil {
		a.logFn("ERROR", "sre: proposal %s: the cancel was sent (action %d) but "+
			"recording it failed: %v", p.ID, actionID, a.observe(err))
	}
	return a.observe(err)
}

// resolveItem closes a finished proposal's approval anchor.
func (a *ActionService) resolveItem(ctx context.Context, p Proposal) {
	if p.QueueID <= 0 {
		return
	}
	if err := a.queue.Resolve(ctx, p.QueueID); err != nil {
		a.logFn("WARN", "sre: proposal %s: closing approval item %d failed: %v", p.ID,
			p.QueueID, err)
	}
}

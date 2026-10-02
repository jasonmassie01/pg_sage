package action

import (
	"context"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// abandonedAfter is how long a proposal may stay executing before the
// service assumes the run died (a crash between claim and record). The
// signal may or may not have been sent; it is never retried.
const abandonedAfter = 5 * time.Minute

// Run proposes, syncs approvals and verifies recovery until ctx ends.
func (a *ActionService) Run(ctx context.Context) {
	t := time.NewTicker(a.cfg.PollInterval)
	defer t.Stop()
	for {
		if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
			a.logFn("WARN", "sre: db %q: action pass: %v", a.svc.Name(), err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one pass: automatic proposals (and approval requests) for
// newly concluded investigations, approval decisions made in the queue,
// due recovery samples and abandoned executions.
func (a *ActionService) Tick(ctx context.Context) error {
	scope, err := a.scope(ctx)
	if err != nil {
		return err
	}
	var errs []error
	if a.cfg.Proposals {
		errs = append(errs, a.autoPropose(ctx, scope))
	}
	errs = append(errs, a.syncRequested(ctx, scope), a.verifyDue(ctx, scope),
		a.markAbandoned(ctx, scope))
	return errors.Join(errs...)
}

// autoPropose proposes for concluded lock and connection investigations
// that have none yet, and requests approval when the policy would allow
// the approved action.
func (a *ActionService) autoPropose(ctx context.Context, scope sre.Scope) error {
	rows, err := a.st.Pool().Query(ctx, `SELECT i.id::text FROM sage.sre_investigations i
		WHERE i.deployment_id = $1 AND i.database_id = $2 AND i.state = 'concluded'
		  AND i.trigger_kind IN ('lock_blocking', 'connection_pressure')
		  AND i.concluded_at > clock_timestamp() - make_interval(secs => $3)
		  AND NOT EXISTS (SELECT 1 FROM sage.sre_action_proposals p
		      WHERE p.deployment_id = i.deployment_id AND p.database_id = i.database_id
		        AND p.investigation_id = i.id)
		ORDER BY i.concluded_at LIMIT 10`, string(scope.DeploymentID),
		string(scope.DatabaseID), a.cfg.ApprovalTTL.Seconds())
	if err != nil {
		return a.observe(sre.StoreError(ctx, "pending proposals", err))
	}
	ids, err := textColumn(rows)
	if err != nil {
		return a.observe(sre.StoreError(ctx, "pending proposals", err))
	}
	var errs []error
	for _, id := range ids {
		p, err := a.Propose(ctx, sre.UUID(id), systemActor)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if p.State == ProposalProposed && a.cfg.RequestApproval && p.Policy.allows() {
			if _, err := a.RequestExecution(ctx, p.ID, systemActor); err != nil &&
				!errors.Is(err, ErrPolicyBlocked) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// syncRequested applies decisions made in the approval queue (a denial
// in the UI or chat, an expiry) to requested proposals.
func (a *ActionService) syncRequested(ctx context.Context, scope sre.Scope) error {
	ps, err := a.ps.list(ctx, scope, `state = 'requested' ORDER BY updated_at
		LIMIT 50`)
	if err != nil {
		return a.observe(err)
	}
	var errs []error
	for _, p := range ps {
		errs = append(errs, a.SyncProposal(ctx, p))
	}
	return errors.Join(errs...)
}

// SyncProposal applies a requested proposal's queue decision now (ChatOps
// calls it right after a denial).
func (a *ActionService) SyncProposal(ctx context.Context, p Proposal) error {
	if p.State != ProposalRequested || p.QueueID <= 0 {
		return nil
	}
	st, err := a.queue.Status(ctx, p.QueueID)
	if err != nil {
		return err
	}
	state := map[string]ProposalState{"rejected": ProposalDenied,
		"expired": ProposalExpired}[st.Status]
	if state == "" {
		return nil
	}
	_, err = a.ps.mutate(ctx, p.Scope, p.ID,
		[]ProposalState{ProposalRequested}, systemActor,
		func(q *Proposal) ([]actionEvent, error) {
			q.State, q.Detail = state, st.Reason
			payload := map[string]any{"proposal_id": string(q.ID),
				"decision": map[ProposalState]string{ProposalDenied: "denied",
					ProposalExpired: "expired"}[state], "queue_id": q.QueueID}
			if st.DecidedBy > 0 {
				now := time.Now()
				q.DecidedBy, q.DecidedAt = st.DecidedBy, &now
				payload["decided_by"] = st.DecidedBy
			}
			return []actionEvent{{typ: "action_decided", payload: payload}}, nil
		})
	if errors.Is(err, ErrProposalState) {
		return nil
	}
	if err != nil {
		return a.observe(err)
	}
	a.resolveItem(ctx, p)
	return nil
}

// verifyDue takes due recovery samples.
func (a *ActionService) verifyDue(ctx context.Context, scope sre.Scope) error {
	ps, err := a.ps.list(ctx, scope, `recovery_state = 'observing'
		AND next_sample_at <= $3 ORDER BY next_sample_at LIMIT 20`, a.now())
	if err != nil {
		return a.observe(err)
	}
	var errs []error
	for _, p := range ps {
		errs = append(errs, a.sampleRecovery(ctx, p))
	}
	return errors.Join(errs...)
}

// sampleRecovery takes one fresh sample and decides the verdict when it
// can.
func (a *ActionService) sampleRecovery(ctx context.Context, p Proposal) error {
	if p.Target == nil {
		return nil
	}
	res := a.targets.Run(ctx, probes.RecoverySample, probes.Args{PID: p.Target.PID,
		BackendStart: p.Target.BackendStart})
	rows, err := probes.RecoveryRows(res)
	if err != nil && res.Status.Usable() {
		res.Status, res.Reason = probes.StatusError, "unreadable: "+err.Error()
	}
	sample := summarizeSample(res, rows, *p.Target, p.Baseline, p.Recovery.StartedAt)
	out, err := a.ps.mutate(ctx, p.Scope, p.ID, []ProposalState{p.State},
		systemActor, func(q *Proposal) ([]actionEvent, error) {
			return a.applySample(q, sample), nil
		})
	if err != nil {
		return a.observe(err)
	}
	if out.Recovery.State != RecoveryObserving {
		a.closeVerification(ctx, out)
	}
	return nil
}

// applySample adds a sample and, when the record is decided, the verdict.
func (a *ActionService) applySample(q *Proposal, s RecoverySample) []actionEvent {
	q.Recovery.add(s)
	events := []actionEvent{{typ: "recovery_sample", payload: map[string]any{
		"proposal_id": string(q.ID), "sample": s}}}
	state, verdict := evaluateRecovery(q.Family, q.Baseline, q.Recovery, a.cfg, a.now())
	if state == RecoveryObserving {
		q.Recovery.NextSampleAt = a.now().Add(a.cfg.RecoveryInterval)
		return events
	}
	q.Recovery.State, q.Recovery.Verdict, q.Recovery.DecidedAt = state, verdict, time.Now()
	return append(events, actionEvent{typ: "recovery_verdict", payload: map[string]any{
		"proposal_id": string(q.ID), "state": string(state), "verdict": verdict,
		"attribution": q.Recovery.Attribution, "samples": len(q.Recovery.Samples)}})
}

// closeVerification records the verdict on the approval item and the
// executor's verification.
func (a *ActionService) closeVerification(ctx context.Context, p Proposal) {
	queueStatus, execVerdict := queueVerification(p.Recovery.State)
	if p.QueueID > 0 {
		if err := a.queue.SetVerification(ctx, p.QueueID, queueStatus); err != nil {
			a.logFn("WARN", "sre: proposal %s: recording the verification failed: %v",
				p.ID, err)
		}
	}
	rec, ok := a.exec.(recoveryRecorder)
	if !ok || p.ActionLogID <= 0 {
		return
	}
	if err := rec.RecordRecoveryVerdict(ctx, p.ActionLogID, execVerdict,
		p.Recovery.Verdict); err != nil {
		a.logFn("WARN", "sre: proposal %s: closing action %d's verification failed: %v",
			p.ID, p.ActionLogID, err)
	}
}

// markAbandoned turns runs that died between claim and record into
// uncertain outcomes and verifies recovery; it never retries the signal.
func (a *ActionService) markAbandoned(ctx context.Context, scope sre.Scope) error {
	ps, err := a.ps.list(ctx, scope, `state = 'executing'
		AND updated_at < clock_timestamp() - make_interval(secs => $3) LIMIT 10`,
		abandonedAfter.Seconds())
	if err != nil {
		return a.observe(err)
	}
	var errs []error
	for _, p := range ps {
		_, err := a.ps.mutate(ctx, scope, p.ID,
			[]ProposalState{ProposalExecuting}, systemActor,
			func(q *Proposal) ([]actionEvent, error) {
				q.State, q.Reason = ProposalUncertain, ReasonExecutionError
				q.Detail = "the run ended without recording its outcome; the cancel " +
					"may or may not have been sent and is never retried"
				q.Recovery = a.startRecovery(AttributionUnknown)
				return []actionEvent{{typ: "action_failed", payload: map[string]any{
					"proposal_id": string(q.ID), "outcome": "uncertain"}}}, nil
			})
		errs = append(errs, a.observe(err))
	}
	return errors.Join(errs...)
}

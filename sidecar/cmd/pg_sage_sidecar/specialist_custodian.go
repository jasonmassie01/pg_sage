package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/specialist"
	"github.com/pg-sage/sidecar/internal/sre"
)

// SubmitCustodian implements specialist.CustodianSubmitter: the custodian
// re-scans now, and only a proposal it still makes, with the same SQL and
// targets, is submitted, through the executor's one pipeline exactly as
// the custodian worker would (fresh evidence, HA state, no approval flag).
// The requester is recorded in the evidence; it lends no authority.
func (c *specialistCustodians) SubmitCustodian(ctx context.Context, database string,
	inv sre.Investigation, want sre.ActionProposal, actor string) (specialist.GateOutcome,
	error) {
	a := c.advisor(database)
	if a == nil {
		return specialist.GateOutcome{}, fmt.Errorf("%w: no custodians for database %q",
			specialist.ErrUnavailable, database)
	}
	sub, found, err := a.submitRequested(ctx, sre.AdviceRequest{Kind: inv.TriggerKind,
		Subject: inv.Subject, Root: inv.Summary.Root}, want, actor)
	switch {
	case errors.Is(err, executor.ErrCustodianBackoff):
		return specialist.GateOutcome{Decision: executor.PolicyDecisionBlocked,
			Reason: "custodian_backoff", Detail: err.Error()}, nil
	case err != nil && !errors.Is(err, executor.ErrCustodianProposalWithheld) &&
		!sub.Executed:
		return specialist.GateOutcome{}, err
	case !found:
		return specialist.GateOutcome{Stale: true,
			Reason: "the custodian no longer proposes this action"}, nil
	}
	out := specialist.GateOutcome{Decision: sub.Decision.Decision,
		Reason: sub.Decision.BlockedReason, Detail: sub.Decision.Detail,
		ActionID: sub.ActionID, Executed: sub.Executed, HandedOff: sub.HandedOff}
	if sub.Executed && err != nil {
		out.Detail = "executed; post-check: " + err.Error()
	}
	return out, nil
}

// submitRequested re-scans the custodian that serves req and submits the
// proposal matching want; found is false when the custodian no longer
// makes it.
func (a *runwayAdvisor) submitRequested(ctx context.Context, req sre.AdviceRequest,
	want sre.ActionProposal, actor string) (executor.CustodianSubmission, bool, error) {
	exec := a.exec.Load()
	if exec == nil {
		return executor.CustodianSubmission{}, false, fmt.Errorf("%w: the executor is "+
			"not running yet", specialist.ErrUnavailable)
	}
	custodian := a.custodianFor(req)
	if custodian == nil {
		return executor.CustodianSubmission{}, false, nil
	}
	scanned, err := custodian.Scan(ctx)
	if err != nil {
		return executor.CustodianSubmission{}, false, fmt.Errorf("custodian scan: %w", err)
	}
	for _, p := range scanned {
		if !sameProposal(p, want) {
			continue
		}
		evidence := map[string]any{}
		for k, v := range p.Evidence {
			evidence[k] = v
		}
		evidence["requested_by"], evidence["requested_via"] = actor, "specialist"
		sub, err := exec.SubmitCustodianProposalDecision(ctx, executor.CustodianProposal{
			Feature: p.Feature, SQL: p.SQL, TargetObjects: append([]string(nil),
				p.TargetObjects...), Deadline: p.Deadline, Evidence: evidence,
			IsReplica: a.isReplica(ctx), ObservedAt: time.Now()})
		return sub, true, err
	}
	return executor.CustodianSubmission{}, false, nil
}

// custodianFor is the custodian whose actions address req (the advisor's
// own selection).
func (a *runwayAdvisor) custodianFor(req sre.AdviceRequest) autonomy.Custodian {
	switch req.Kind {
	case sre.TriggerWraparound:
		return a.freeze
	case sre.TriggerDiskWAL:
		if req.Root == "inactive_slot" || req.Root == "slow_consumer" {
			return a.wal
		}
	}
	return nil
}

func sameProposal(p autonomy.Proposal, want sre.ActionProposal) bool {
	return strings.TrimSpace(p.SQL) != "" && p.Feature == want.Feature &&
		p.SQL == want.SQL && slices.Equal(p.TargetObjects, want.Targets)
}

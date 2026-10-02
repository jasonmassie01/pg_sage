package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
)

// runwayAdvisor answers a concluded pre-incident investigation with the
// existing custodian actions that address it: the freeze custodian's
// proposals for a wraparound runway, the WAL custodian's bound (or its
// escalation plan) for slot retention. It runs the custodians' own
// read-only scans and asks the standing gate to explain each proposal;
// it never authorizes, records or executes anything. The custodian
// workers submit their proposals on their own tick under the configured
// autonomy. Sequences have no custodian action: the advisor says so.
type runwayAdvisor struct {
	freeze    autonomy.Custodian
	wal       autonomy.Custodian
	isReplica func(context.Context) bool
	exec      atomic.Pointer[executor.Executor]
}

// manualOnly marks a proposal no custodian action covers.
const manualOnly = "manual_only"

func newRunwayAdvisor(freeze, wal autonomy.Custodian,
	isReplica func(context.Context) bool) *runwayAdvisor {
	return &runwayAdvisor{freeze: freeze, wal: wal, isReplica: isReplica}
}

// attach supplies the executor once it is built (after the investigator).
func (a *runwayAdvisor) attach(e *executor.Executor) { a.exec.Store(e) }

// Advise implements sre.ActionAdvisor.
func (a *runwayAdvisor) Advise(ctx context.Context,
	req sre.AdviceRequest) ([]sre.ActionProposal, error) {
	switch req.Kind {
	case sre.TriggerSequence:
		name := strings.TrimPrefix(req.Subject, "sequence ")
		return []sre.ActionProposal{{Feature: "sequence", Targets: []string{name},
			Action: "Change the binding limit through a reviewed manual migration " +
				"(the investigation's operator step); no custodian action covers it",
			Verdict: manualOnly, RiskTier: "high"}}, nil
	case sre.TriggerWraparound:
		return a.fromCustodian(ctx, a.freeze, func(p autonomy.Proposal) bool {
			table, byTable := strings.CutPrefix(req.Subject, "table ")
			return !byTable || (len(p.TargetObjects) > 0 && p.TargetObjects[0] == table)
		})
	case sre.TriggerDiskWAL:
		if req.Root != "inactive_slot" && req.Root != "slow_consumer" {
			return nil, nil // only slot retention has a custodian action (the bound)
		}
		return a.fromCustodian(ctx, a.wal, func(autonomy.Proposal) bool { return true })
	}
	return nil, nil
}

// fromCustodian scans one custodian and explains up to MaxProposals of
// its proposals that match.
func (a *runwayAdvisor) fromCustodian(ctx context.Context, c autonomy.Custodian,
	match func(autonomy.Proposal) bool) ([]sre.ActionProposal, error) {
	exec := a.exec.Load()
	if exec == nil {
		return nil, fmt.Errorf("the executor is not running yet")
	}
	scanned, err := c.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("custodian scan: %w", err)
	}
	var out []sre.ActionProposal
	for _, p := range scanned {
		if len(out) == sre.MaxProposals {
			break
		}
		if match(p) {
			out = append(out, a.explain(ctx, exec, p))
		}
	}
	return out, nil
}

func (a *runwayAdvisor) explain(ctx context.Context, exec *executor.Executor,
	p autonomy.Proposal) sre.ActionProposal {
	out := sre.ActionProposal{Feature: p.Feature, SQL: p.SQL,
		Targets: append([]string(nil), p.TargetObjects...)}
	if strings.TrimSpace(p.SQL) == "" {
		out.Action, out.Verdict = p.Plan, manualOnly
		out.Reason = "the custodian escalates this to an operator"
		return out
	}
	d := exec.ExplainCustodianProposal(ctx, executor.CustodianProposal{
		Feature: p.Feature, SQL: p.SQL, TargetObjects: out.Targets,
		Deadline: p.Deadline, Evidence: p.Evidence, IsReplica: a.isReplica(ctx)})
	out.Action, out.Verdict, out.Reason, out.RiskTier = p.SQL, d.Decision,
		d.BlockedReason, d.RiskTier
	return out
}

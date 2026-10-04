package executor

import (
	"context"
	"errors"
	"fmt"
)

// CustodianSubmission is what happened to a submitted custodian proposal:
// the standing gate's decision, and whether the change ran or was handed
// to the approval queue.
type CustodianSubmission struct {
	Decision  ActionPolicyDecision
	HandedOff bool
	Executed  bool
	ActionID  int64
}

// SubmitCustodianProposalDecision runs a custodian change through Apply
// (the one pipeline: policy gate, trust ledger, budgets, binding facts,
// lease, re-authorization, post-check) exactly like SubmitCustodianProposal
// and also reports the gate's decision, so a caller that asked for the
// proposal (the Postgres-specialist contract) can be told the verdict.
// A withheld proposal returns ErrCustodianProposalWithheld with the
// decision; a handoff to the approval queue is not an error.
func (e *Executor) SubmitCustodianProposalDecision(
	ctx context.Context, proposal CustodianProposal,
) (CustodianSubmission, error) {
	if err := e.custodianBackoff(ctx, proposal.SQL); err != nil {
		return CustodianSubmission{}, err
	}
	run := &custodianRun{executor: e, proposal: proposal,
		finding: custodianFinding(proposal)}
	actionID, err := e.Apply(ctx, ActionIntent{
		Request: custodianRequest(proposal), Lease: &run.finding, WaitForSlot: true,
		Admit: func(context.Context, int64) error {
			if e.pool == nil {
				return fmt.Errorf("execute custodian proposal: database pool unavailable")
			}
			return nil
		},
		Execute: run.execute, Verify: run.verify,
	})
	out := CustodianSubmission{Decision: run.decision, Executed: run.executed,
		ActionID: actionID}
	var withheld *WithheldError
	if errors.As(err, &withheld) {
		out.Decision = withheld.Decision
	}
	e.shadowWithheldCustodian(ctx, proposal, err)
	if e.handOffForApproval(ctx, proposal, err) {
		out.HandedOff = true
		return out, nil
	}
	return out, custodianWithheld(err, "after lease")
}

// CustodianContract is the typed action contract of a custodian proposal's
// SQL (risk tier, rollback class, success criteria); false when the
// executor would refuse the SQL.
func CustodianContract(sql string) (ActionContract, bool) {
	return contractForCustodianProposal(sql)
}

package specialist

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

const remediationNotice = "pg_sage decided this through its policy gate, trust ledger, " +
	"budgets and binding facts, exactly as for its own proposals. A caller can request " +
	"a remediation; it can never approve, force or bypass one."

// RequestRemediation asks pg_sage to act on one of a concluded
// investigation's candidate remediations. It needs the propose scope. The
// request becomes an ordinary proposal: a cancel is queued for a person's
// approval through the action service, a custodian action is re-scanned
// and submitted through the executor's pipeline. The answer is the gate's
// verdict and reason; the caller's note is audited, never acted on.
func (s *Service) RequestRemediation(ctx context.Context, id Identity, database, invID,
	remID string, req RemediationRequest) (RemediationResponse, error) {
	b, err := s.admit(id, ScopePropose, database, true)
	if err != nil {
		return RemediationResponse{}, err
	}
	uid, err := parseInvestigationID(invID)
	if err != nil {
		return RemediationResponse{}, err
	}
	if !remediationIDPattern.MatchString(remID) {
		return RemediationResponse{}, invalidf("malformed remediation id")
	}
	if err := checkText("reason", req.Reason, false, maxReasonRunes, true); err != nil {
		return RemediationResponse{}, err
	}
	d, err := b.Detail(ctx, uid)
	if err != nil {
		return RemediationResponse{}, backendErr(err)
	}
	if !requestableState(d.Investigation) {
		return RemediationResponse{}, fmt.Errorf("%w: the investigation is %s; only a "+
			"concluded investigation's remediations can be requested", ErrNotRequestable,
			d.Investigation.State)
	}
	resp, err := s.route(ctx, b, d.Investigation, remID, id.Actor())
	if err != nil {
		return RemediationResponse{}, err
	}
	resp.ContractVersion, resp.Database, resp.InvestigationID = ContractVersion, database,
		string(uid)
	resp.RemediationID, resp.RequestedBy, resp.Notice = remID, id.Actor(), remediationNotice
	if err := s.audit(ctx, id, database, uid, remID, resp, req.Reason); err != nil {
		// The request already reached pg_sage's own path, which records it
		// on the investigation's chain and the approval queue; the caller
		// must learn the verdict, so the audit gap is logged, not returned.
		s.log.Error("specialist: auditing a remediation request failed", "database",
			database, "investigation", uid, "remediation", remID, "actor", id.Actor(),
			"verdict", resp.Verdict, "err", err)
	}
	return resp, nil
}

// route sends the request down pg_sage's own path for its class.
func (s *Service) route(ctx context.Context, b Backend, inv sre.Investigation, remID,
	actor string) (RemediationResponse, error) {
	switch {
	case strings.HasPrefix(remID, cancelPrefix):
		return s.requestCancel(ctx, b, inv, strings.TrimPrefix(remID, cancelPrefix), actor)
	case strings.HasPrefix(remID, custodianPrefix):
		for _, p := range inv.Summary.Proposals {
			if custodianRemediationID(p) == remID {
				return s.requestCustodian(ctx, b, inv, p, actor)
			}
		}
	}
	return RemediationResponse{}, fmt.Errorf("%w: remediation %s is not a candidate of "+
		"this investigation", ErrNotFound, remID)
}

func (s *Service) requestCancel(ctx context.Context, b Backend, inv sre.Investigation,
	raw, actor string) (RemediationResponse, error) {
	pid, err := sre.ParseUUID(raw)
	if err != nil {
		return RemediationResponse{}, invalidf("malformed cancel remediation id")
	}
	proposals, err := b.Proposals(ctx, inv.ID)
	if err != nil && !errors.Is(err, ErrNoActions) {
		return RemediationResponse{}, backendErr(err)
	}
	for _, p := range proposals {
		if p.ID == pid && p.InvestigationID == inv.ID {
			return s.requestProposal(ctx, b, p, actor)
		}
	}
	return RemediationResponse{}, fmt.Errorf("%w: no cancel proposal %s in this "+
		"investigation", ErrNotFound, pid)
}

func (s *Service) requestProposal(ctx context.Context, b Backend, p sreaction.ProposalView,
	actor string) (RemediationResponse, error) {
	resp := RemediationResponse{ProposalID: string(p.ID)}
	switch p.State {
	case sreaction.ProposalRequested:
		resp.Verdict, resp.Reason, resp.ApprovalQueueID = "already_requested",
			"the proposal is already in the approval queue", p.QueueID
		return resp, nil
	case sreaction.ProposalProposed:
	default:
		resp.Verdict, resp.Reason = "not_requestable", "the proposal is "+string(p.State)
		return resp, nil
	}
	v, err := b.RequestProposal(ctx, p.ID, actor)
	switch {
	case errors.Is(err, sreaction.ErrPolicyBlocked):
		resp.Verdict, resp.Reason = "blocked", sre.Scrub(err.Error())
		return resp, nil
	case errors.Is(err, sreaction.ErrProposalState):
		resp.Verdict, resp.Reason = "not_requestable", sre.Scrub(err.Error())
		return resp, nil
	case err != nil:
		return RemediationResponse{}, backendErr(err)
	}
	resp.Verdict, resp.ApprovalQueueID = "queued_for_approval", v.QueueID
	resp.Reason = "a person approves the queued item; pg_sage rechecks the target " +
		"before it signals"
	return resp, nil
}

func (s *Service) requestCustodian(ctx context.Context, b Backend, inv sre.Investigation,
	p sre.ActionProposal, actor string) (RemediationResponse, error) {
	if p.Verdict == manualOnly || strings.TrimSpace(p.SQL) == "" {
		return RemediationResponse{Verdict: "not_requestable",
			Reason: "no custodian action covers this; it is an operator step"}, nil
	}
	out, err := b.SubmitCustodian(ctx, inv, p, actor)
	if err != nil {
		return RemediationResponse{}, backendErr(err)
	}
	return RemediationResponse{Verdict: verdictOf(out), Reason: sre.Scrub(out.Reason),
		Detail: sre.Scrub(out.Detail)}, nil
}

// verdictOf maps the gate's decision onto the contract's verdicts; an
// unknown decision is blocked (fail closed).
func verdictOf(out GateOutcome) string {
	switch {
	case out.Stale:
		return "stale"
	case out.HandedOff:
		return "queued_for_approval"
	case out.Executed:
		return "executed"
	}
	switch out.Decision {
	case executor.PolicyDecisionExecute:
		return "executed"
	case executor.PolicyDecisionQueueApproval:
		return "queued_for_approval"
	case executor.PolicyDecisionParked:
		return "parked"
	}
	return "blocked"
}

func (s *Service) audit(ctx context.Context, id Identity, database string, uid sre.UUID,
	remID string, resp RemediationResponse, reason string) error {
	_, err := s.store.Record(ctx, Record{Kind: KindRemediation, TokenID: id.TokenID,
		IdentityName: id.Name, Actor: id.Actor(), Transport: transportOf(id),
		Database: database, InvestigationID: string(uid), RemediationID: remID,
		Verdict: resp.Verdict, Reason: sre.Scrub(reason), Outbound: OutboundNone})
	if err != nil {
		return fmt.Errorf("%w: auditing the request: %v", ErrUnavailable, err)
	}
	return nil
}

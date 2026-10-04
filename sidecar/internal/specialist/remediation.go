package specialist

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Candidate remediations are pg_sage's own: the evidence-matched cancel
// proposals of the action service and the custodian actions the runway
// advisor attached to the diagnosis. A caller can only request one of
// these by id; it never supplies SQL, a target or a verdict.

// Remediation classes and id prefixes.
const (
	cancelPrefix    = "cancel_backend."
	custodianPrefix = "custodian."
	manualOnly      = "manual_only"
)

var remediationIDPattern = regexp.MustCompile(`^[a-z_]{1,32}\.[0-9a-f-]{8,36}$`)

// custodianID is the stable id of a custodian proposal: a hash of what it
// would run, so a changed proposal is a different remediation.
func custodianRemediationID(p sre.ActionProposal) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{p.Feature, p.SQL, p.Action,
		strings.Join(p.Targets, ",")}, "\x00")))
	return custodianPrefix + hex.EncodeToString(sum[:8])
}

// requestable reports whether remediations of inv may be requested: only
// those of a concluded investigation.
func requestableState(inv sre.Investigation) bool {
	return inv.State == sre.StateConcluded
}

func (m mapper) remediations(inv sre.Investigation,
	proposals []sreaction.ProposalView) []Remediation {
	out := []Remediation{}
	open := requestableState(inv)
	for _, p := range proposals {
		if p.InvestigationID == inv.ID {
			out = append(out, m.cancel(p, open))
		}
	}
	for _, p := range inv.Summary.Proposals {
		out = append(out, m.custodian(p, open))
	}
	return out
}

func (m mapper) cancel(p sreaction.ProposalView, open bool) Remediation {
	c := p.Contract
	r := Remediation{ID: cancelPrefix + string(p.ID), Class: string(p.Class),
		Title: "Cancel the root blocker's statement", Targets: []string{},
		State: string(p.State), RiskTier: c.BaseRiskTier,
		Gate: GatePreview{Verdict: p.Policy.Decision, Reason: m.red.Text(p.Policy.Reason),
			Preview: true},
		Requestable:   open && p.State == sreaction.ProposalProposed && p.Target != nil,
		RequiredScope: ScopePropose, EvidenceIDs: []string{},
		Rollback: Rollback{Class: c.RollbackClass,
			Reversible:  c.Reversibility == executor.ReversibilityReversible,
			Description: m.red.Text(c.Inverse)},
		PredictedEffect: PredictedEffect{Criteria: append([]string{}, c.PostConditions...),
			Metric: "waiting_sessions"}}
	if p.Detail != "" {
		r.Gate.Reason = m.red.Text(p.Detail)
	}
	for _, id := range p.EvidenceIDs {
		r.EvidenceIDs = append(r.EvidenceIDs, string(id))
	}
	if t := p.Target; t != nil {
		r.Targets = []string{fmt.Sprintf("pid %d", t.PID)}
		r.Title = fmt.Sprintf("Cancel the statement of blocking backend pid %d", t.PID)
		baseline, expected := float64(p.Baseline.Waiting), 0.0
		r.PredictedEffect.Quantified = true
		r.PredictedEffect.Baseline, r.PredictedEffect.Expected = &baseline, &expected
		r.PredictedEffect.Summary = fmt.Sprintf("cancelling pid %d's statement releases "+
			"its locks; the %d sessions waiting behind it are expected to proceed",
			t.PID, p.Baseline.Waiting)
	} else {
		r.PredictedEffect.Summary = "not derivable: " + m.red.Text(string(p.Reason))
	}
	return r
}

func (m mapper) custodian(p sre.ActionProposal, open bool) Remediation {
	r := Remediation{ID: custodianRemediationID(p), Class: p.Feature, Title: m.red.Text(p.Action),
		Targets: make([]string, 0, len(p.Targets)), State: "proposed",
		RiskTier: p.RiskTier, Gate: GatePreview{Verdict: p.Verdict,
			Reason: m.red.Text(p.Reason), Preview: true},
		Requestable:   open && p.Verdict != manualOnly && strings.TrimSpace(p.SQL) != "",
		RequiredScope: ScopePropose, EvidenceIDs: []string{},
		PredictedEffect: PredictedEffect{Criteria: []string{}, Summary: "the custodian's " +
			"action for this diagnosis; its effect is verified after it runs"},
		Rollback: Rollback{Class: "manual", Description: "an operator step; nothing runs"}}
	if p.Verdict == manualOnly {
		r.State = manualOnly
	}
	for _, t := range p.Targets {
		r.Targets = append(r.Targets, m.red.Name(t))
	}
	if c, ok := executor.CustodianContract(p.SQL); ok {
		r.PredictedEffect.Criteria = append([]string{}, c.SuccessCriteria...)
		r.Rollback = Rollback{Class: c.RollbackClass,
			Reversible:  c.RollbackClass == executor.ReversibilityReversible,
			Description: "rollback class " + c.RollbackClass}
		if r.RiskTier == "" {
			r.RiskTier = c.BaseRiskTier
		}
	}
	return r
}

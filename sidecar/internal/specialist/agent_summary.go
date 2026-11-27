package specialist

import (
	"context"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/whynot"
)

// summary_for_agent (contract revision 1.2.0, mcp-02) restates a result
// in the shape generalist SRE agents' quality gates check before they act
// on a specialist's answer: one root-cause sentence, the cited evidence
// with its numbers, and the specific actions pg_sage can take. When none
// can be requested it names the first failing link of the why-not chain
// (ux-08a). It is derived from the result alone: nothing new is read and
// nothing is invented.

// AgentSummary is the result for a generalist agent.
type AgentSummary struct {
	Status      string          `json:"status"`
	Reason      string          `json:"reason"`
	RootCause   string          `json:"root_cause"`
	Evidence    []AgentEvidence `json:"evidence"`
	Actions     []AgentAction   `json:"actions"`
	WhyNoAction *WhyNoAction    `json:"why_no_action"`
}

// AgentEvidence is one cited evidence row and the numbers it holds.
type AgentEvidence struct {
	EvidenceID string             `json:"evidence_id"`
	Numbers    map[string]float64 `json:"numbers"`
}

// AgentAction is one candidate remediation as an agent acts on it.
type AgentAction struct {
	RemediationID string   `json:"remediation_id"`
	Title         string   `json:"title"`
	RiskTier      string   `json:"risk_tier"`
	Requestable   bool     `json:"requestable"`
	GateVerdict   string   `json:"gate_verdict"`
	EvidenceIDs   []string `json:"evidence_ids"`
}

// WhyNoAction is the first link that holds pg_sage back.
type WhyNoAction struct {
	Link     string    `json:"link"`
	Sentence string    `json:"sentence"`
	Reason   string    `json:"reason"`
	Fix      *AgentFix `json:"fix"`
	NoFix    string    `json:"no_fix"`
}

// AgentFix is the why-not link's typed fix (grant_sql, setting, ui_link or
// wait_until), for a person to apply.
type AgentFix struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Note  string `json:"note"`
}

// agentSummary derives the summary from a mapped result.
func agentSummary(r Result, database string) AgentSummary {
	s := AgentSummary{Status: r.Outcome, Reason: r.OutcomeReason,
		RootCause: rootSentence(r.RootCause), Evidence: chainEvidence(r.CausalChain),
		Actions: agentActions(r.Remediations)}
	if r.Investigation.Terminal && !anyRequestable(r.Remediations) {
		s.WhyNoAction = whyNoAction(r, database)
	}
	return s
}

func rootSentence(rc *RootCause) string {
	if rc == nil {
		return ""
	}
	parts := []string{rc.Label}
	if rc.Mechanism != "" {
		parts = append(parts, ": "+rc.Mechanism)
	}
	if rc.Subject != "" {
		parts = append(parts, " ("+rc.Subject+")")
	}
	return strings.Join(parts, "")
}

// chainEvidence is every citation of the causal chain, root first, once.
func chainEvidence(chain []ChainLink) []AgentEvidence {
	out := []AgentEvidence{}
	seen := map[string]bool{}
	for _, link := range chain {
		for _, c := range link.Evidence {
			if seen[c.EvidenceID] {
				continue
			}
			seen[c.EvidenceID] = true
			numbers := make(map[string]float64, len(c.Numbers))
			for k, v := range c.Numbers {
				numbers[k] = v
			}
			out = append(out, AgentEvidence{EvidenceID: c.EvidenceID, Numbers: numbers})
		}
	}
	return out
}

func agentActions(rems []Remediation) []AgentAction {
	out := make([]AgentAction, 0, len(rems))
	for _, r := range rems {
		out = append(out, AgentAction{RemediationID: r.ID, Title: r.Title,
			RiskTier: r.RiskTier, Requestable: r.Requestable, GateVerdict: r.Gate.Verdict,
			EvidenceIDs: append([]string{}, r.EvidenceIDs...)})
	}
	return out
}

func anyRequestable(rems []Remediation) bool {
	for _, r := range rems {
		if r.Requestable {
			return true
		}
	}
	return false
}

// whyNoAction runs the why-not chain over what the result already says:
// the diagnosis is the finding, its remediations the candidates and their
// gate previews the verdicts. Without a root cause nothing can be acted
// on, so the finding link fails with the outcome's reason.
func whyNoAction(r Result, database string) *WhyNoAction {
	if r.RootCause == nil {
		return &WhyNoAction{Link: string(whynot.LinkFinding),
			Sentence: "No root cause was established (" + outcomeText(r) + ").",
			NoFix: "pg_sage acts only on a diagnosed root cause; open a new " +
				"investigation if the symptom persists"}
	}
	in := whynot.Inputs{Now: r.GeneratedAt, Database: database, LLMEnabled: true,
		Observed: observedOf(r), Findings: whynot.Findings{Open: 1},
		Candidates: len(r.Remediations)}
	for _, rem := range r.Remediations {
		in.Verdicts = append(in.Verdicts, whynot.Verdict{ActionType: rem.Class,
			Decision: rem.Gate.Verdict, Reason: rem.Gate.Reason})
	}
	for _, link := range whynot.Chain(context.Background(), in).Links {
		if link.FirstFail {
			return whyNotLink(link)
		}
	}
	return &WhyNoAction{Link: "unknown", Sentence: "No link of the why-not chain " +
		"failed from what this result shows.", NoFix: "ask an operator to open Why " +
		"not? for this database in the pg_sage dashboard"}
}

func outcomeText(r Result) string {
	if r.OutcomeReason == "" {
		return r.Outcome
	}
	return r.Outcome + ": " + r.OutcomeReason
}

// observedOf is when the investigation's probes last read the database;
// fresh only when one of them returned data.
func observedOf(r Result) whynot.Observed {
	o := whynot.Observed{LastSnapshot: r.Investigation.CreatedAt}
	for _, e := range r.Evidence {
		if e.ObservedAt != nil && e.ObservedAt.After(o.LastSnapshot) {
			o.LastSnapshot = *e.ObservedAt
		}
		if e.CapabilityState == "ok" {
			o.Freshness = "fresh"
		}
	}
	if o.LastSnapshot.IsZero() {
		o.LastSnapshot = time.Unix(0, 0).UTC()
	}
	return o
}

func whyNotLink(l whynot.Link) *WhyNoAction {
	w := &WhyNoAction{Link: string(l.Name), Sentence: l.Sentence, Reason: l.Reason,
		NoFix: l.NoFix}
	if l.Fix != nil {
		w.Fix = &AgentFix{Kind: string(l.Fix.Kind), Value: l.Fix.Value, Note: l.Fix.Note}
	}
	return w
}

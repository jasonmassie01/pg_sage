package specialist

import (
	"encoding/json"
	"net/url"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The tool-calling investigator's output in the contract (revision
// 1.1.0). The section is labelled model output; it never changes what the
// v1 fields mean. A contested or concluded root counts only where the
// family's earned root authority adopted it (then root_cause says
// source=model, authority=model_earned); otherwise it is advisory and the
// reason says why. Free text passes the contract's redaction.

// transcriptTool is the MCP tool serving the redacted transcript.
const transcriptTool = "specialist_investigation_transcript"

var verdicts = map[string]string{sre.ModelAgreed: VerdictAgree,
	sre.ModelConcluded: VerdictConclude, sre.ModelContested: VerdictContest,
	sre.ModelUnmodeled: VerdictUnmodeled, sre.ModelInconclusive: VerdictInconclusive}

// usableSteps are step statuses that are not missing evidence.
var usableSteps = map[string]bool{string(probes.StatusOK): true,
	string(probes.StatusEmpty): true, "final": true, "protocol": true}

// investigator maps the investigator's conclusion and run; nil when the
// investigator did not run.
func (m mapper) investigator(d sre.Detail) *InvestigatorResult {
	s := d.Investigation.Summary
	mc, run := s.ModelConclusion, s.Investigator
	if mc == nil && run == nil {
		return nil
	}
	out := &InvestigatorResult{Label: InvestigatorLabel, Claims: []InvestigatorClaim{},
		MissingEvidence: []MissingEvidence{}}
	out.Verdict, out.Adoption = m.verdict(mc, run, s.Family)
	if mc != nil {
		switch out.Verdict {
		case VerdictAgree, VerdictConclude, VerdictContest:
			out.Root, out.GraphRoot = mc.Root, mc.GraphRoot
		case VerdictUnmodeled:
			out.GraphRoot = mc.GraphRoot
			if c := mc.Cause; c != nil {
				out.Cause = &UnmodeledCause{Label: m.red.Text(c.Label),
					Mechanism: m.red.Text(c.Mechanism)}
			}
		}
	}
	if run == nil {
		return out
	}
	out.Claims = m.claims(s.Narrative)
	for _, n := range run.DroppedClaims {
		out.DroppedClaims += n
	}
	out.MissingEvidence = m.investigatorMissing(run)
	out.Run = InvestigatorRun{Plan: run.Plan, Protocol: run.Protocol,
		Stop: m.red.Text(run.Stop), ModelCalls: run.ModelCalls, ToolCalls: run.ToolCalls,
		Probes: run.Probes}
	out.Transcript = &TranscriptLink{Href: BasePath + "/databases/" +
		url.PathEscape(d.Database) + "/investigations/" + string(d.Investigation.ID) +
		"/transcript", MCPTool: transcriptTool, Schema: sre.TranscriptSchema, Redacted: true}
	return out
}

// verdict is the contract verdict and the root's adoption.
func (m mapper) verdict(mc *sre.ModelConclusion, run *sre.InvestigatorRun,
	family string) (string, RootAdoption) {
	if mc == nil {
		return VerdictNoAnswer, RootAdoption{Status: AdoptionNotApplicable,
			Reason: "the investigator stopped without an answer (" + m.red.Text(run.Stop) +
				"); the deterministic diagnosis stands"}
	}
	v, ok := verdicts[mc.Outcome]
	if !ok {
		return VerdictInconclusive, RootAdoption{Status: AdoptionNotApplicable,
			Reason: "the investigator's answer is not one this contract knows; it is not used"}
	}
	a := RootAdoption{Status: AdoptionNotApplicable, Reason: m.red.Text(mc.Reason)}
	switch v {
	case VerdictConclude, VerdictContest:
		a.Status, a.Family = AdoptionAdvisory, authorityFamily(mc.Root, family)
		if mc.Authority == sre.ContestAdopted {
			a.Status = AdoptionAdopted
		}
	case VerdictUnmodeled:
		a.Status = AdoptionAdvisory
	}
	return v, a
}

// authorityFamily is the family whose root authority decides a model
// root: the root node's own family, else the investigation's.
func authorityFamily(root, family string) string {
	if n, ok := causal.NodeByID(causal.NodeID(root)); ok {
		return string(n.Family)
	}
	return family
}

// claims are the investigator's verified claims, each citing its evidence
// with the numbers that evidence holds.
func (m mapper) claims(n *sre.Narrative) []InvestigatorClaim {
	out := []InvestigatorClaim{}
	if n == nil {
		return out
	}
	for _, c := range n.Claims {
		claim := InvestigatorClaim{Text: m.red.Text(c.Text),
			Evidence: make([]Citation, 0, len(c.EvidenceIDs))}
		for _, id := range c.EvidenceIDs {
			claim.Evidence = append(claim.Evidence, Citation{EvidenceID: string(id),
				Numbers: numbersOf(m.evidence[id])})
		}
		out = append(out, claim)
	}
	return out
}

// investigatorMissing names the reads the investigator asked for that
// returned no usable observation (failed, refused or not permitted).
func (m mapper) investigatorMissing(run *sre.InvestigatorRun) []MissingEvidence {
	out := []MissingEvidence{}
	seen := map[string]bool{}
	for _, st := range run.Steps {
		if st.Tool == "" || st.Tool == sre.ToolSubmit || usableSteps[st.Status] {
			continue
		}
		id := stepTarget(st)
		if seen[id] {
			continue
		}
		seen[id] = true
		reason := m.red.Text(st.Note)
		if reason == "" {
			reason = "the investigator's read returned no usable observation"
		}
		out = append(out, MissingEvidence{ProbeID: id, Status: st.Status, Reason: reason,
			Source: "investigator"})
	}
	return out
}

// stepTarget names what a step read: the catalog probe, tool:view for a
// pg_stat view, else the tool. Model-supplied names are used only when
// they are catalog names.
func stepTarget(st sre.InvestigatorStep) string {
	var a struct {
		Probe string `json:"probe"`
		View  string `json:"view"`
	}
	if len(st.Args) == 0 || json.Unmarshal(st.Args, &a) != nil {
		return st.Tool
	}
	switch st.Tool {
	case sre.ToolRunProbe:
		if _, ok := probes.Catalog().Spec(probes.ID(a.Probe)); ok {
			return a.Probe
		}
	case sre.ToolStatView:
		if probes.StatView(a.View) != "" {
			return st.Tool + ":" + a.View
		}
	}
	return st.Tool
}

package sre

import (
	"fmt"
	"strings"
)

// writeModel renders the model output of an investigation, when there is
// any, after the deterministic diagnosis and labeled as model-generated
// (CHECK-41): the ranking is not a confidence, every claim shows the
// evidence it cites.
func writeModel(b *strings.Builder, s Summary) {
	if r := s.ModelRanking; r != nil {
		fmt.Fprintf(b, "## Model ranking (model-generated; %s)\n\n", r.Basis)
		for i, n := range r.Nodes {
			fmt.Fprintf(b, "%d. %s\n", i+1, n)
		}
		b.WriteString("\n")
	}
	if n := s.Narrative; n != nil {
		b.WriteString("## Narrative (model-generated; each claim cites evidence)\n\n")
		for _, c := range n.Claims {
			ids := make([]string, 0, len(c.EvidenceIDs))
			for _, id := range c.EvidenceIDs {
				ids = append(ids, string(id))
			}
			fmt.Fprintf(b, "- %s [%s]\n", c.Text, strings.Join(ids, ", "))
		}
		b.WriteString("\n")
	}
	if p := s.ModelProbe; p != nil {
		b.WriteString("## Model-proposed probe\n\n")
		fmt.Fprintf(b, "- %s: %s [%s]\n\n", p.ProbeID, p.Rationale, p.EvidenceID)
	}
	writeInvestigator(b, s.Investigator, s.ModelConclusion)
}

// writeInvestigator renders the tool-calling investigator's transcript
// and conclusion (roadmap 2.1), labeled as model-generated.
func writeInvestigator(b *strings.Builder, run *InvestigatorRun, mc *ModelConclusion) {
	if run == nil {
		return
	}
	fmt.Fprintf(b, "## Investigator (model-generated; plan %s, %s protocol, stopped: %s)\n\n",
		run.Plan, run.Protocol, run.Stop)
	for _, st := range run.Steps {
		fmt.Fprintf(b, "- %d. %s %s", st.Seq, st.Tool, st.Status)
		if st.EvidenceID != "" {
			fmt.Fprintf(b, " [%s] sha256 %s", st.EvidenceID, st.Digest)
		}
		if st.Note != "" {
			fmt.Fprintf(b, " (%s)", st.Note)
		}
		b.WriteString("\n")
	}
	if mc != nil {
		fmt.Fprintf(b, "\nModel conclusion: outcome %s, root %q, authority %s: %s\n",
			mc.Outcome, mc.Root, mc.Authority, mc.Reason)
	}
	b.WriteString("\n")
}

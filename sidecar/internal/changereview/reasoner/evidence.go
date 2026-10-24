package reasoner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
)

// ruleHits adds the impact engine's rule hits on the statements the review
// accepted as model findings: they are deterministic for the model's SQL,
// but the SQL is the model's, so they stay warnings at most (the merge
// clamps them) and say so in their title.
func (s *session) ruleHits(out *changereview.Reasoning) {
	seen := map[string]bool{}
	for _, st := range out.Statements {
		rec := s.inspections[st.EvidenceID]
		if rec == nil {
			continue
		}
		origin := "translated"
		if rec.origin == changereview.OriginModelExtracted {
			origin = "extracted"
		}
		for _, f := range rec.ins.Findings {
			key := f.RuleID + "\x00" + f.Title
			if seen[key] {
				continue
			}
			seen[key] = true
			ids := append([]string{st.EvidenceID}, f.EvidenceIDs...)
			out.Findings = append(out.Findings, changereview.Finding{Kind: f.Kind,
				Severity: severityOf(f.Severity), Origin: changereview.OriginModel,
				Title: fmt.Sprintf("%s on model-%s SQL: %s", f.RuleID, origin, f.Title),
				Text:  f.Text, EvidenceIDs: dedupe(ids), Suggestion: f.Suggestion})
		}
	}
}

// citedEvidence is the evidence the reasoning's statements and findings
// cite that the run itself produced (the deterministic evidence is the
// review's already), in first-cited order.
func (s *session) citedEvidence(out changereview.Reasoning) []changereview.Evidence {
	var res []changereview.Evidence
	seen := map[string]bool{}
	add := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		if e, ok := s.lookup(id); ok {
			res = append(res, e)
		}
	}
	for _, st := range out.Statements {
		add(st.EvidenceID)
	}
	for _, f := range out.Findings {
		for _, id := range f.EvidenceIDs {
			add(id)
		}
	}
	return res
}

// lookup finds evidence a tool returned or an inspection holds.
func (s *session) lookup(id string) (changereview.Evidence, bool) {
	if e, ok := s.evidence[id]; ok {
		return e, true
	}
	for _, rec := range s.inspections {
		if e, ok := rec.byID[id]; ok && !strings.HasPrefix(id, "inspect:") {
			return e, true
		}
	}
	return changereview.Evidence{}, false
}

func dedupe(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// redactTranscript is a copy of the run's transcript with every string
// redacted (tool arguments are the model's, and may quote the diff).
func redactTranscript(t agentloop.Transcript) *agentloop.Transcript {
	raw, err := json.Marshal(t)
	if err != nil {
		return &agentloop.Transcript{Stop: t.Stop, StopDetail: "transcript not encodable"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return &agentloop.Transcript{Stop: t.Stop, StopDetail: "transcript not decodable"}
	}
	clean, err := json.Marshal(redactTree(tree))
	if err != nil {
		return &agentloop.Transcript{Stop: t.Stop, StopDetail: "transcript not encodable"}
	}
	var out agentloop.Transcript
	if err := json.Unmarshal(clean, &out); err != nil {
		return &agentloop.Transcript{Stop: t.Stop, StopDetail: "transcript not decodable"}
	}
	return &out
}

func redactTree(v any) any {
	switch t := v.(type) {
	case string:
		return diffscan.Redact(t)
	case []any:
		for i := range t {
			t[i] = redactTree(t[i])
		}
	case map[string]any:
		for k, x := range t {
			t[k] = redactTree(x)
		}
	}
	return v
}

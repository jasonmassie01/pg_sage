package sre

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Persisted model output (Sage SRE M3). It lives in the summary beside
// the deterministic diagnosis, is labeled wherever it is shown, and is
// validated again before any store I/O: the store itself refuses a
// ranking that would put another root above a concluded root cause.

// Labels the surfaces show with model output.
const (
	ModelRankingLabel = "model ranking"
	ModelRankingBasis = "model-generated order of the causal graph's open hypotheses; " +
		"not a confidence and not part of the deterministic diagnosis"
	NarrativeLabel  = "model-generated narrative"
	ModelProbeLabel = "model-proposed probe"
)

// maxRationaleRunes bounds a next probe's one-line rationale.
const maxRationaleRunes = 300

// ModelRanking is the model's order of the graph's open hypotheses.
type ModelRanking struct {
	Label string   `json:"label"`
	Basis string   `json:"basis"`
	Nodes []string `json:"nodes"`
}

// NarrativeClaim is one verified model claim and the evidence it cites.
type NarrativeClaim struct {
	Text        string `json:"text"`
	EvidenceIDs []UUID `json:"evidence_ids"`
}

// Narrative is the model's verified, cited narration.
type Narrative struct {
	Label  string           `json:"label"`
	Claims []NarrativeClaim `json:"claims"`
}

// ModelProbe is the one catalog probe the model asked for, its typed
// arguments, its rationale and the evidence it produced.
type ModelProbe struct {
	Label         string     `json:"label"`
	ProbeID       string     `json:"probe_id"`
	PID           int32      `json:"pid,omitempty"`
	BackendStart  *time.Time `json:"backend_start,omitempty"`
	WindowSeconds int64      `json:"window_seconds,omitempty"`
	Rationale     string     `json:"rationale"`
	EvidenceID    UUID       `json:"evidence_id,omitempty"`
}

func (p ModelProbe) args() probes.Args {
	a := probes.Args{PID: p.PID, Window: time.Duration(p.WindowSeconds) * time.Second}
	if p.BackendStart != nil {
		a.BackendStart = *p.BackendStart
	}
	return a
}

func (s Summary) validateModel(hs []HypothesisRecord, concluded bool) error {
	if err := s.ModelRanking.validate(hs, s.Root, concluded); err != nil {
		return err
	}
	if err := s.Narrative.validate(); err != nil {
		return err
	}
	if err := s.ModelContest.validate(hs, s.Root, concluded); err != nil {
		return err
	}
	if err := s.ModelConclusion.validate(hs, s.Root, concluded); err != nil {
		return err
	}
	if err := s.Investigator.validate(); err != nil {
		return err
	}
	return s.ModelProbe.validate()
}

func (r *ModelRanking) validate(hs []HypothesisRecord, root string, concluded bool) error {
	if r == nil {
		return nil
	}
	if r.Label != ModelRankingLabel || r.Basis != ModelRankingBasis {
		return fmt.Errorf("%w: model ranking must carry its label", ErrInvalidRequest)
	}
	open := map[string]bool{}
	for _, h := range hs {
		if h.Status != HypothesisRuledOut {
			open[h.Node] = true
		}
	}
	if len(r.Nodes) == 0 || len(r.Nodes) != len(open) {
		return fmt.Errorf("%w: model ranking orders %d of %d open hypotheses",
			ErrInvalidRequest, len(r.Nodes), len(open))
	}
	seen := map[string]bool{}
	for _, n := range r.Nodes {
		if !open[n] || seen[n] {
			return fmt.Errorf("%w: model ranking node %q is not an open hypothesis",
				ErrInvalidRequest, n)
		}
		seen[n] = true
	}
	if concluded && r.Nodes[0] != root {
		return fmt.Errorf("%w: model ranking puts %q above the root cause %q",
			ErrInvalidRequest, r.Nodes[0], root)
	}
	return nil
}

func (n *Narrative) validate() error {
	if n == nil {
		return nil
	}
	if n.Label != NarrativeLabel || len(n.Claims) == 0 || len(n.Claims) > MaxClaims {
		return fmt.Errorf("%w: narrative needs its label and 1-%d claims",
			ErrInvalidRequest, MaxClaims)
	}
	for i, c := range n.Claims {
		if err := checkText("claim", c.Text, true, MaxClaimRunes); err != nil {
			return fmt.Errorf("claim %d: %w", i+1, err)
		}
		if len(c.EvidenceIDs) == 0 || len(c.EvidenceIDs) > maxFacts {
			return fmt.Errorf("%w: claim %d cites %d evidence items", ErrInvalidRequest,
				i+1, len(c.EvidenceIDs))
		}
		for _, id := range c.EvidenceIDs {
			if _, err := ParseUUID(string(id)); err != nil {
				return fmt.Errorf("claim %d: %w", i+1, err)
			}
		}
	}
	return nil
}

func (p *ModelProbe) validate() error {
	if p == nil {
		return nil
	}
	if p.Label != ModelProbeLabel {
		return fmt.Errorf("%w: model probe must carry its label", ErrInvalidRequest)
	}
	if err := probes.Catalog().CheckArgs(probes.ID(p.ProbeID), p.args()); err != nil {
		return fmt.Errorf("%w: model probe: %v", ErrInvalidRequest, err)
	}
	if err := checkText("rationale", p.Rationale, true, maxRationaleRunes); err != nil {
		return err
	}
	if p.EvidenceID != "" {
		if _, err := ParseUUID(string(p.EvidenceID)); err != nil {
			return err
		}
	}
	return nil
}

// modelRefs lists the evidence ids the model output cites.
func (s Summary) modelRefs() []UUID {
	var out []UUID
	if s.Narrative != nil {
		for _, c := range s.Narrative.Claims {
			out = append(out, c.EvidenceIDs...)
		}
	}
	if s.ModelProbe != nil && s.ModelProbe.EvidenceID != "" {
		out = append(out, s.ModelProbe.EvidenceID)
	}
	return append(out, s.Investigator.evidenceRefs()...)
}

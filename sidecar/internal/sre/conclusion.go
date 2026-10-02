package sre

import (
	"encoding/json"
	"fmt"
	"math"
)

// Conclusion limits.
const (
	MaxHypotheses  = 32
	maxFacts       = 16
	maxFactRunes   = 512
	maxSummaryJSON = 64 << 10
)

// validate checks a conclusion before any store I/O: a final state, one
// root exactly when concluded, bounded well-formed hypotheses, and facts
// that each cite one evidence id. Whether the ids are in scope is
// checked in the store, against the investigation's own evidence.
func (c Conclusion) validate() error {
	switch c.State {
	case StateConcluded, StateInconclusive:
	case StateFailed:
		if err := checkText("failure code", c.FailureCode, true, 64); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: %q is not a final state", ErrInvalidRequest, c.State)
	}
	if len(c.Hypotheses) > MaxHypotheses {
		return fmt.Errorf("%w: %d hypotheses over %d", ErrInvalidRequest,
			len(c.Hypotheses), MaxHypotheses)
	}
	roots := 0
	for i, h := range c.Hypotheses {
		if err := h.validate(); err != nil {
			return fmt.Errorf("hypothesis %d: %w", i+1, err)
		}
		if h.Status == HypothesisRoot {
			roots++
		}
	}
	if want := c.State == StateConcluded; (roots == 1) != want || roots > 1 {
		return fmt.Errorf("%w: %s with %d root causes", ErrInvalidRequest, c.State, roots)
	}
	if err := validateFacts(c.Summary.Observed); err != nil {
		return err
	}
	if err := c.Summary.validateModel(c.Hypotheses, c.State == StateConcluded); err != nil {
		return err
	}
	if err := c.Summary.CustomerImpact.validate(); err != nil {
		return err
	}
	if err := validateProposals(c.Summary.Proposals); err != nil {
		return err
	}
	raw, err := json.Marshal(c.Summary)
	if err != nil || len(raw) > maxSummaryJSON {
		return fmt.Errorf("%w: summary too large or unencodable", ErrInvalidRequest)
	}
	return nil
}

func (h HypothesisRecord) validate() error {
	if !h.Status.valid() {
		return fmt.Errorf("%w: status %q", ErrInvalidRequest, h.Status)
	}
	if math.IsNaN(h.Confidence) || h.Confidence < 0 || h.Confidence > 1 {
		return fmt.Errorf("%w: confidence %v outside [0, 1]", ErrInvalidRequest,
			h.Confidence)
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{{"graph version", h.GraphVersion, 32}, {"family", h.Family, 64},
		{"node", h.Node, 64}, {"label", h.Label, 256},
		{"refutation probe", h.RefutationProbe, 64}} {
		if err := checkText(f.name, f.value, true, f.max); err != nil {
			return err
		}
	}
	if h.Status == HypothesisRuledOut && len(h.Contradict) == 0 {
		return fmt.Errorf("%w: ruled out without contradicting evidence", ErrInvalidRequest)
	}
	if len(h.Support)+len(h.Contradict) > maxFacts {
		return fmt.Errorf("%w: more than %d facts", ErrInvalidRequest, maxFacts)
	}
	return validateFacts(append(append([]Fact(nil), h.Support...), h.Contradict...))
}

func validateFacts(facts []Fact) error {
	for _, f := range facts {
		if _, err := ParseUUID(string(f.EvidenceID)); err != nil {
			return fmt.Errorf("fact %q: %w", f.Text, err)
		}
		if err := checkText("fact", f.Text, true, maxFactRunes); err != nil {
			return err
		}
	}
	return nil
}

// evidenceRefs lists every evidence id a conclusion cites.
func (c Conclusion) evidenceRefs() []UUID {
	var out []UUID
	for _, h := range c.Hypotheses {
		for _, f := range append(append([]Fact(nil), h.Support...), h.Contradict...) {
			out = append(out, f.EvidenceID)
		}
	}
	for _, f := range c.Summary.Observed {
		out = append(out, f.EvidenceID)
	}
	if ci := c.Summary.CustomerImpact; ci != nil && ci.EvidenceID != "" {
		out = append(out, ci.EvidenceID)
	}
	return append(out, c.Summary.modelRefs()...)
}

func factIDs(facts []Fact) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, string(f.EvidenceID))
	}
	return out
}

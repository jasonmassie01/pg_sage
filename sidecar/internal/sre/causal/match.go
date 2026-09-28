package causal

import (
	"fmt"
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Status is a hypothesis' place in a diagnosis.
type Status string

// Hypothesis statuses.
const (
	StatusRoot         Status = "root_cause"
	StatusContributing Status = "contributing"
	StatusAlternative  Status = "unproven"
	StatusRuledOut     Status = "ruled_out"
)

// Fact is one observation bound to the evidence it came from. Its
// numbers are rendered with probes.FormatValue, exactly as the evidence.
type Fact struct {
	EvidenceID string
	Text       string
}

// Hypothesis is one scored mechanism for one subject (a pid, a query).
type Hypothesis struct {
	Node            NodeID
	Label           string
	Mechanism       string
	Subject         string
	Confidence      float64
	Status          Status
	Support         []Fact
	Contradict      []Fact
	RefutationProbe string
}

// Missing names evidence the diagnosis could not use.
type Missing struct {
	ProbeID probes.ID
	Status  probes.Status // empty when the probe was not collected
	Reason  string
}

// Observation is a probe result with the evidence id it is cited by.
type Observation struct {
	EvidenceID string
	Result     probes.Result
}

// Diagnosis separates the root cause from contributing factors, unproven
// alternatives and ruled-out hypotheses (AI-SRE-SPEC §2.4). Inconclusive
// (no root) is a first-class outcome with a reason.
type Diagnosis struct {
	Family       Family
	GraphVersion string
	Subject      string
	Root         *Hypothesis
	Contributing []Hypothesis
	Alternatives []Hypothesis
	RuledOut     []Hypothesis
	Missing      []Missing
	Conclusive   bool
	Reason       string
	// Ratio is the latency ratio of a plan-family diagnosis (0 otherwise).
	Ratio float64
}

func newHypothesis(id NodeID, subject string) Hypothesis {
	n, _ := NodeByID(id)
	return Hypothesis{Node: id, Label: n.Label, Mechanism: n.Mechanism,
		Subject: subject, RefutationProbe: n.Refutation}
}

// add records supporting evidence worth w confidence.
func (h *Hypothesis) add(w float64, evidenceID, text string) {
	h.Confidence = math.Round((h.Confidence+w)*100) / 100
	h.Support = append(h.Support, Fact{EvidenceID: evidenceID, Text: text})
}

// contradict records a refuting observation; the hypothesis is ruled out.
func (h *Hypothesis) contradict(evidenceID, text string) {
	h.Contradict = append(h.Contradict, Fact{EvidenceID: evidenceID, Text: text})
}

// rank orders scored hypotheses into a diagnosis: contradicted ones are
// ruled out; supported ones (confidence >= SupportThreshold) that
// amplify another supported one are contributing; the most confident
// remaining supported one is the root (graph order breaks ties).
func rank(family Family, hs []Hypothesis) Diagnosis {
	d := Diagnosis{Family: family, GraphVersion: GraphVersion}
	sorted := append([]Hypothesis(nil), hs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return graphIndex(sorted[i].Node) < graphIndex(sorted[j].Node)
	})
	supported := map[NodeID]bool{}
	for _, h := range sorted {
		if len(h.Contradict) == 0 && h.Confidence >= SupportThreshold {
			supported[h.Node] = true
		}
	}
	rootIdx := -1
	for i, h := range sorted {
		if supported[h.Node] && !amplifiesSupported(h.Node, supported) &&
			(rootIdx < 0 || h.Confidence > sorted[rootIdx].Confidence) {
			rootIdx = i
		}
	}
	for i, h := range sorted {
		d.place(h, i == rootIdx, supported)
	}
	d.Conclusive = d.Root != nil
	if !d.Conclusive {
		d.Reason = fmt.Sprintf("no hypothesis reached confidence %.2f",
			SupportThreshold)
	}
	return d
}

func (d *Diagnosis) place(h Hypothesis, root bool, supported map[NodeID]bool) {
	switch {
	case root:
		h.Status = StatusRoot
		d.Root = &h
		d.Subject = h.Subject
	case len(h.Contradict) > 0:
		h.Status = StatusRuledOut
		d.RuledOut = append(d.RuledOut, h)
	case supported[h.Node] && amplifiesSupported(h.Node, supported):
		h.Status = StatusContributing
		d.Contributing = append(d.Contributing, h)
	default:
		h.Status = StatusAlternative
		d.Alternatives = append(d.Alternatives, h)
	}
}

func amplifiesSupported(id NodeID, supported map[NodeID]bool) bool {
	n, _ := NodeByID(id)
	for _, target := range n.Amplifies {
		if supported[target] {
			return true
		}
	}
	return false
}

func find(obs []Observation, id probes.ID) (Observation, bool) {
	for _, o := range obs {
		if o.Result.ProbeID == id {
			return o, true
		}
	}
	return Observation{}, false
}

// missingFor lists the needed probes that were not collected or did not
// produce an observation, in the order given.
func missingFor(obs []Observation, ids ...probes.ID) []Missing {
	var out []Missing
	for _, id := range ids {
		o, ok := find(obs, id)
		switch {
		case !ok:
			out = append(out, Missing{ProbeID: id, Reason: "not_collected"})
		case !o.Result.Status.Usable():
			out = append(out, Missing{ProbeID: id, Status: o.Result.Status,
				Reason: o.Result.Reason})
		}
	}
	return out
}

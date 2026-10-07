package specialist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre"
)

// maxNumbers bounds the numbers one citation carries.
const maxNumbers = 16

// diagnosis fills the root cause, causal chain, alternatives, ruled-out
// hypotheses and model contest from the latest diagnosis revision.
func (m mapper) diagnosis(r *Result, d sre.Detail) {
	hs := sre.LatestRevision(d.Hypotheses)
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].Ordinal < hs[j].Ordinal })
	contest := d.Investigation.Summary.ModelContest
	mc := d.Investigation.Summary.ModelConclusion
	for _, h := range hs {
		switch h.Status {
		case sre.HypothesisRoot:
			r.CausalChain = append([]ChainLink{m.link(h, "root_cause")}, r.CausalChain...)
			r.RootCause = m.root(h, contest, mc)
		case sre.HypothesisContributing:
			r.CausalChain = append(r.CausalChain, m.link(h, "contributing"))
		case sre.HypothesisUnproven:
			r.Alternatives = append(r.Alternatives, m.hypothesis(h))
		case sre.HypothesisRuledOut:
			r.RuledOut = append(r.RuledOut, m.hypothesis(h))
		}
	}
	for i := range r.CausalChain {
		r.CausalChain[i].Ordinal = i + 1
	}
	if contest != nil {
		r.ModelContest = &ModelContest{GraphRoot: contest.GraphRoot,
			ModelRoot: contest.ModelRoot, Authority: contest.Authority,
			Reason: m.red.Text(contest.Reason)}
	}
}

// root names the root and its authority: a model root adopted under the
// family's earned root authority (roadmap 2.4) is "model_earned", whether
// it contested a conclusive graph root or concluded an inconclusive graph
// (the investigator, roadmap 2.1); otherwise the causal graph named it.
func (m mapper) root(h sre.HypothesisRecord, contest *sre.ModelContest,
	mc *sre.ModelConclusion) *RootCause {
	rc := &RootCause{Node: h.Node, Label: h.Label, Family: h.Family,
		Subject: m.red.Text(h.Subject), Mechanism: m.red.Text(h.Mechanism), Source: "graph",
		Authority: "deterministic", EvidenceIDs: []string{}}
	adopted := contest != nil && contest.Authority == sre.ContestAdopted &&
		contest.ModelRoot == h.Node
	adopted = adopted || (mc != nil && mc.Authority == sre.ContestAdopted && mc.Root == h.Node)
	if adopted {
		rc.Source, rc.Authority = "model", "model_earned"
	}
	for _, f := range h.Support {
		rc.EvidenceIDs = append(rc.EvidenceIDs, string(f.EvidenceID))
	}
	return rc
}

func (m mapper) link(h sre.HypothesisRecord, role string) ChainLink {
	return ChainLink{Role: role, Node: h.Node, Label: h.Label,
		Mechanism: m.red.Text(h.Mechanism), Subject: m.red.Text(h.Subject),
		Evidence: m.citations(h.Support)}
}

func (m mapper) hypothesis(h sre.HypothesisRecord) Hypothesis {
	return Hypothesis{Node: h.Node, Label: h.Label, Status: string(h.Status),
		Support: m.citations(h.Support), Contradict: m.citations(h.Contradict)}
}

func (m mapper) citations(facts []sre.Fact) []Citation {
	out := make([]Citation, 0, len(facts))
	for _, f := range facts {
		out = append(out, Citation{EvidenceID: string(f.EvidenceID),
			Text: m.red.Text(f.Text), Numbers: numbersOf(m.evidence[f.EvidenceID])})
	}
	return out
}

func payloadIndex(ev []sre.EvidenceView) map[sre.UUID][]byte {
	out := make(map[sre.UUID][]byte, len(ev))
	for _, e := range ev {
		out[e.ID] = e.Payload
	}
	return out
}

// numbersOf reads the numeric columns of an evidence payload's rows: one
// row's values, or for several rows the row count and max_<column>.
func numbersOf(payload []byte) map[string]float64 {
	out := map[string]float64{}
	var p struct {
		Rows []map[string]any `json:"rows"`
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if len(payload) == 0 || dec.Decode(&p) != nil || len(p.Rows) == 0 {
		return out
	}
	prefix := ""
	if len(p.Rows) > 1 {
		prefix = "max_"
		out["rows"] = float64(len(p.Rows))
	}
	for _, row := range p.Rows {
		for col, v := range row {
			n, ok := v.(json.Number)
			if !ok {
				continue
			}
			f, err := n.Float64()
			if err != nil {
				continue
			}
			if cur, seen := out[prefix+col]; !seen || f > cur {
				out[prefix+col] = f
			}
		}
	}
	return boundNumbers(out)
}

func boundNumbers(in map[string]float64) map[string]float64 {
	if len(in) <= maxNumbers {
		return in
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]float64, maxNumbers)
	for _, k := range keys[:maxNumbers] {
		out[k] = in[k]
	}
	return out
}

const scoreBasis = "causal-graph support score of the root hypothesis; not a " +
	"probability"

// confidenceOf is the root's graph score with the family's bench
// calibration when one exists; without a root there is no score.
func confidenceOf(root *RootCause, d sre.Detail, cal *CalibratedRate) Confidence {
	c := Confidence{ScoreBasis: scoreBasis, Calibration: "uncalibrated"}
	if root == nil {
		return c
	}
	for _, h := range sre.LatestRevision(d.Hypotheses) {
		if h.Status == sre.HypothesisRoot {
			score := h.Confidence
			c.Score = &score
		}
	}
	if cal != nil && cal.N > 0 {
		c.Calibration, c.Calibrated = "bench_top1", cal
	}
	return c
}

// missing merges the diagnosis' missing evidence, unavailable probes, a
// missing probe plan and a caller window the probes could not see.
func (m mapper) missing(d sre.Detail, rec *Record) []MissingEvidence {
	out := []MissingEvidence{}
	seen := map[string]bool{}
	add := func(e MissingEvidence) {
		if !seen[e.ProbeID] {
			seen[e.ProbeID] = true
			out = append(out, e)
		}
	}
	inv := d.Investigation
	for _, mi := range inv.Summary.Missing {
		add(MissingEvidence{ProbeID: mi.ProbeID, Status: mi.Status,
			Reason: m.red.Text(mi.Reason), Source: "diagnosis"})
	}
	for _, e := range d.Evidence {
		if e.CapabilityState != "" && e.CapabilityState != "ok" {
			reason := e.ReasonCode
			if reason == "" {
				reason = "the probe returned no usable observation"
			}
			add(MissingEvidence{ProbeID: e.ProbeID, Status: e.CapabilityState,
				Reason: m.red.Text(reason), Source: "probe"})
		}
	}
	if inv.State == sre.StateFailed && inv.FailureCode == "no_probe_plan" {
		add(MissingEvidence{ProbeID: "probe_plan", Status: "unavailable", Source: "plan",
			Reason: "this investigation kind has no probe plan yet; open the " +
				"investigation with a family (lock_blocking, connection_pressure, ...)"})
	}
	if w := windowGap(rec, inv); w != "" {
		add(MissingEvidence{ProbeID: "history", Status: "unavailable", Source: "window",
			Reason: w})
	}
	return out
}

// windowGap explains a caller window that ended before the probes ran:
// probes observe current state, so what ended earlier may not be visible.
func windowGap(rec *Record, inv sre.Investigation) string {
	if rec == nil || rec.Window == nil || rec.Window.End == nil || inv.CreatedAt.IsZero() {
		return ""
	}
	gap := inv.CreatedAt.Sub(*rec.Window.End)
	if gap < windowGapThreshold {
		return ""
	}
	return fmt.Sprintf("the caller's window ended %d minutes before the investigation "+
		"started; pg_sage probes observe the current state, so conditions that ended "+
		"earlier may not be visible", int(gap.Minutes()))
}

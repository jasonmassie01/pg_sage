package rca

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Narration on the probe catalog (Sage SRE M1). Before narrating an
// incident the engine runs its family's catalog probes (fixed, read-only,
// capped SQL; never chosen by the model) and, for lock contention, the
// causal graph. Probe results become evidence P1..Pn and hypotheses
// H1..Hn, next to the incident's own evidence E1..En. With the LLM off
// the deterministic summary carries the leading hypothesis.

// ProbeRunner runs catalog probes for this engine's database
// (*probes.Runner in production).
type ProbeRunner interface {
	Run(ctx context.Context, id probes.ID, args probes.Args) probes.Result
}

// probeGatherTimeout bounds all probes of one incident.
const probeGatherTimeout = 5 * time.Second

// probeTextRows bounds the rows of one probe result shown as evidence.
const probeTextRows = 10

// WithProbes attaches the catalog probe runner used for narration
// evidence. nil keeps narration on the incident's own evidence.
func (e *Engine) WithProbes(r ProbeRunner) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probes = r
}

// incidentEvidence is what the probes and the causal graph add to an
// incident's own evidence.
type incidentEvidence struct {
	observations []causal.Observation
	diagnosis    *causal.Diagnosis
	hypotheses   []causal.Hypothesis // in H1..Hn order
}

// gatherEvidence runs the incident's catalog probes and the causal
// graph. It never holds e.mu during I/O.
func (e *Engine) gatherEvidence(ctx context.Context, inc Incident) incidentEvidence {
	e.mu.Lock()
	runner := e.probes
	e.mu.Unlock()
	var ev incidentEvidence
	if runner == nil {
		return ev
	}
	ctx, cancel := context.WithTimeout(ctx, probeGatherTimeout)
	defer cancel()
	for i, id := range incidentProbes(inc.SignalIDs) {
		ev.observations = append(ev.observations, causal.Observation{
			EvidenceID: fmt.Sprintf("P%d", i+1), Result: runner.Run(ctx, id, probes.Args{})})
	}
	if hasSignal(inc, "lock_contention") {
		d := causal.DiagnoseLock(ev.observations, causalBlockers(inc))
		ev.diagnosis = &d
		ev.hypotheses = orderedHypotheses(d)
	}
	return ev
}

// incidentProbes is the union of the signals' catalog probes, capped.
func incidentProbes(signals []string) []probes.ID {
	seen := map[probes.ID]bool{}
	var out []probes.ID
	for _, sig := range signals {
		for _, id := range probes.ForSignal(sig) {
			if !seen[id] && len(out) < probes.MaxProbesPerSignal {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func hasSignal(inc Incident, id string) bool {
	for _, s := range inc.SignalIDs {
		if s == id {
			return true
		}
	}
	return false
}

// causalBlockers maps the incident's recorded root blockers (M0) to
// causal-graph input, cited by their E ids.
func causalBlockers(inc Incident) []causal.BlockerEvidence {
	var out []causal.BlockerEvidence
	for i, l := range inc.CausalChain {
		if b := l.Blocker; b != nil {
			out = append(out, causal.BlockerEvidence{EvidenceID: fmt.Sprintf("E%d", i+1),
				PID: b.PID, State: b.State, TotalBlocked: b.TotalBlocked,
				ChainDepth: b.ChainDepth})
		}
	}
	return out
}

func orderedHypotheses(d causal.Diagnosis) []causal.Hypothesis {
	var out []causal.Hypothesis
	if d.Root != nil {
		out = append(out, *d.Root)
	}
	out = append(out, d.Contributing...)
	out = append(out, d.Alternatives...)
	return append(out, d.RuledOut...)
}

// hypothesisText renders one hypothesis as evidence: every number in it
// comes from the facts (probe values) or the computed confidence.
func hypothesisText(h causal.Hypothesis) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s, confidence %s) for %s: %s", h.Label, h.Status,
		probes.FormatValue(h.Confidence), h.Subject, h.Mechanism)
	for _, f := range h.Support {
		fmt.Fprintf(&b, " Supported by %s: %s.", f.EvidenceID, f.Text)
	}
	for _, f := range h.Contradict {
		fmt.Fprintf(&b, " Ruled out by %s: %s.", f.EvidenceID, f.Text)
	}
	fmt.Fprintf(&b, " Refutation probe: %s.", h.RefutationProbe)
	return b.String()
}

// diagnosisSummary is the deterministic one-line diagnosis for the
// notification: the leading hypothesis, contributing factors and the
// evidence that could not be collected.
func diagnosisSummary(ev incidentEvidence) string {
	d := ev.diagnosis
	if d == nil {
		return ""
	}
	var parts []string
	if d.Root != nil {
		parts = append(parts, fmt.Sprintf("Likely (H1, confidence %s): %s, %s",
			probes.FormatValue(d.Root.Confidence), d.Root.Label, d.Root.Subject))
		for i, h := range d.Contributing {
			parts = append(parts, fmt.Sprintf("contributing (H%d): %s", i+2, h.Label))
		}
	} else {
		parts = append(parts, "Cause inconclusive: "+d.Reason)
	}
	var missing []string
	for _, m := range d.Missing {
		if m.Status != "" {
			missing = append(missing, fmt.Sprintf("%s (%s)", m.ProbeID, m.Status))
		}
	}
	if len(missing) > 0 {
		parts = append(parts, "Missing: "+strings.Join(missing, ", "))
	}
	return strings.Join(parts, "; ")
}

// deterministicNarration is DeterministicNarration plus the diagnosis.
func deterministicNarration(inc Incident, ev incidentEvidence) Narration {
	n := DeterministicNarration(inc)
	if s := diagnosisSummary(ev); s != "" {
		n.Text += "; " + s
	}
	return n
}

package causal

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// pg_sage's own actions are change events (AI-SRE-SPEC principle 10):
// every diagnosis carries "did pg_sage cause this?" as a hypothesis. It
// is supported by the actions in the window, ruled out by an observed
// absence of actions, and unproven when the history cannot be read. It
// is a question for the operator, never a root cause on its own: its
// confidence stays below the support threshold.

// Self-action scoring.
const (
	selfActionWeight   = 0.1
	maxSelfActionFacts = 3
)

// WithSelfActions adds the self-action hypothesis to a diagnosis without
// changing its root cause.
func WithSelfActions(d Diagnosis, obs []Observation) Diagnosis {
	h := newHypothesis(SageOwnAction, "pg_sage")
	h.Status = StatusAlternative
	o, ok := find(obs, probes.SageActions)
	var actions []probes.SageAction
	var err error
	if ok {
		actions, err = probes.SageActionRows(o.Result)
	}
	switch {
	case !ok:
		d.Missing = append(d.Missing, Missing{ProbeID: probes.SageActions,
			Reason: "not_collected"})
	case err != nil:
		d.Missing = append(d.Missing, Missing{ProbeID: probes.SageActions,
			Status: o.Result.Status, Reason: unavailableReason(o, err)})
	case len(actions) == 0:
		h.contradict(o.EvidenceID, "pg_sage recorded no action in the window")
		h.Status = StatusRuledOut
	default:
		for i, a := range actions {
			if i == maxSelfActionFacts {
				break
			}
			h.add(selfActionWeight, o.EvidenceID, fmt.Sprintf("pg_sage ran %s "+
				"(action %d, %s) %s s before the probe", a.ActionType, a.ID, a.Outcome,
				probes.FormatValue(a.AgeS)))
		}
	}
	if h.Status == StatusRuledOut {
		d.RuledOut = append(d.RuledOut, h)
	} else {
		d.Alternatives = append(d.Alternatives, h)
	}
	return d
}

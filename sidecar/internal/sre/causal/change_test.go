package causal

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CHECK-38 (AI-SRE-SPEC principle 10): pg_sage's own actions are change
// events, and "did we cause this?" is always a hypothesis of every
// diagnosis: supported evidence when pg_sage acted in the window, ruled
// out with evidence when it did not, unproven when the history is
// unreadable. It is never the root on its own.

func actionsObs(id string, rows ...probes.Row) Observation {
	return obs(id, probes.SageActions, rows...)
}

func TestSelfActions_ActionInWindowIsAHypothesis(t *testing.T) {
	base := DiagnoseConnections([]Observation{connObs("E1", t0, 30,
		group{app: "api", state: "idle", n: 28})})
	d := WithSelfActions(base, []Observation{actionsObs("E9",
		probes.Row{"id": int64(42), "action_type": "create_index", "outcome": "success",
			"executed_at": t0.Add(-10 * time.Minute), "age_s": 600.0})})
	h := requireStatus(t, d, SageOwnAction, StatusAlternative)
	if len(h.Support) != 1 || h.Support[0].EvidenceID != "E9" ||
		!strings.Contains(h.Support[0].Text, "create_index") ||
		!strings.Contains(h.Support[0].Text, "42") {
		t.Fatalf("self-action hypothesis = %+v", h)
	}
	if d.Root == nil || d.Root.Node != PoolFanOut {
		t.Fatalf("the family's root changed: %+v", d.Root)
	}
}

func TestSelfActions_NoActionIsRuledOutWithEvidence(t *testing.T) {
	d := WithSelfActions(Diagnosis{Family: FamilyWAL}, []Observation{actionsObs("E3")})
	h := requireStatus(t, d, SageOwnAction, StatusRuledOut)
	if h.Contradict[0].EvidenceID != "E3" {
		t.Fatalf("contradiction = %+v", h.Contradict)
	}
}

// Many actions still never make it the root: it is a question to check,
// not a mechanism the graph can prove.
func TestSelfActions_NeverRootAlone(t *testing.T) {
	var rows []probes.Row
	for i := 0; i < 20; i++ {
		rows = append(rows, probes.Row{"id": int64(i + 1), "action_type": "vacuum",
			"outcome": "success", "executed_at": t0, "age_s": 60.0})
	}
	d := WithSelfActions(Diagnosis{Family: FamilyLockBlocking,
		Reason: "no lock waits at probe time"}, []Observation{actionsObs("E1", rows...)})
	if d.Root != nil || d.Conclusive {
		t.Fatalf("self action became the root: %+v", d)
	}
	h := requireStatus(t, d, SageOwnAction, StatusAlternative)
	if h.Confidence >= SupportThreshold {
		t.Fatalf("self action confidence %v reaches the support threshold", h.Confidence)
	}
	if len(h.Support) > 3 {
		t.Fatalf("self action cites %d facts, want at most 3", len(h.Support))
	}
}

func TestSelfActions_UnreadableHistoryIsUnprovenAndMissing(t *testing.T) {
	for name, input := range map[string][]Observation{
		"not collected": nil,
		"unsupported":   {unavailable("E1", probes.SageActions, probes.StatusUnsupported)},
	} {
		d := WithSelfActions(Diagnosis{Family: FamilyWAL}, input)
		h := requireStatus(t, d, SageOwnAction, StatusAlternative)
		if len(h.Support)+len(h.Contradict) != 0 || len(d.Missing) == 0 ||
			d.Missing[len(d.Missing)-1].ProbeID != probes.SageActions {
			t.Errorf("%s: hypothesis %+v, missing %+v", name, h, d.Missing)
		}
	}
}

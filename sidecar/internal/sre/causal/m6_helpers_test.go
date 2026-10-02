package causal

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Builders for the M6 reactive families' synthetic probe evidence.

var m6t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// mib (1 << 20) is declared in wal_test.go.

func obsAt(ev string, id probes.ID, at time.Duration, rows ...probes.Row) Observation {
	st := probes.StatusOK
	if len(rows) == 0 {
		st = probes.StatusEmpty
	}
	return Observation{EvidenceID: ev, Result: probes.Result{ProbeID: id, Version: "v1",
		Status: st, ObservedAt: m6t0.Add(at), Rows: rows}}
}

func failedObs(ev string, id probes.ID, st probes.Status, reason string) Observation {
	return Observation{EvidenceID: ev, Result: probes.Result{ProbeID: id, Version: "v1",
		Status: st, Reason: reason, ObservedAt: m6t0}}
}

// statusOf finds a node's status in a diagnosis ("" when absent).
func statusOf(d Diagnosis, id NodeID) Status {
	if d.Root != nil && d.Root.Node == id {
		return StatusRoot
	}
	for _, group := range [][]Hypothesis{d.Contributing, d.Alternatives, d.RuledOut} {
		for _, h := range group {
			if h.Node == id {
				return h.Status
			}
		}
	}
	return ""
}

func hypothesisOf(d Diagnosis, id NodeID) (Hypothesis, bool) {
	if d.Root != nil && d.Root.Node == id {
		return *d.Root, true
	}
	for _, group := range [][]Hypothesis{d.Contributing, d.Alternatives, d.RuledOut} {
		for _, h := range group {
			if h.Node == id {
				return h, true
			}
		}
	}
	return Hypothesis{}, false
}

func wantRoot(t *testing.T, d Diagnosis, id NodeID) {
	t.Helper()
	if d.Root == nil || d.Root.Node != id || !d.Conclusive {
		t.Fatalf("root = %+v (conclusive %v), want %s; diagnosis %+v", d.Root,
			d.Conclusive, id, d)
	}
}

func wantInconclusive(t *testing.T, d Diagnosis) {
	t.Helper()
	if d.Root != nil || d.Conclusive {
		t.Fatalf("root = %+v, want an inconclusive diagnosis", d.Root)
	}
}

func wantStatus(t *testing.T, d Diagnosis, id NodeID, st Status) {
	t.Helper()
	if got := statusOf(d, id); got != st {
		h, _ := hypothesisOf(d, id)
		t.Fatalf("%s status = %q, want %q (hypothesis %+v)", id, got, st, h)
	}
}

func wantMissing(t *testing.T, d Diagnosis, id probes.ID, reason string) {
	t.Helper()
	for _, m := range d.Missing {
		if m.ProbeID == id && (reason == "" || m.Reason == reason) {
			return
		}
	}
	t.Fatalf("missing = %+v, want %s %s", d.Missing, id, reason)
}

// wantFact checks a hypothesis cites ev with text containing substr.
func wantFact(t *testing.T, fs []Fact, ev, substr string) {
	t.Helper()
	for _, f := range fs {
		if f.EvidenceID == ev && strings.Contains(f.Text, substr) {
			return
		}
	}
	t.Fatalf("facts %+v lack %q citing %s", fs, substr, ev)
}

// everyHypothesisCarriesRefutation is CHECK-37 for one diagnosis.
func everyHypothesisCarriesRefutation(t *testing.T, d Diagnosis) {
	t.Helper()
	all := append(append(append([]Hypothesis{}, d.Contributing...), d.Alternatives...),
		d.RuledOut...)
	if d.Root != nil {
		all = append(all, *d.Root)
	}
	if len(all) == 0 {
		t.Fatal("diagnosis has no hypotheses")
	}
	for _, h := range all {
		if h.RefutationProbe == "" || h.OperatorStep == "" {
			t.Errorf("%s lacks a refutation probe or operator step", h.Node)
		}
		n, ok := NodeByID(h.Node)
		if !ok || n.Family != d.Family {
			t.Errorf("%s is not a %s node", h.Node, d.Family)
		}
	}
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

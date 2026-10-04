package sre

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// Roadmap 2.4: the blanket "the model may never change a graph root" rule
// is retired for a measured, per-family one. When the graph is conclusive
// and the model ranks another open hypothesis first, the contest is kept
// beside the diagnosis: advisory (the graph's root stands, L1) unless the
// family's root authority was earned on the held-out bench, in which case
// the model's root is adopted and the graph's root is kept as a
// contributing factor. The contest is validated before any store I/O.

// No concurrent access tests: adoptRoot and the contest validation are
// pure functions of their arguments.

func TestAdoptRoot_MovesTheModelNodeToTheRoot(t *testing.T) {
	_, _, d := idleChainFixture(t)
	if d.Root == nil || d.Root.Node != "idle_in_tx_holder" || !d.Conclusive {
		t.Fatalf("fixture diagnosis = %+v", d)
	}
	before := len(d.Contributing)
	got, ok := adoptRoot(d, "ddl_lock_queue")
	if !ok || got.Root == nil || got.Root.Node != "ddl_lock_queue" ||
		got.Root.Status != causal.StatusRoot || !got.Conclusive {
		t.Fatalf("adopted = %+v (%v)", got.Root, ok)
	}
	statuses := map[causal.NodeID]causal.Status{}
	for _, h := range append(append([]causal.Hypothesis(nil), got.Contributing...),
		got.Alternatives...) {
		statuses[h.Node] = h.Status
	}
	if statuses["idle_in_tx_holder"] != causal.StatusContributing {
		t.Fatalf("the graph's root must stay as a contributing factor: %v", statuses)
	}
	if _, still := statuses["ddl_lock_queue"]; still {
		t.Fatalf("the adopted node is listed twice: %v", statuses)
	}
	if d.Root.Node != "idle_in_tx_holder" || len(d.Contributing) != before {
		t.Fatal("adoptRoot modified its input diagnosis")
	}
	c := conclusionOf(got)
	if c.State != StateConcluded || c.Summary.Root != "ddl_lock_queue" {
		t.Fatalf("conclusion = %s root %q", c.State, c.Summary.Root)
	}
	if err := c.validate(); err != nil {
		t.Fatalf("the adopted conclusion must validate: %v", err)
	}
}

func TestAdoptRoot_RefusesWhatIsNotAnOpenAlternative(t *testing.T) {
	_, _, d := idleChainFixture(t)
	for _, node := range []string{"", "idle_in_tx_holder", "no_such_node"} {
		if got, ok := adoptRoot(d, node); ok || got.Root.Node != "idle_in_tx_holder" {
			t.Errorf("%q: adopted %+v", node, got.Root)
		}
	}
	if len(d.RuledOut) > 0 {
		if _, ok := adoptRoot(d, string(d.RuledOut[0].Node)); ok {
			t.Errorf("a ruled-out hypothesis %s was adopted", d.RuledOut[0].Node)
		}
	}
	_, _, unknown := unknownLockFixture(t)
	if _, ok := adoptRoot(unknown, "idle_in_tx_holder"); ok {
		t.Error("an inconclusive diagnosis has no root to override")
	}
}

// contested is the idle-chain conclusion with a contest of authority.
func contested(t *testing.T, authority string) Conclusion {
	t.Helper()
	_, _, d := idleChainFixture(t)
	graph := string(d.Root.Node)
	if authority == ContestAdopted {
		d, _ = adoptRoot(d, "ddl_lock_queue")
	}
	c := conclusionOf(d)
	c.Summary.ModelContest = &ModelContest{Label: ModelContestLabel, GraphRoot: graph,
		ModelRoot: "ddl_lock_queue", Authority: authority, Reason: "measured"}
	return c
}

func TestModelContest_ValidAdvisoryAndAdopted(t *testing.T) {
	for _, a := range []string{ContestAdvisory, ContestAdopted} {
		if err := contested(t, a).validate(); err != nil {
			t.Errorf("%s contest rejected: %v", a, err)
		}
	}
}

func TestModelContest_Negatives(t *testing.T) {
	cases := map[string]struct {
		authority string
		mutate    func(*ModelContest)
	}{
		"no label": {ContestAdvisory, func(m *ModelContest) { m.Label = "" }},
		"same root": {ContestAdvisory, func(m *ModelContest) {
			m.ModelRoot = m.GraphRoot
		}},
		"unknown authority": {ContestAdvisory, func(m *ModelContest) { m.Authority = "auto" }},
		"model root not a hypothesis": {ContestAdvisory, func(m *ModelContest) {
			m.ModelRoot = "checkpoint_storm_root"
		}},
		"graph root not a hypothesis": {ContestAdvisory, func(m *ModelContest) {
			m.GraphRoot = "no_such_node"
		}},
		"advisory but model root stored": {ContestAdopted, func(m *ModelContest) {
			m.Authority = ContestAdvisory
		}},
		"adopted but graph root stored": {ContestAdvisory, func(m *ModelContest) {
			m.Authority = ContestAdopted
		}},
		"reason too long": {ContestAdvisory, func(m *ModelContest) {
			m.Reason = strings.Repeat("r", 501)
		}},
		"empty reason": {ContestAdvisory, func(m *ModelContest) { m.Reason = " " }},
	}
	for name, c := range cases {
		conc := contested(t, c.authority)
		c.mutate(conc.Summary.ModelContest)
		if err := conc.validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestModelContest_NeedsAConcludedInvestigation(t *testing.T) {
	_, _, d := unknownLockFixture(t)
	c := conclusionOf(d)
	c.Summary.ModelContest = &ModelContest{Label: ModelContestLabel,
		GraphRoot: "idle_in_tx_holder", ModelRoot: "ddl_lock_queue",
		Authority: ContestAdvisory, Reason: "x"}
	if err := c.validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a contest on an inconclusive investigation: err = %v", err)
	}
}

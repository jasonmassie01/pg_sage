package srebench

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// Every family has clean, noise, decoy and benign scenarios (CHECK-42).
// A decoy is a benign lookalike of one of its family's mechanisms whose
// right answer is "inconclusive"; a noise variant is a clean fault under
// unrelated load, so it has a clean twin with the same gold root. Every
// fault program has a manifestation predicate and a post-fix verifier.

var benchFamilies = []sre.TriggerKind{sre.TriggerLock, sre.TriggerConnections,
	sre.TriggerWAL, sre.TriggerPlan, sre.TriggerWraparound, sre.TriggerDiskWAL,
	sre.TriggerSequence}

// ofFamily reports a node of the scenario family. The disk/WAL runway
// shares the WAL retention mechanisms (slots, archiver, write surge) by
// design (graph v3), so their nodes are its mechanisms too.
func ofFamily(n causal.Node, kind sre.TriggerKind) bool {
	return string(n.Family) == string(kind) ||
		(kind == sre.TriggerDiskWAL && n.Family == causal.FamilyWAL)
}

func checkGold(t *testing.T, sc Scenario) {
	t.Helper()
	switch sc.Class {
	case ClassPositive, ClassNoise:
		if sc.Gold.Root == "" || sc.Gold.Lookalike != "" {
			t.Errorf("%s: a %s scenario needs a gold root and no lookalike", sc.ID, sc.Class)
		}
	case ClassDecoy:
		n, ok := causal.NodeByID(causal.NodeID(sc.Gold.Lookalike))
		if sc.Gold.Root != "" || !ok || !ofFamily(n, sc.Family) {
			t.Errorf("%s: a decoy needs no root and a lookalike of its family (%q)", sc.ID,
				sc.Gold.Lookalike)
		}
	case ClassBenign:
		if sc.Gold.Root != "" || sc.Gold.Lookalike != "" {
			t.Errorf("benign %s has a gold root or lookalike", sc.ID)
		}
	default:
		t.Errorf("%s has class %q", sc.ID, sc.Class)
	}
	for _, node := range append([]string{sc.Gold.Root}, sc.Gold.Contributing...) {
		if node == "" {
			continue
		}
		if n, ok := causal.NodeByID(causal.NodeID(node)); !ok || !ofFamily(n, sc.Family) {
			t.Errorf("%s: gold node %q is not a %s mechanism", sc.ID, node, sc.Family)
		}
	}
}

func TestScenarios_AreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	classes := map[sre.TriggerKind]map[string]int{}
	clean := map[string]bool{}
	for _, sc := range Scenarios() {
		if sc.ID == "" || seen[sc.ID] || sc.Program == nil {
			t.Fatalf("scenario %+v is malformed or duplicated", sc)
		}
		seen[sc.ID] = true
		if classes[sc.Family] == nil {
			classes[sc.Family] = map[string]int{}
		}
		classes[sc.Family][sc.Class]++
		checkGold(t, sc)
		if sc.Class == ClassPositive {
			clean[string(sc.Family)+"/"+sc.Gold.Root] = true
		}
		p, ok := sc.Program.(program)
		if !ok || p.manifest == nil || p.recover == nil {
			t.Errorf("%s: needs a manifestation predicate and a post-fix verifier", sc.ID)
		}
	}
	for _, sc := range Scenarios() {
		if sc.Class == ClassNoise && !clean[string(sc.Family)+"/"+sc.Gold.Root] {
			t.Errorf("noise %s has no clean twin with root %s", sc.ID, sc.Gold.Root)
		}
	}
	for _, f := range benchFamilies {
		for _, class := range []string{ClassPositive, ClassNoise, ClassDecoy, ClassBenign} {
			if classes[f][class] == 0 {
				t.Errorf("family %s has no %s scenario", f, class)
			}
		}
	}
}

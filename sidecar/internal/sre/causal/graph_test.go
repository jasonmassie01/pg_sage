package causal

import (
	"math"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The graph is static and versioned: each node is a mechanism with its
// predicted observations, a refutation probe from the catalog (or
// none_available) and the nodes it amplifies.

func TestGraph_NodesAreWellFormed(t *testing.T) {
	if GraphVersion == "" {
		t.Fatal("graph version is empty")
	}
	nodes := Graph()
	// v1: 4 lock + 2 plan; v2: connections 3, WAL 4, own change 1; v3: recent
	// change 1 (M5), checkpoint 4, temp files 3, replication lag 6, LWLock 5
	// (M6 reactive), wraparound 7, disk/WAL growth 1 (shares WAL's slot,
	// archiver and surge nodes) and sequences 3 (M6 runways).
	if len(nodes) != 44 {
		t.Fatalf("graph has %d nodes, want 44", len(nodes))
	}
	families := map[Family]bool{FamilyLockBlocking: true, FamilyPlanRegression: true,
		FamilyConnections: true, FamilyWAL: true, FamilyChange: true,
		FamilyCheckpoint: true, FamilyTempFiles: true, FamilyReplicationLag: true,
		FamilyLWLock: true, FamilyWraparound: true, FamilyDiskWAL: true,
		FamilySequence: true}
	seen := map[NodeID]bool{}
	for _, n := range nodes {
		if seen[n.ID] {
			t.Fatalf("duplicate node %s", n.ID)
		}
		seen[n.ID] = true
		if !families[n.Family] {
			t.Errorf("%s has family %q", n.ID, n.Family)
		}
		if n.Mechanism == "" || n.Predicted == "" {
			t.Errorf("%s lacks mechanism or predicted observations", n.ID)
		}
		if n.Refutation != NoRefutation && !probes.IsSignal(probes.ID(n.Refutation)) {
			if _, ok := probes.Catalog().Spec(probes.ID(n.Refutation)); !ok {
				t.Errorf("%s refutation probe %q is not in the catalog",
					n.ID, n.Refutation)
			}
		}
	}
	for _, n := range nodes {
		for _, a := range n.Amplifies {
			target, ok := NodeByID(a)
			if !ok || target.Family != n.Family || a == n.ID {
				t.Errorf("%s amplifies invalid node %s", n.ID, a)
			}
		}
	}
	if _, ok := NodeByID("made_up"); ok {
		t.Fatal("NodeByID resolved an unknown node")
	}
}

func TestGraph_ReturnsACopy(t *testing.T) {
	nodes := Graph()
	nodes[0].Mechanism = "tampered"
	if Graph()[0].Mechanism == "tampered" {
		t.Fatal("Graph exposes its internal table")
	}
}

// Confidence threshold boundary: 0.50 supports, just below does not.
func TestRank_ThresholdBoundary(t *testing.T) {
	for _, c := range []struct {
		conf float64
		root bool
	}{{SupportThreshold, true}, {math.Nextafter(SupportThreshold, 0), false},
		{0, false}, {1, true}} {
		hs := []Hypothesis{{Node: HotRowContention, Confidence: c.conf,
			Support: []Fact{{EvidenceID: "P1", Text: "x"}}}}
		d := rank(FamilyLockBlocking, hs)
		if (d.Root != nil) != c.root || d.Conclusive != c.root {
			t.Errorf("confidence %v: root=%v conclusive=%v, want %v", c.conf,
				d.Root != nil, d.Conclusive, c.root)
		}
		if !c.root && (len(d.Alternatives) != 1 ||
			d.Alternatives[0].Status != StatusAlternative) {
			t.Errorf("confidence %v not kept as an alternative: %+v", c.conf, d)
		}
	}
}

func TestRank_RuledOutNeverBecomesRoot(t *testing.T) {
	hs := []Hypothesis{{Node: IdleInTxHolder, Confidence: 0.9,
		Contradict: []Fact{{EvidenceID: "P1", Text: "root is active"}}}}
	d := rank(FamilyLockBlocking, hs)
	if d.Root != nil || len(d.RuledOut) != 1 {
		t.Fatalf("a contradicted hypothesis became root: %+v", d)
	}
}

func TestRank_TieBreaksByGraphOrder(t *testing.T) {
	hs := []Hypothesis{
		{Node: HotRowContention, Confidence: 0.7},
		{Node: PreparedXactHolder, Confidence: 0.7},
	}
	d := rank(FamilyLockBlocking, hs)
	if d.Root == nil || d.Root.Node != PreparedXactHolder {
		t.Fatalf("root = %+v, want the earlier graph node on a tie", d.Root)
	}
}

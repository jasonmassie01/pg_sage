package causal

import "testing"

// Graph v4 (Sage SRE follow-ups B, after v1.8.0 shipped v3): pool
// exhaustion at an external pooler joins the connection family
// (CHECK-04), and two-sample comparisons are refused across a restart,
// failover, other server or major-version change (CHECK-07). A
// diagnosis records the graph that produced it, so the version moves.
func TestGraphV4_Version(t *testing.T) {
	if GraphVersion != "causal-v4" {
		t.Fatalf("graph version = %q, want causal-v4 (pooler node, identity checks)",
			GraphVersion)
	}
	if _, ok := NodeByID(PoolerSaturation); !ok {
		t.Fatal("causal-v4 has no pooler node")
	}
}

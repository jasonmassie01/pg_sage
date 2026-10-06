package fleetlearn

import (
	"math"
	"testing"
)

func fp(db, boundary string, tables, indexes, queries []string) Fingerprint {
	return Fingerprint{Database: db, Boundary: boundary, Tables: tables,
		Indexes: indexes, Queries: queries}
}

func TestSimilarity_IdenticalIsOneDisjointIsZero(t *testing.T) {
	a := fp("a", "", []string{"t1", "t2"}, []string{"i1"}, []string{"q1"})
	b := fp("b", "", []string{"t1", "t2"}, []string{"i1"}, []string{"q1"})
	if got := Similarity(a, b); math.Abs(got-1) > 1e-9 {
		t.Fatalf("identical similarity = %v, want 1", got)
	}
	c := fp("c", "", []string{"x"}, []string{"y"}, []string{"z"})
	if got := Similarity(a, c); got != 0 {
		t.Fatalf("disjoint similarity = %v, want 0", got)
	}
}

func TestSimilarity_WeightsAndMissingDimensions(t *testing.T) {
	// Tables identical, indexes and queries disjoint: only the table weight.
	a := fp("a", "", []string{"t1"}, []string{"i1"}, []string{"q1"})
	b := fp("b", "", []string{"t1"}, []string{"i2"}, []string{"q2"})
	if got := Similarity(a, b); math.Abs(got-WeightTables) > 1e-9 {
		t.Fatalf("tables-only overlap = %v, want %v", got, WeightTables)
	}
	// Neither side has queries (no pg_stat_statements): the query weight
	// is redistributed, so identical schemas still score 1.
	c := fp("c", "", []string{"t1"}, []string{"i1"}, nil)
	d := fp("d", "", []string{"t1"}, []string{"i1"}, nil)
	if got := Similarity(c, d); math.Abs(got-1) > 1e-9 {
		t.Fatalf("no-query identical similarity = %v, want 1", got)
	}
	// Only one side has queries: that dimension counts as zero overlap.
	e := fp("e", "", []string{"t1"}, []string{"i1"}, []string{"q1"})
	if got := Similarity(c, e); math.Abs(got-(WeightTables+WeightIndexes)) > 1e-9 {
		t.Fatalf("one-sided queries = %v, want %v", got, WeightTables+WeightIndexes)
	}
}

func TestSimilarity_EmptyFingerprints(t *testing.T) {
	if got := Similarity(Fingerprint{}, Fingerprint{}); got != 0 {
		t.Fatalf("two empty fingerprints = %v, want 0 (nothing to compare)", got)
	}
	a := fp("a", "", []string{"t1"}, nil, nil)
	if got := Similarity(a, Fingerprint{}); got != 0 {
		t.Fatalf("against empty = %v, want 0", got)
	}
}

func TestSimilarity_IsSymmetricAndIgnoresDuplicates(t *testing.T) {
	a := fp("a", "", []string{"t1", "t2", "t2"}, []string{"i1"}, []string{"q1", "q2"})
	b := fp("b", "", []string{"t2", "t3"}, []string{"i1", "i9"}, []string{"q2"})
	if Similarity(a, b) != Similarity(b, a) {
		t.Fatal("similarity is not symmetric")
	}
	// tables {t1,t2} vs {t2,t3}: 1/3; indexes 1/2; queries 1/2.
	want := WeightTables/3 + WeightIndexes/2 + WeightQueries/2
	if got := Similarity(a, b); math.Abs(got-want) > 1e-9 {
		t.Fatalf("similarity = %v, want %v", got, want)
	}
}

func TestLookAlikes_RespectsBoundaryThresholdSelfAndOrder(t *testing.T) {
	target := fp("t", "", []string{"a", "b"}, []string{"i"}, []string{"q"})
	peers := []Fingerprint{
		target, // itself: never its own look-alike
		fp("same", "", []string{"a", "b"}, []string{"i"}, []string{"q"}),
		fp("close", "", []string{"a", "b"}, []string{"i"}, []string{"z"}),
		fp("far", "", []string{"x"}, nil, nil),
		fp("other-tenant", "tenant:acme", []string{"a", "b"}, []string{"i"}, []string{"q"}),
	}
	got := LookAlikes(target, peers, 0.6)
	if len(got) != 2 {
		t.Fatalf("look-alikes = %+v, want same and close", got)
	}
	if got[0].Database != "same" || got[1].Database != "close" {
		t.Fatalf("order = %+v, want most similar first", got)
	}
	if got[0].Similarity < got[1].Similarity {
		t.Fatal("not sorted by similarity")
	}
	for _, l := range got {
		if l.Database == "other-tenant" || l.Database == "t" {
			t.Fatalf("crossed a boundary or matched itself: %+v", l)
		}
	}
}

func TestLookAlikes_ThresholdBoundaryIsInclusive(t *testing.T) {
	target := fp("t", "", []string{"a"}, []string{"i"}, []string{"q"})
	peer := fp("p", "", []string{"a"}, []string{"x"}, []string{"y"})
	s := Similarity(target, peer)
	if got := LookAlikes(target, []Fingerprint{peer}, s); len(got) != 1 {
		t.Fatalf("similarity equal to the threshold must qualify, got %+v", got)
	}
	if got := LookAlikes(target, []Fingerprint{peer}, s+1e-6); len(got) != 0 {
		t.Fatalf("below the threshold must not qualify, got %+v", got)
	}
}

func TestLookAlikes_IsolatedBoundaryHasNoPeers(t *testing.T) {
	b := "agentdb-isolated:agentdb:x"
	target := fp("agentdb:x", b, []string{"a"}, nil, nil)
	peer := fp("agentdb:y", b, []string{"a"}, nil, nil)
	if got := LookAlikes(target, []Fingerprint{peer}, 0.1); len(got) != 0 {
		t.Fatalf("an isolated database got peers: %+v", got)
	}
	if got := LookAlikes(target, nil, 0.1); got == nil || len(got) != 0 {
		t.Fatalf("no peers must be an empty, non-nil list, got %#v", got)
	}
}

func TestLookAlikes_EmptyTargetHasNoPeers(t *testing.T) {
	peers := []Fingerprint{fp("p", "", nil, nil, nil)}
	if got := LookAlikes(Fingerprint{Database: "t"}, peers, 0); len(got) != 0 {
		t.Fatalf("an empty fingerprint matched: %+v", got)
	}
}

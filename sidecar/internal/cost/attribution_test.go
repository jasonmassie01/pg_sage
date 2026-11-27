package cost

import (
	"math"
	"testing"
	"time"
)

// Per-query cost over a window = the instance's compute cost x the query's
// share of the recorded execution time; idle_share is the part of the
// instance's vCPU time no recorded query used.

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

func TestAttribute_SharesTheComputeCostByExecTime(t *testing.T) {
	deltas := []QueryDelta{
		{Database: "orders", QueryID: 2, ExecMs: 30_000},
		{Database: "orders", QueryID: 1, ExecMs: 50_000},
		{Database: "orders", QueryID: 3, ExecMs: 20_000},
	}
	a := Attribute(1.0, time.Hour, 1, deltas)
	if !near(a.ComputeCost, 1.0) || !near(a.TotalExecSeconds, 100) {
		t.Fatalf("compute %v exec %v, want $1 and 100 s", a.ComputeCost, a.TotalExecSeconds)
	}
	want := []struct {
		id   string
		cost float64
	}{{"1", 0.50}, {"2", 0.30}, {"3", 0.20}}
	if len(a.Queries) != 3 {
		t.Fatalf("%d queries, want 3", len(a.Queries))
	}
	for i, w := range want {
		q := a.Queries[i]
		if q.QueryID != w.id || !near(q.Cost, w.cost) || !near(q.Share, w.cost) ||
			q.Database != "orders" {
			t.Fatalf("query %d = %+v, want id %s cost/share %v (sorted by cost)", i, q,
				w.id, w.cost)
		}
	}
	if a.IdleShare == nil || !near(*a.IdleShare, 1-100.0/3600) {
		t.Fatalf("idle share = %v, want 1 - 100/3600", a.IdleShare)
	}
}

func TestAttribute_IdleShareUsesVCPUs(t *testing.T) {
	a := Attribute(2.0, 2*time.Hour, 4, []QueryDelta{{QueryID: 9, ExecMs: 7_200_000}})
	// 7200 s of execution over 2 h x 4 vCPUs = 28800 vCPU-seconds: 75% idle.
	if a.IdleShare == nil || !near(*a.IdleShare, 0.75) || !near(a.ComputeCost, 4) {
		t.Fatalf("idle %v compute %v, want 0.75 and $4", a.IdleShare, a.ComputeCost)
	}
}

func TestAttribute_UnknownVCPUsGiveUnknownIdleShare(t *testing.T) {
	a := Attribute(1, time.Hour, 0, []QueryDelta{{QueryID: 1, ExecMs: 1000}})
	if a.IdleShare != nil {
		t.Fatalf("idle share = %v, want unknown without vCPUs", *a.IdleShare)
	}
	if len(a.Queries) != 1 || !near(a.Queries[0].Cost, 1) {
		t.Fatalf("queries = %+v, want the full cost on the one query", a.Queries)
	}
}

func TestAttribute_OverCommittedIdleShareIsZeroNotNegative(t *testing.T) {
	// More recorded time than vCPU time (parallel workers count per worker).
	a := Attribute(1, time.Hour, 1, []QueryDelta{{QueryID: 1, ExecMs: 7_200_000}})
	if a.IdleShare == nil || *a.IdleShare != 0 {
		t.Fatalf("idle share = %v, want clamped to 0", a.IdleShare)
	}
}

func TestAttribute_EmptyAndZeroInputs(t *testing.T) {
	a := Attribute(1, time.Hour, 2, nil)
	if a.Queries == nil || len(a.Queries) != 0 || a.TotalExecSeconds != 0 {
		t.Fatalf("no deltas: %+v, want an empty (non-nil) list", a)
	}
	if a.IdleShare == nil || *a.IdleShare != 1 || !near(a.ComputeCost, 1) {
		t.Fatalf("no deltas: idle %v compute %v, want fully idle and $1", a.IdleShare,
			a.ComputeCost)
	}
	z := Attribute(1, time.Hour, 2, []QueryDelta{{QueryID: 1, ExecMs: 0}})
	if len(z.Queries) != 0 {
		t.Fatalf("zero delta: %+v, want no query charged", z.Queries)
	}
	w := Attribute(1, 0, 2, []QueryDelta{{QueryID: 1, ExecMs: 10}})
	if w.ComputeCost != 0 || w.IdleShare != nil {
		t.Fatalf("zero window: %+v, want no cost and unknown idle", w)
	}
}

func TestAttribute_InvalidDeltasAreDroppedNeverNegative(t *testing.T) {
	a := Attribute(1, time.Hour, 1, []QueryDelta{
		{QueryID: 1, ExecMs: 1000},
		{QueryID: 2, ExecMs: -500},
		{QueryID: 3, ExecMs: math.NaN()},
		{QueryID: 4, ExecMs: math.Inf(1)},
	})
	if len(a.Queries) != 1 || a.Queries[0].QueryID != "1" || !near(a.Queries[0].Share, 1) {
		t.Fatalf("queries = %+v, want only the valid delta", a.Queries)
	}
	if a.Dropped != 3 {
		t.Fatalf("dropped = %d, want 3", a.Dropped)
	}
}

func TestAttribute_SharesSumToOne(t *testing.T) {
	var deltas []QueryDelta
	for i := 1; i <= 57; i++ {
		deltas = append(deltas, QueryDelta{Database: "a", QueryID: int64(i),
			ExecMs: float64(i*i) * 1.37})
	}
	a := Attribute(3.21, 5*time.Hour, 8, deltas)
	var share, cost float64
	for _, q := range a.Queries {
		share += q.Share
		cost += q.Cost
	}
	if math.Abs(share-1) > 1e-9 || math.Abs(cost-a.ComputeCost) > 1e-9 {
		t.Fatalf("shares sum %v, costs sum %v of %v", share, cost, a.ComputeCost)
	}
}

func TestAttribute_SameQueryIDInTwoDatabasesStaysApart(t *testing.T) {
	a := Attribute(1, time.Hour, 0, []QueryDelta{
		{Database: "orders", QueryID: 7, ExecMs: 3000},
		{Database: "billing", QueryID: 7, ExecMs: 1000},
	})
	if len(a.Queries) != 2 || a.Queries[0].Database != "orders" ||
		a.Queries[1].Database != "billing" || !near(a.Queries[1].Cost, 0.25) {
		t.Fatalf("queries = %+v, want one entry per database", a.Queries)
	}
}

func TestAttribute_LargeQueryIDIsExactText(t *testing.T) {
	a := Attribute(1, time.Hour, 0, []QueryDelta{{QueryID: -9_100_000_000_000_000_001,
		ExecMs: 1}})
	if a.Queries[0].QueryID != "-9100000000000000001" {
		t.Fatalf("queryid = %q, want the exact 64-bit value as text", a.Queries[0].QueryID)
	}
}

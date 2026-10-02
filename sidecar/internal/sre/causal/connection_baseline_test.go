package causal

import (
	"strings"
	"testing"
	"time"
)

// A leak beside another application's stable pool. The stable pool
// cannot explain pressure that is growing: it is the baseline the leak
// grows on, so it contributes and the leak is the root. This was a tie
// (0.75 each) that graph order handed to pool_fan_out; the replay case
// conn-leak-beside-healthy-pool exposed it.
//
// No concurrent access tests: DiagnoseConnections is a pure function of
// its input and holds no shared state.

func leakBesidePool(before, after, waiting int64, restart bool) []Observation {
	second := serverStart
	if restart {
		second = serverStart.Add(time.Minute)
	}
	sample := func(id string, at time.Time, etl int64, started time.Time) Observation {
		gs := []group{{app: "web", state: "idle", n: 32, serverStarts: started},
			{app: "web", state: "active", n: 2, serverStarts: started},
			{app: "etl", state: "idle", n: etl, serverStarts: started}}
		if waiting > 0 {
			gs = append(gs, group{app: "etl", state: "active", n: waiting,
				waiting: waiting, serverStarts: started})
		}
		return connObs(id, at, 34+etl+waiting, gs...)
	}
	return []Observation{sample("E1", t0, before, serverStart),
		sample("E2", t0.Add(5*time.Second), after, second)}
}

func requireNoContributing(t *testing.T, d Diagnosis) {
	t.Helper()
	if len(d.Contributing) != 0 {
		t.Fatalf("contributing = %+v, want none", d.Contributing)
	}
}

func TestConnections_LeakBesideStablePoolIsTheRoot(t *testing.T) {
	d := DiagnoseConnections(leakBesidePool(4, 13, 0, false))
	root := requireStatus(t, d, ConnectionLeak, StatusRoot)
	if root.Subject != `application "etl"` || root.Confidence != 0.75 ||
		!strings.Contains(root.Support[0].Text, "4 to 13") {
		t.Fatalf("leak root = %+v, want etl growing 4 to 13 at 0.75", root)
	}
	pool := requireStatus(t, d, PoolFanOut, StatusContributing)
	if pool.Subject != `application "web"` || pool.Confidence != 0.75 {
		t.Fatalf("contributing pool = %+v, want web's stable pool at 0.75", pool)
	}
	if !d.Conclusive || d.Subject != `application "etl"` || len(d.Contributing) != 1 {
		t.Fatalf("diagnosis = %+v, want conclusive on etl with one contributor", d)
	}
	requireStatus(t, d, BlockedBacklog, StatusRuledOut)
}

// A leak at the growth threshold is supported even when the stable pool
// beside it scores higher: the pool was there before the pressure grew.
func TestConnections_SmallLeakBesideStablePool(t *testing.T) {
	d := DiagnoseConnections(leakBesidePool(2, 2+leakMinGrowth, 0, false))
	root := requireStatus(t, d, ConnectionLeak, StatusRoot)
	if root.Confidence != 0.55 || root.Subject != `application "etl"` {
		t.Fatalf("leak root = %+v, want etl at 0.55", root)
	}
	requireStatus(t, d, PoolFanOut, StatusContributing)
}

// Boundary: growth below the threshold is no leak; the pool stays root
// and nothing contributes.
func TestConnections_GrowthBelowThresholdBesideStablePool(t *testing.T) {
	d := DiagnoseConnections(leakBesidePool(2, 1+leakMinGrowth, 0, false))
	root := requireStatus(t, d, PoolFanOut, StatusRoot)
	if root.Subject != `application "web"` {
		t.Fatalf("fan-out root = %+v, want web", root)
	}
	requireStatus(t, d, ConnectionLeak, StatusAlternative)
	requireNoContributing(t, d)
}

// One application both holding and growing its idle backends is a leak;
// its own pool is not a separate baseline and does not contribute.
func TestConnections_SameApplicationPoolDoesNotContribute(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObs("E1", t0, 10, group{app: "worker", state: "idle", n: 5}),
		connObs("E2", t0.Add(5*time.Second), 19, group{app: "worker", state: "idle", n: 14}),
	})
	requireStatus(t, d, ConnectionLeak, StatusRoot)
	requireStatus(t, d, PoolFanOut, StatusAlternative)
	requireNoContributing(t, d)
}

// Pure fan-out: two applications with stable pools, nothing grows.
func TestConnections_PureFanOutAcrossStablePools(t *testing.T) {
	d := DiagnoseConnections(leakBesidePool(4, 4, 0, false))
	root := requireStatus(t, d, PoolFanOut, StatusRoot)
	if root.Subject != `application "web"` || root.Confidence != 0.75 {
		t.Fatalf("fan-out root = %+v, want web at 0.75", root)
	}
	requireStatus(t, d, ConnectionLeak, StatusRuledOut)
	requireNoContributing(t, d)
}

// CHECK-07: after a restart growth is unknown, so the pool cannot be the
// baseline of a leak nobody can see.
func TestConnections_RestartBesideStablePool(t *testing.T) {
	d := DiagnoseConnections(leakBesidePool(4, 13, 0, true))
	requireStatus(t, d, PoolFanOut, StatusRoot)
	requireStatus(t, d, ConnectionLeak, StatusAlternative)
	requireNoContributing(t, d)
}

// Lock waits refute idle pools: a contradicted pool is ruled out, never
// a contributor, even beside a growing application.
func TestConnections_LockWaitsRuleOutThePoolBesideALeak(t *testing.T) {
	d := DiagnoseConnections(leakBesidePool(4, 13, backlogMinWaiting+1, false))
	requireStatus(t, d, BlockedBacklog, StatusRoot)
	requireStatus(t, d, PoolFanOut, StatusRuledOut)
	requireStatus(t, d, ConnectionLeak, StatusAlternative)
	requireNoContributing(t, d)
}

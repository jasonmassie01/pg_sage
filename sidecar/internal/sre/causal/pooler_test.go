package causal

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CHECK-04: pool exhaustion at an external pooler, backend exhaustion
// and a lock backlog produce different evidence. With PgBouncer
// telemetry, clients queueing at the pooler while PostgreSQL has headroom
// are diagnosed as pool exhaustion at the pooler instead of "no
// connection pressure". Without it, the family behaves as before and
// says the pooler telemetry is unavailable.

type pgbPool struct {
	pooler, db       string
	clActive, wait   int64
	svActive, svIdle int64
	maxwait          float64
}

func poolerObs(id string, at time.Time, ps ...pgbPool) Observation {
	var rows []probes.Row
	for _, p := range ps {
		rows = append(rows, probes.Row{"pooler": p.pooler, "database": p.db, "user": "app",
			"pool_mode": "transaction", "cl_active": p.clActive, "cl_waiting": p.wait,
			"sv_active": p.svActive, "sv_idle": p.svIdle, "sv_used": int64(0),
			"maxwait_s": p.maxwait, "avg_wait_us": float64(1500)})
	}
	o := obs(id, probes.PoolerPools, rows...)
	o.Result.ObservedAt = at
	return o
}

// quietDB is a database with headroom: 20 active, 2 idle, no waits.
func quietDB(id string, at time.Time) Observation {
	return connObs(id, at, 23, group{app: "checkout", state: "active", n: 20},
		group{app: "checkout", state: "idle", n: 2})
}

var queued = pgbPool{pooler: "pgb-1", db: "checkout", clActive: 20, wait: 38,
	svActive: 20, svIdle: 0, maxwait: 4.2}

func TestPooler_QueueingWithDatabaseHeadroomIsTheRoot(t *testing.T) {
	t1 := t0.Add(5 * time.Second)
	later := queued
	later.wait, later.maxwait = 41, 6.8
	d := DiagnoseConnections([]Observation{quietDB("E1", t0), poolerObs("P1", t0, queued),
		quietDB("E2", t1), poolerObs("P2", t1, later)})
	root := requireStatus(t, d, PoolerSaturation, StatusRoot)
	if root.Subject != `pool "checkout" at pooler "pgb-1"` || root.Confidence < 0.7 {
		t.Fatalf("pooler root = %+v", root)
	}
	text := ""
	for _, f := range root.Support {
		text += f.Text + "\n"
		if f.EvidenceID != "P2" && f.EvidenceID != "P1" {
			t.Fatalf("pooler support cites %s", f.EvidenceID)
		}
	}
	for _, want := range []string{"41 clients wait", "6.8 s", "0 idle server"} {
		if !strings.Contains(text, want) {
			t.Errorf("support %q lacks %q", text, want)
		}
	}
	if !d.Conclusive || hasMissing(d, probes.PoolerPools, ReasonPoolerUnavailable) {
		t.Fatalf("diagnosis = %+v", d)
	}
	found := false
	for _, f := range d.Observed {
		found = found || (f.EvidenceID == "P2" && strings.Contains(f.Text, "pgb-1"))
	}
	if !found {
		t.Fatalf("observed facts %+v lack the pooler's state", d.Observed)
	}
}

// Without pooler telemetry the family is unchanged: the same evidence
// that is a fan-out stays a fan-out, no pooler hypothesis is invented,
// and the diagnosis says the pooler could not be seen.
func TestPooler_NotConfiguredBehavesAsBeforeAndSaysSo(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObs("E1", t0, 36, group{app: "api", state: "idle", n: 30}),
		connObs("E2", t0.Add(5*time.Second), 36, group{app: "api", state: "idle", n: 30}),
	})
	requireStatus(t, d, PoolFanOut, StatusRoot)
	if _, st := byNode(d, PoolerSaturation); st != "" {
		t.Fatalf("a pooler hypothesis appeared without pooler telemetry (%s)", st)
	}
	if !hasMissing(d, probes.PoolerPools, ReasonPoolerUnavailable) {
		t.Fatalf("missing = %+v, want the pooler telemetry stated unavailable", d.Missing)
	}
	quiet := DiagnoseConnections([]Observation{quietDB("E1", t0),
		quietDB("E2", t0.Add(5*time.Second))})
	if quiet.Conclusive || !hasMissing(quiet, probes.PoolerPools, ReasonPoolerUnavailable) {
		t.Fatalf("a quiet database without a pooler = %+v", quiet)
	}
}

func TestPooler_NoWaitingClientsRuleItOut(t *testing.T) {
	busy := pgbPool{pooler: "pgb-1", db: "checkout", clActive: 60, svActive: 18, svIdle: 2}
	d := DiagnoseConnections([]Observation{quietDB("E1", t0), poolerObs("P1", t0, busy),
		quietDB("E2", t0.Add(5*time.Second)), poolerObs("P2", t0.Add(5*time.Second), busy)})
	h := requireStatus(t, d, PoolerSaturation, StatusRuledOut)
	if len(h.Contradict) == 0 || !strings.Contains(h.Contradict[0].Text, "no client waits") {
		t.Fatalf("contradiction = %+v", h.Contradict)
	}
	if d.Conclusive {
		t.Fatalf("a busy pooler without a queue concluded %+v", d.Root)
	}
}

// Brief queueing (under a second, seen once) is not exhaustion.
func TestPooler_ThresholdBoundaries(t *testing.T) {
	cases := map[string]struct {
		maxwait   float64
		bothWait  bool
		supported bool
	}{
		"one second, one sample":         {1.0, false, true},
		"just under a second":            {0.999, false, false},
		"just under a second, sustained": {0.999, true, true},
		"no wait time, one sample":       {0, false, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := queued
			p.maxwait = c.maxwait
			first := p
			if !c.bothWait {
				first.wait = 0
			}
			d := DiagnoseConnections([]Observation{quietDB("E1", t0),
				poolerObs("P1", t0, first), quietDB("E2", t0.Add(5*time.Second)),
				poolerObs("P2", t0.Add(5*time.Second), p)})
			h, st := byNode(d, PoolerSaturation)
			if got := st == StatusRoot; got != c.supported {
				t.Fatalf("status %s confidence %v, want supported=%v", st, h.Confidence,
					c.supported)
			}
		})
	}
}

// Backends piled up on a lock hold the pool's server connections: the
// backlog is the root and the pooler's queue contributes.
func TestPooler_LockBacklogIsTheRootAndThePoolerContributes(t *testing.T) {
	backlog := func(id string, at time.Time) Observation {
		return connObs(id, at, 23, group{app: "checkout", state: "active", n: 20,
			waiting: 18})
	}
	t1 := t0.Add(5 * time.Second)
	d := DiagnoseConnections([]Observation{backlog("E1", t0), poolerObs("P1", t0, queued),
		backlog("E2", t1), poolerObs("P2", t1, queued)})
	requireStatus(t, d, BlockedBacklog, StatusRoot)
	requireStatus(t, d, PoolerSaturation, StatusContributing)
}

// Every pool is judged; the subject is the pool with the most waiting
// clients, and the admin console is never a pool.
func TestPooler_PicksTheMostQueuedPool(t *testing.T) {
	small := pgbPool{pooler: "pgb-2", db: "reports", clActive: 5, wait: 3, svActive: 5,
		maxwait: 9}
	admin := pgbPool{pooler: "pgb-1", db: "pgbouncer", clActive: 1, wait: 90, maxwait: 30}
	t1 := t0.Add(5 * time.Second)
	d := DiagnoseConnections([]Observation{quietDB("E1", t0),
		poolerObs("P1", t0, small, queued, admin), quietDB("E2", t1),
		poolerObs("P2", t1, small, queued, admin)})
	root := requireStatus(t, d, PoolerSaturation, StatusRoot)
	if root.Subject != `pool "checkout" at pooler "pgb-1"` {
		t.Fatalf("subject = %q", root.Subject)
	}
}

// An unreachable pooler, or a failed pooler probe, is missing evidence:
// no hypothesis is scored from it, and a reachable pooler still counts.
func TestPooler_UnavailableTelemetryIsMissing(t *testing.T) {
	down := obs("P1", probes.PoolerPools, probes.Row{"pooler": "pgb-1",
		"unreachable": true, "reason": "pooler_timeout"})
	d := DiagnoseConnections([]Observation{quietDB("E1", t0), down,
		quietDB("E2", t0.Add(5*time.Second))})
	if _, st := byNode(d, PoolerSaturation); st != "" {
		t.Fatalf("an unreachable pooler produced a hypothesis (%s)", st)
	}
	if !hasMissing(d, probes.PoolerPools, "pooler_timeout") {
		t.Fatalf("missing = %+v, want pooler_timeout", d.Missing)
	}
	failed := Observation{EvidenceID: "P1", Result: probes.Result{ProbeID: probes.PoolerPools,
		Status: probes.StatusNoPrivilege, Reason: "pooler_auth_failed"}}
	d = DiagnoseConnections([]Observation{quietDB("E1", t0), failed})
	if !hasMissing(d, probes.PoolerPools, "pooler_auth_failed") {
		t.Fatalf("missing = %+v, want pooler_auth_failed", d.Missing)
	}
	t1 := t0.Add(5 * time.Second)
	partial := poolerObs("P2", t1, queued)
	partial.Result.Rows = append(partial.Result.Rows, probes.Row{"pooler": "pgb-2",
		"unreachable": true, "reason": "pooler_unreachable"})
	d = DiagnoseConnections([]Observation{quietDB("E1", t0), poolerObs("P1", t0, queued),
		quietDB("E2", t1), partial})
	requireStatus(t, d, PoolerSaturation, StatusRoot)
	if !hasMissing(d, probes.PoolerPools, "pooler_unreachable") {
		t.Fatalf("the unreachable second pooler is not stated: %+v", d.Missing)
	}
}

func TestGraph_PoolerNode(t *testing.T) {
	n, ok := NodeByID(PoolerSaturation)
	if !ok || n.Family != FamilyConnections || n.Refutation != string(probes.PoolerPools) {
		t.Fatalf("pooler node = %+v (%v)", n, ok)
	}
	if len(n.Amplifies) != 1 || n.Amplifies[0] != BlockedBacklog {
		t.Fatalf("pooler amplifies %v, want only the blocked backlog", n.Amplifies)
	}
	if !probes.IsSignal(probes.PoolerPools) {
		t.Fatal("pooler telemetry must be a signal probe, never a model-callable one")
	}
	for _, bad := range []string{"max_connections", "restart"} {
		if containsFold(n.OperatorStep, "raise "+bad) || containsFold(n.OperatorStep, bad+" the") {
			t.Fatalf("operator step suggests %s: %s", bad, n.OperatorStep)
		}
	}
}

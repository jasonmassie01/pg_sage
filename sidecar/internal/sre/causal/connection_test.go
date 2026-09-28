package causal

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Connection pressure family (AI-SRE-SPEC §6): pool fan-out, blocked
// backlog and connection leak are told apart from two samples of
// connection_saturation (and the lock graph), with the saturation stated
// as an observation (CHECK-04).

var serverStart = time.Date(2026, 9, 27, 6, 0, 0, 0, time.UTC)

type group struct {
	app, state   string
	n, waiting   int64
	otherDB      bool
	serverStarts time.Time
}

func connObs(id string, at time.Time, total int64, gs ...group) Observation {
	var rows []probes.Row
	for _, g := range gs {
		started := g.serverStarts
		if started.IsZero() {
			started = serverStart
		}
		rows = append(rows, probes.Row{"in_current_database": !g.otherDB,
			"application_name": g.app, "client_addr": "10.0.0.5", "state": g.state,
			"backends": g.n, "waiting_on_lock": g.waiting, "max_connections": int64(100),
			"reserved_connections": int64(3), "total_client_backends": total,
			"server_started_at": started})
	}
	o := obs(id, probes.ConnectionSaturation, rows...)
	o.Result.ObservedAt = at
	return o
}

var t0 = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

func byNode(d Diagnosis, id NodeID) (Hypothesis, Status) {
	if d.Root != nil && d.Root.Node == id {
		return *d.Root, StatusRoot
	}
	for _, set := range []struct {
		hs []Hypothesis
		st Status
	}{{d.Contributing, StatusContributing}, {d.Alternatives, StatusAlternative},
		{d.RuledOut, StatusRuledOut}} {
		for _, h := range set.hs {
			if h.Node == id {
				return h, set.st
			}
		}
	}
	return Hypothesis{}, ""
}

func requireStatus(t *testing.T, d Diagnosis, id NodeID, want Status) Hypothesis {
	t.Helper()
	h, got := byNode(d, id)
	if got != want {
		t.Fatalf("%s is %q, want %q; diagnosis %+v", id, got, want, d)
	}
	return h
}

func TestConnections_PoolFanOut(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObs("E1", t0, 36, group{app: "api", state: "idle", n: 30},
			group{app: "api", state: "active", n: 2}),
		connObs("E2", t0.Add(5*time.Second), 36, group{app: "api", state: "idle", n: 30},
			group{app: "api", state: "active", n: 2}),
	})
	root := requireStatus(t, d, PoolFanOut, StatusRoot)
	if root.Subject != `application "api"` || root.Confidence < 0.7 ||
		root.Support[0].EvidenceID != "E2" {
		t.Fatalf("fan-out root = %+v", root)
	}
	requireStatus(t, d, BlockedBacklog, StatusRuledOut)
	leak := requireStatus(t, d, ConnectionLeak, StatusRuledOut)
	if !strings.Contains(leak.Contradict[0].Text, "30 to 30") {
		t.Fatalf("leak contradiction %q must show the flat count", leak.Contradict[0].Text)
	}
	if len(d.Observed) == 0 || !strings.Contains(d.Observed[0].Text, "36 of 97") {
		t.Fatalf("saturation observation = %+v, want 36 of 97 usable", d.Observed)
	}
}

func TestConnections_Leak(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObs("E1", t0, 10, group{app: "worker", state: "idle", n: 5}),
		connObs("E2", t0.Add(5*time.Second), 19, group{app: "worker", state: "idle", n: 14}),
	})
	root := requireStatus(t, d, ConnectionLeak, StatusRoot)
	if !strings.Contains(root.Support[0].Text, "5 to 14") {
		t.Fatalf("leak support %q must show the growth", root.Support[0].Text)
	}
	if _, st := byNode(d, PoolFanOut); st == StatusRoot {
		t.Fatal("fan-out became root while the pool grows")
	}
}

func TestConnections_BlockedBacklog(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObs("E1", t0, 16, group{app: "api", state: "active", n: 12, waiting: 10},
			group{app: "api", state: "idle", n: 2}),
		obs("E2", probes.LockGraph, edge(20, 10, "relation", "AccessExclusiveLock",
			"idle in transaction", 90, false)),
		connObs("E3", t0.Add(5*time.Second), 16,
			group{app: "api", state: "active", n: 12, waiting: 10},
			group{app: "api", state: "idle", n: 2}),
	})
	root := requireStatus(t, d, BlockedBacklog, StatusRoot)
	if root.Subject != "lock waits" || !strings.Contains(root.Support[0].Text, "10") {
		t.Fatalf("backlog root = %+v", root)
	}
	requireStatus(t, d, PoolFanOut, StatusRuledOut)
}

// CHECK-06: ordinary connection use is not an incident.
func TestConnections_HealthyIsInconclusive(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObs("E1", t0, 5, group{app: "api", state: "idle", n: 3},
			group{app: "api", state: "active", n: 1}),
		connObs("E2", t0.Add(5*time.Second), 5, group{app: "api", state: "idle", n: 3},
			group{app: "api", state: "active", n: 1}),
	})
	if d.Conclusive || d.Root != nil || d.Reason == "" {
		t.Fatalf("healthy connections = %+v, want inconclusive with a reason", d)
	}
}

// Nil/empty: no observations at all is inconclusive with the missing
// probe named, never a healthy zero (CHECK-08).
func TestConnections_MissingOrUnavailableEvidence(t *testing.T) {
	for name, input := range map[string][]Observation{
		"none":         nil,
		"no_privilege": {unavailable("E1", probes.ConnectionSaturation, probes.StatusNoPrivilege)},
	} {
		d := DiagnoseConnections(input)
		if d.Conclusive || len(d.Missing) == 0 ||
			d.Missing[0].ProbeID != probes.ConnectionSaturation {
			t.Errorf("%s: diagnosis = %+v, want inconclusive naming the probe", name, d)
		}
	}
}

// CHECK-07: a restart between the samples invalidates the comparison;
// the leak hypothesis cannot be supported or refuted from it.
func TestConnections_RestartBetweenSamplesInvalidatesGrowth(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObs("E1", t0, 10, group{app: "worker", state: "idle", n: 2}),
		connObs("E2", t0.Add(5*time.Second), 19, group{app: "worker", state: "idle",
			n: 14, serverStarts: serverStart.Add(time.Minute)}),
	})
	leak := requireStatus(t, d, ConnectionLeak, StatusAlternative)
	if len(leak.Support)+len(leak.Contradict) != 0 {
		t.Fatalf("leak used an invalid comparison: %+v", leak)
	}
	if !hasMissing(d, probes.ConnectionSaturation, "server_restarted") {
		t.Fatalf("missing = %+v, want server_restarted", d.Missing)
	}
}

// One sample: fan-out is still judged; growth is missing evidence.
func TestConnections_SingleSample(t *testing.T) {
	d := DiagnoseConnections([]Observation{connObs("E1", t0, 30,
		group{app: "api", state: "idle", n: 28})})
	requireStatus(t, d, PoolFanOut, StatusRoot)
	requireStatus(t, d, ConnectionLeak, StatusAlternative)
	if !hasMissing(d, probes.ConnectionSaturation, "second_sample_unavailable") {
		t.Fatalf("missing = %+v", d.Missing)
	}
}

// Attribution uses this database's sessions; another database's idle
// pool counts toward saturation but is not blamed here. The application
// name is untrusted text and appears quoted.
func TestConnections_ScopeAndUntrustedNames(t *testing.T) {
	name := `x"; DROP TABLE t; --`
	d := DiagnoseConnections([]Observation{connObs("E1", t0, 80,
		group{app: "other", state: "idle", n: 60, otherDB: true},
		group{app: name, state: "idle", n: 15})})
	root := requireStatus(t, d, PoolFanOut, StatusRoot)
	if root.Subject != `application "x\"; DROP TABLE t; --"` {
		t.Fatalf("subject = %q, want the quoted current-database application", root.Subject)
	}
	if !strings.Contains(d.Observed[0].Text, "80 of 97") {
		t.Fatalf("saturation = %+v, want cluster-wide 80 of 97", d.Observed)
	}
}

func hasMissing(d Diagnosis, id probes.ID, reason string) bool {
	for _, m := range d.Missing {
		if m.ProbeID == id && m.Reason == reason {
			return true
		}
	}
	return false
}

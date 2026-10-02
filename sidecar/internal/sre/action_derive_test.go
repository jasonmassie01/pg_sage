package sre

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Which concluded investigations propose an evidence-matched cancel, and
// the exact target and baseline they derive from cited evidence only.
// Cancellation is proposed only for an active root blocker with waiters:
// an idle-in-transaction holder has no query to cancel (CHECK-02), a
// prepared transaction has no backend, and short-transaction contention
// is not one backend's doing.

var deriveStart = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func evidenceOf(t *testing.T, res probes.Result) Evidence {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	return Evidence{ID: NewUUID(), ProbeID: string(res.ProbeID), ProbeVersion: "v1",
		CapabilityState: "available", ObservedAt: deriveStart, Payload: raw}
}

func lockEdge(waiter, blocker int64, mode, lockType, state string, age float64,
	waiting bool) probes.Row {
	return probes.Row{"waiter_pid": waiter, "waiter_backend_start": deriveStart.Add(
		time.Duration(waiter) * time.Second), "waiter_state": "active",
		"lock_type": lockType, "requested_mode": mode, "relation": "public.orders",
		"blocker_pid": blocker, "blocker_kind": "backend", "blocker_state": state,
		"blocker_waiting": waiting, "blocker_xact_age_s": age,
		"blocker_backend_start": deriveStart, "blocker_query_id": int64(77)}
}

// activeDDLGraph: pid 5151 runs a statement, an ALTER (pid 20) queues
// behind it, a reader (pid 30) behind the ALTER.
func activeDDLGraph() probes.Result {
	return rows(probes.LockGraph,
		lockEdge(20, 5151, "AccessExclusiveLock", "relation", "active", 12, false),
		lockEdge(30, 20, "AccessShareLock", "relation", "active", 1, true))
}

type deriveCase struct {
	inv  Investigation
	hyps []HypothesisRecord
	ev   []Evidence
}

func lockCase(t *testing.T, node, subject string, graph probes.Result) deriveCase {
	t.Helper()
	g := evidenceOf(t, graph)
	return deriveCase{
		inv: Investigation{ID: NewUUID(), State: StateConcluded, TriggerKind: TriggerLock,
			Summary: Summary{Family: "lock_blocking", Conclusive: true, Root: node}},
		hyps: []HypothesisRecord{{Revision: 1, Ordinal: 1, Family: "lock_blocking",
			Node: node, Subject: subject, Status: HypothesisRoot, Confidence: 0.7,
			Support: []Fact{{EvidenceID: g.ID, Text: "root blocker"}}}},
		ev: []Evidence{g},
	}
}

func (c deriveCase) derive() (cancelCandidate, *Ineligible) {
	return deriveCancel(c.inv, c.hyps, c.ev)
}

func TestDeriveCancelActiveDDLQueueRoot(t *testing.T) {
	c := lockCase(t, "ddl_lock_queue", "pid 5151", activeDDLGraph())
	got, why := c.derive()
	if why != nil {
		t.Fatalf("active DDL-queue root ineligible: %+v", why)
	}
	if got.PID != 5151 || !got.BackendStart.Equal(deriveStart) || got.QueryID != 77 ||
		got.Node != "ddl_lock_queue" || got.Family != "lock_blocking" {
		t.Fatalf("candidate = %+v", got)
	}
	if got.Baseline.Waiting != 2 || len(got.Baseline.Waiters) != 2 ||
		got.Baseline.Waiters[0].PID != 20 ||
		!got.Baseline.Waiters[0].BackendStart.Equal(deriveStart.Add(20*time.Second)) {
		t.Fatalf("baseline = %+v, want both transitive waiters", got.Baseline)
	}
	if len(got.EvidenceIDs) != 1 || got.EvidenceIDs[0] != c.ev[0].ID {
		t.Fatalf("evidence ids = %v, want the cited lock graph", got.EvidenceIDs)
	}
}

func TestDeriveCancelRefusesIneligibleRoots(t *testing.T) {
	idle := lockCase(t, "idle_in_tx_holder", "pid 5151", rows(probes.LockGraph,
		lockEdge(20, 5151, "AccessExclusiveLock", "relation", "idle in transaction", 90,
			false)))
	prepared := lockCase(t, "prepared_xact_holder", "prepared transaction",
		rows(probes.LockGraph, func() probes.Row {
			r := lockEdge(20, 0, "AccessExclusiveLock", "relation", "", 0, false)
			r["blocker_kind"] = "prepared_xact"
			return r
		}()))
	hotShort := lockCase(t, "hot_row_contention", "pid 5151", rows(probes.LockGraph,
		lockEdge(20, 5151, "ShareLock", "transactionid", "active", 2, false),
		lockEdge(21, 5151, "ShareLock", "transactionid", "active", 2, false)))
	idleRootDDL := lockCase(t, "ddl_lock_queue", "pid 5151", rows(probes.LockGraph,
		lockEdge(20, 5151, "AccessExclusiveLock", "relation", "idle in transaction", 90,
			false)))
	for name, tc := range map[string]struct {
		c    deriveCase
		want ActionReason
	}{
		"idle in transaction (CHECK-02)": {idle, ReasonIdleInTransaction},
		"idle head of a DDL queue":       {idleRootDDL, ReasonIdleInTransaction},
		"prepared transaction":           {prepared, ReasonRootNotBackend},
		"short hot-row contention":       {hotShort, ReasonShortContention},
	} {
		if _, why := tc.c.derive(); why == nil || why.Reason != tc.want {
			t.Errorf("%s: ineligible = %+v, want %s", name, why, tc.want)
		}
	}
}

func TestDeriveCancelLongHotRowHolderIsEligible(t *testing.T) {
	c := lockCase(t, "hot_row_contention", "pid 5151", rows(probes.LockGraph,
		lockEdge(20, 5151, "ShareLock", "transactionid", "active", 60, false),
		lockEdge(21, 5151, "ShareLock", "transactionid", "active", 60, false)))
	got, why := c.derive()
	if why != nil || got.PID != 5151 || got.Baseline.Waiting != 2 {
		t.Fatalf("a 60 s row-lock holder = %+v, %+v; want eligible", got, why)
	}
	c = lockCase(t, "hot_row_contention", "pid 5151", rows(probes.LockGraph,
		lockEdge(20, 5151, "ShareLock", "transactionid", "active", 59.9, false),
		lockEdge(21, 5151, "ShareLock", "transactionid", "active", 59.9, false)))
	if _, why := c.derive(); why == nil || why.Reason != ReasonShortContention {
		t.Fatalf("a 59.9 s holder = %+v, want short contention", why)
	}
}

func TestDeriveCancelNeedsAConcludedSupportedFamily(t *testing.T) {
	c := lockCase(t, "ddl_lock_queue", "pid 5151", activeDDLGraph())
	c.inv.State = StateInconclusive
	if _, why := c.derive(); why == nil || why.Reason != ReasonNotConcluded {
		t.Fatalf("inconclusive = %+v, want not_concluded", why)
	}
	c = lockCase(t, "ddl_lock_queue", "pid 5151", activeDDLGraph())
	c.inv.TriggerKind = TriggerWAL
	if _, why := c.derive(); why == nil || why.Reason != ReasonUnsupportedFamily {
		t.Fatalf("WAL family = %+v, want unsupported_family", why)
	}
	c = lockCase(t, "ddl_lock_queue", "pid 5151", activeDDLGraph())
	c.hyps[0].Status = HypothesisUnproven
	if _, why := c.derive(); why == nil || why.Reason != ReasonNotConcluded {
		t.Fatalf("no root hypothesis = %+v, want not_concluded", why)
	}
}

func connectionCase(t *testing.T, node string, citeGraph bool) deriveCase {
	t.Helper()
	g := evidenceOf(t, activeDDLGraph())
	conn := evidenceOf(t, rows(probes.ConnectionSaturation,
		connRow("api", "active", 12, 40)))
	support := []Fact{{EvidenceID: conn.ID, Text: "12 backends wait on locks"}}
	if citeGraph {
		support = append(support, Fact{EvidenceID: g.ID,
			Text: "the lock graph shows root blocker pid 5151"})
	}
	return deriveCase{
		inv: Investigation{ID: NewUUID(), State: StateConcluded,
			TriggerKind: TriggerConnections,
			Summary:     Summary{Family: "connection_pressure", Conclusive: true, Root: node}},
		hyps: []HypothesisRecord{{Revision: 1, Ordinal: 1, Family: "connection_pressure",
			Node: node, Subject: "lock waits", Status: HypothesisRoot, Confidence: 0.8,
			Support: support}},
		ev: []Evidence{conn, g},
	}
}

func TestDeriveCancelConnectionBacklogWithASingleRoot(t *testing.T) {
	got, why := connectionCase(t, "blocked_backlog", true).derive()
	if why != nil || got.PID != 5151 || got.Family != "connection_pressure" ||
		got.Node != "blocked_backlog" || len(got.EvidenceIDs) != 2 {
		t.Fatalf("backlog with a root blocker = %+v, %+v", got, why)
	}
	if _, why := connectionCase(t, "blocked_backlog", false).derive(); why == nil ||
		why.Reason != ReasonLockEvidenceMissing {
		t.Fatalf("backlog without a cited lock graph = %+v, want lock_evidence_missing", why)
	}
	if _, why := connectionCase(t, "pool_fan_out", true).derive(); why == nil ||
		why.Reason != ReasonUnsupportedRoot {
		t.Fatalf("pool fan-out = %+v, want unsupported_root", why)
	}
}

func TestDeriveCancelEvidenceProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		c    func() deriveCase
		want ActionReason
	}{
		"graph not cited": {func() deriveCase {
			c := lockCase(t, "ddl_lock_queue", "pid 5151", activeDDLGraph())
			c.hyps[0].Support = []Fact{{EvidenceID: NewUUID(), Text: "x"}}
			return c
		}, ReasonLockEvidenceMissing},
		"graph unusable": {func() deriveCase {
			return lockCase(t, "ddl_lock_queue", "pid 5151", probes.Result{
				ProbeID: probes.LockGraph, Status: probes.StatusNoPrivilege})
		}, ReasonLockEvidenceMissing},
		"graph empty": {func() deriveCase {
			return lockCase(t, "ddl_lock_queue", "pid 5151", rows(probes.LockGraph))
		}, ReasonNoWaiters},
		"malformed payload": {func() deriveCase {
			c := lockCase(t, "ddl_lock_queue", "pid 5151", activeDDLGraph())
			c.ev[0].Payload = []byte(`{"probe_id":"lock_graph","rows":[{"waiter_pid":"x"`)
			return c
		}, ReasonLockEvidenceMissing},
		"no backend_start": {func() deriveCase {
			r := lockEdge(20, 5151, "AccessExclusiveLock", "relation", "active", 12, false)
			delete(r, "blocker_backend_start")
			return lockCase(t, "ddl_lock_queue", "pid 5151", rows(probes.LockGraph, r))
		}, ReasonLockEvidenceMissing},
		"subject mismatch": {func() deriveCase {
			return lockCase(t, "ddl_lock_queue", "pid 1", activeDDLGraph())
		}, ReasonEvidenceInconsistent},
	} {
		if _, why := tc.c().derive(); why == nil || why.Reason != tc.want {
			t.Errorf("%s: ineligible = %+v, want %s", name, why, tc.want)
		}
	}
}

// The root is the non-waiting blocker with the most transitive waiters;
// ties go to the lower pid (the causal graph's rule).
func TestDeriveCancelPicksTheGraphRoot(t *testing.T) {
	graph := rows(probes.LockGraph,
		lockEdge(20, 7000, "AccessExclusiveLock", "relation", "active", 12, false),
		lockEdge(21, 5151, "AccessExclusiveLock", "relation", "active", 12, false),
		lockEdge(22, 5151, "AccessShareLock", "relation", "active", 12, false))
	got, why := lockCase(t, "ddl_lock_queue", "pid 5151", graph).derive()
	if why != nil || got.PID != 5151 || got.Baseline.Waiting != 2 {
		t.Fatalf("root = %+v, %+v; want pid 5151 with 2 waiters", got, why)
	}
	tie := rows(probes.LockGraph,
		lockEdge(20, 7000, "AccessExclusiveLock", "relation", "active", 12, false),
		lockEdge(21, 5151, "AccessExclusiveLock", "relation", "active", 12, false))
	got, why = lockCase(t, "ddl_lock_queue", fmt.Sprintf("pid %d", 5151), tie).derive()
	if why != nil || got.PID != 5151 {
		t.Fatalf("tie = %+v, %+v; want the lower pid", got, why)
	}
}

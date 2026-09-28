package causal

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Causal graph v1, lock blocking family (AI-SRE-SPEC §6): the matcher
// distinguishes an idle-in-transaction holder, DDL queued behind a long
// transaction (lock-queue amplification), hot-row contention and a
// prepared-transaction holder from structured evidence, with explicit
// ruled-out alternatives. It is deterministic and never calls the LLM.

func edge(waiter, blocker int, lockType, mode, state string, age float64,
	blockerWaiting bool) probes.Row {
	kind := "backend"
	if blocker == 0 {
		kind = "prepared_xact"
	}
	var ageV any = age
	if math.IsNaN(age) {
		ageV = nil
	}
	return probes.Row{"waiter_pid": int64(waiter), "lock_type": lockType,
		"requested_mode": mode, "relation": "public.orders",
		"blocker_pid": int64(blocker), "blocker_kind": kind,
		"blocker_state": state, "blocker_waiting": blockerWaiting,
		"blocker_xact_age_s":    ageV,
		"blocker_backend_start": time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)}
}

func obs(id string, probe probes.ID, rows ...probes.Row) Observation {
	st := probes.StatusOK
	if len(rows) == 0 {
		st = probes.StatusEmpty
	}
	return Observation{EvidenceID: id, Result: probes.Result{ProbeID: probe,
		Version: "v1", Status: st, Rows: rows, ObservedAt: time.Now()}}
}

func unavailable(id string, probe probes.ID, st probes.Status) Observation {
	return Observation{EvidenceID: id, Result: probes.Result{ProbeID: probe,
		Version: "v1", Status: st, Reason: "insufficient_privilege"}}
}

// idleChain: holder 10 idle in transaction for 75 s; ALTER (20) waits
// for ACCESS EXCLUSIVE behind it; a reader (30) waits behind the ALTER.
func idleChain(holderState string, age float64) []Observation {
	return []Observation{
		obs("P1", probes.LockGraph,
			edge(20, 10, "relation", "AccessExclusiveLock", holderState, age, false),
			edge(30, 20, "relation", "AccessShareLock", "active", 1, true)),
		obs("P2", probes.PreparedXacts),
	}
}

func findHyp(d Diagnosis, id NodeID) (Hypothesis, bool) {
	all := append([]Hypothesis{}, d.Contributing...)
	all = append(all, d.Alternatives...)
	all = append(all, d.RuledOut...)
	if d.Root != nil {
		all = append(all, *d.Root)
	}
	for _, h := range all {
		if h.Node == id {
			return h, true
		}
	}
	return Hypothesis{}, false
}

func requireRoot(t *testing.T, d Diagnosis, id NodeID, conf float64) Hypothesis {
	t.Helper()
	if d.Root == nil {
		t.Fatalf("no root cause; reason %q; diagnosis %+v", d.Reason, d)
	}
	if d.Root.Node != id || math.Abs(d.Root.Confidence-conf) > 1e-9 ||
		d.Root.Status != StatusRoot || !d.Conclusive {
		t.Fatalf("root = %s %.2f %s, want %s %.2f", d.Root.Node,
			d.Root.Confidence, d.Root.Status, id, conf)
	}
	return *d.Root
}

func requireRuledOut(t *testing.T, d Diagnosis, id NodeID, reason string) {
	t.Helper()
	h, ok := findHyp(d, id)
	if !ok || h.Status != StatusRuledOut || len(h.Contradict) == 0 ||
		!strings.Contains(h.Contradict[0].Text, reason) {
		t.Fatalf("%s = %+v, want ruled out with %q", id, h, reason)
	}
	for _, r := range d.RuledOut {
		if r.Node == id {
			return
		}
	}
	t.Fatalf("%s missing from RuledOut", id)
}

func citesOnly(t *testing.T, h Hypothesis, ids ...string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, id := range ids {
		allowed[id] = true
	}
	for _, f := range append(append([]Fact{}, h.Support...), h.Contradict...) {
		if !allowed[f.EvidenceID] {
			t.Fatalf("%s cites %q, want one of %v", h.Node, f.EvidenceID, ids)
		}
	}
}

func TestDiagnoseLock_IdleHolderWithQueuedDDL(t *testing.T) {
	d := DiagnoseLock(idleChain("idle in transaction", 75), nil)
	root := requireRoot(t, d, IdleInTxHolder, 0.85)
	if root.Subject != "pid 10" || root.RefutationProbe != string(probes.LockGraph) {
		t.Fatalf("root subject/refutation = %q/%q", root.Subject, root.RefutationProbe)
	}
	if len(root.Support) != 3 || !strings.Contains(root.Support[0].Text,
		"idle in transaction") || !strings.Contains(root.Support[2].Text, "75") {
		t.Fatalf("root support = %+v", root.Support)
	}
	citesOnly(t, root, "P1")
	if len(d.Contributing) != 1 || d.Contributing[0].Node != DDLLockQueue ||
		d.Contributing[0].Status != StatusContributing ||
		math.Abs(d.Contributing[0].Confidence-0.85) > 1e-9 {
		t.Fatalf("contributing = %+v, want the queued DDL as amplification",
			d.Contributing)
	}
	requireRuledOut(t, d, HotRowContention, "relation locks")
	requireRuledOut(t, d, PreparedXactHolder, "no prepared transactions")
	if d.Family != FamilyLockBlocking || d.GraphVersion != GraphVersion {
		t.Fatalf("family/version = %s/%s", d.Family, d.GraphVersion)
	}
}

func TestDiagnoseLock_DDLBehindActiveLongTransaction(t *testing.T) {
	d := DiagnoseLock(idleChain("active", 90), nil)
	requireRoot(t, d, DDLLockQueue, 0.85)
	requireRuledOut(t, d, IdleInTxHolder, "is active")
	requireRuledOut(t, d, HotRowContention, "relation locks")
	if len(d.Contributing) != 0 {
		t.Fatalf("contributing = %+v, want none", d.Contributing)
	}
}

func TestDiagnoseLock_DDLBehindShortTransactionScoresLower(t *testing.T) {
	d := DiagnoseLock(idleChain("active", 5), nil)
	requireRoot(t, d, DDLLockQueue, 0.70)
}

func TestDiagnoseLock_HotRowContention(t *testing.T) {
	o := []Observation{obs("P1", probes.LockGraph,
		edge(20, 10, "transactionid", "ShareLock", "active", 2, false),
		edge(30, 20, "tuple", "ExclusiveLock", "active", 1, true),
		edge(40, 20, "tuple", "ExclusiveLock", "active", 1, true)),
		obs("P2", probes.PreparedXacts)}
	d := DiagnoseLock(o, nil)
	requireRoot(t, d, HotRowContention, 0.70)
	requireRuledOut(t, d, IdleInTxHolder, "is active")
	requireRuledOut(t, d, DDLLockQueue, "ACCESS EXCLUSIVE")
}

func TestDiagnoseLock_PreparedTransactionHolder(t *testing.T) {
	o := []Observation{
		obs("P1", probes.LockGraph,
			edge(20, 0, "relation", "AccessShareLock", "", math.NaN(), false)),
		obs("P2", probes.PreparedXacts, probes.Row{"gid_hash": "ab",
			"prepared_age_s": 600.0, "xid_age": int64(40)}),
	}
	d := DiagnoseLock(o, nil)
	root := requireRoot(t, d, PreparedXactHolder, 0.85)
	citesOnly(t, root, "P1", "P2")
	requireRuledOut(t, d, IdleInTxHolder, "prepared transaction")
}

// With lock_graph unavailable the matcher falls back to the root-blocker
// identity M0 records on the incident, reports the missing probe, and
// leaves the mechanisms it cannot test unproven (never ruled out).
func TestDiagnoseLock_FallsBackToIncidentBlockerEvidence(t *testing.T) {
	o := []Observation{unavailable("P1", probes.LockGraph, probes.StatusNoPrivilege)}
	blockers := []BlockerEvidence{{EvidenceID: "E2", PID: 10,
		State: "idle in transaction", TotalBlocked: 2, ChainDepth: 2}}
	d := DiagnoseLock(o, blockers)
	root := requireRoot(t, d, IdleInTxHolder, 0.70)
	citesOnly(t, root, "E2")
	for _, id := range []NodeID{DDLLockQueue, HotRowContention} {
		h, ok := findHyp(d, id)
		if !ok || h.Status != StatusAlternative {
			t.Fatalf("%s = %+v, want unproven alternative", id, h)
		}
	}
	if len(d.Missing) == 0 || d.Missing[0].ProbeID != probes.LockGraph ||
		d.Missing[0].Status != probes.StatusNoPrivilege {
		t.Fatalf("missing = %+v, want lock_graph no_privilege", d.Missing)
	}
}

func TestDiagnoseLock_ActiveHolderFromIncidentEvidenceOnly(t *testing.T) {
	blockers := []BlockerEvidence{{EvidenceID: "E2", PID: 10, State: "active",
		TotalBlocked: 3}}
	d := DiagnoseLock(nil, blockers)
	if d.Conclusive || d.Root != nil {
		t.Fatalf("an active holder with no lock graph must be inconclusive: %+v",
			d.Root)
	}
	requireRuledOut(t, d, IdleInTxHolder, "is active")
	if !strings.Contains(d.Reason, "0.50") {
		t.Fatalf("reason = %q, want the confidence threshold named", d.Reason)
	}
}

func TestDiagnoseLock_NoEvidenceIsInconclusive(t *testing.T) {
	d := DiagnoseLock(nil, nil)
	if d.Conclusive || d.Root != nil || d.Reason == "" {
		t.Fatalf("diagnosis = %+v, want inconclusive with a reason", d)
	}
}

// Fresh probe evidence wins: a cleared chain is not diagnosed from the
// incident's older blocker identity.
func TestDiagnoseLock_ClearedChainIsInconclusive(t *testing.T) {
	blockers := []BlockerEvidence{{EvidenceID: "E2", PID: 10,
		State: "idle in transaction", TotalBlocked: 2}}
	d := DiagnoseLock([]Observation{obs("P1", probes.LockGraph)}, blockers)
	if d.Conclusive || !strings.Contains(d.Reason, "no lock waits") {
		t.Fatalf("diagnosis = %+v, want inconclusive: no lock waits", d)
	}
}

func TestDiagnoseLock_SubjectIsRootWithMostWaiters(t *testing.T) {
	o := []Observation{obs("P1", probes.LockGraph,
		edge(11, 10, "relation", "AccessShareLock", "idle in transaction", 80, false),
		edge(41, 40, "relation", "AccessExclusiveLock", "idle in transaction", 80, false),
		edge(42, 41, "relation", "AccessShareLock", "active", 1, true),
		edge(43, 41, "relation", "AccessShareLock", "active", 1, true))}
	d := DiagnoseLock(o, nil)
	if d.Root == nil || d.Root.Subject != "pid 40" {
		t.Fatalf("root = %+v, want the holder blocking 3 sessions", d.Root)
	}
}

func TestDiagnoseLock_IsDeterministicAndOrderInsensitive(t *testing.T) {
	a := idleChain("idle in transaction", 75)
	b := idleChain("idle in transaction", 75)
	rows := b[0].Result.Rows
	rows[0], rows[1] = rows[1], rows[0]
	da, db := DiagnoseLock(a, nil), DiagnoseLock(b, nil)
	if !reflect.DeepEqual(da, db) {
		t.Fatalf("edge order changed the diagnosis:\n%+v\n%+v", da, db)
	}
}

func TestDiagnoseLock_WaitCycleHasNoRoot(t *testing.T) {
	o := []Observation{obs("P1", probes.LockGraph,
		edge(10, 20, "transactionid", "ShareLock", "active", 1, true),
		edge(20, 10, "transactionid", "ShareLock", "active", 1, true))}
	d := DiagnoseLock(o, nil)
	if d.Conclusive || d.Root != nil {
		t.Fatalf("a wait cycle has no root blocker: %+v", d.Root)
	}
}

// CHECK-37: every hypothesis names a refutation probe or none_available.
func TestDiagnoseLock_EveryHypothesisHasRefutation(t *testing.T) {
	for _, o := range [][]Observation{idleChain("idle in transaction", 75),
		idleChain("active", 90), nil} {
		d := DiagnoseLock(o, nil)
		all := append(append(append([]Hypothesis{}, d.Contributing...),
			d.Alternatives...), d.RuledOut...)
		if d.Root != nil {
			all = append(all, *d.Root)
		}
		for _, h := range all {
			if h.RefutationProbe == "" {
				t.Fatalf("%s has no refutation probe", h.Node)
			}
		}
		if len(all) != 4 {
			t.Fatalf("diagnosis lists %d lock hypotheses, want all 4", len(all))
		}
	}
}

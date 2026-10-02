package causal

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// LWLock contention family: sampled wait events of active backends,
// classified by the LWLock they wait on (lock manager, subtransaction
// SLRU, multixact SLRU, WAL write, buffer mapping/content). A class is
// supported when enough backends wait on it in at least half of the
// samples; a class nobody waited on is ruled out; transient waits stay
// unproven. Wait event names follow PostgreSQL 14 to 18.

type wait struct {
	typ, event string
	qid        int64 // 0 means unknown
	n          int64
}

func waitSample(ev string, at time.Duration, active int64, ws ...wait) Observation {
	var rows []probes.Row
	for _, w := range ws {
		var qid any
		if w.qid != 0 {
			qid = w.qid
		}
		rows = append(rows, probes.Row{"wait_event_type": w.typ, "wait_event": w.event,
			"query_id": qid, "in_current_database": true, "backends": w.n,
			"active_backends": active})
	}
	return obsAt(ev, probes.LWLockWaits, at, rows...)
}

func samples(ws ...[]wait) []Observation {
	var out []Observation
	for i, w := range ws {
		out = append(out, waitSample("L"+string(rune('1'+i)), time.Duration(i)*3*time.Second,
			30, w...))
	}
	return out
}

func walWrite(n int64, qid int64) wait { return wait{"LWLock", "WALWrite", qid, n} }
func lockMgr(n int64) wait             { return wait{"LWLock", "LockManager", 0, n} }
func cpu(n int64) wait                 { return wait{"CPU", "", 0, n} }

func TestLWLock_WALWriteContentionAttributedToAQuery(t *testing.T) {
	hot := []wait{walWrite(15, 9), walWrite(5, 0), {"IO", "WALSync", 9, 1}, cpu(9)}
	d := DiagnoseLWLock(samples(hot, hot, hot, hot))
	if d.Family != FamilyLWLock {
		t.Fatalf("family = %s", d.Family)
	}
	wantRoot(t, d, WALWriteContention)
	if d.Root.Subject != "queryid 9" {
		t.Fatalf("subject = %q", d.Root.Subject)
	}
	wantFact(t, d.Root.Support, "L4", "WALWrite")
	wantFact(t, d.Root.Support, "L4", "4 of 4 samples")
	for _, id := range []NodeID{LockManagerContention, SubtransSLRUContention,
		MultiXactSLRUContention, BufferContention} {
		wantStatus(t, d, id, StatusRuledOut)
	}
	if len(d.Observed) == 0 {
		t.Fatal("no observed summary of the samples")
	}
	everyHypothesisCarriesRefutation(t, d)
}

func TestLWLock_LockManagerContention(t *testing.T) {
	d := DiagnoseLWLock(samples([]wait{lockMgr(12), cpu(18)}, []wait{lockMgr(11), cpu(19)},
		[]wait{lockMgr(2), cpu(28)}, []wait{lockMgr(9), cpu(21)}))
	wantRoot(t, d, LockManagerContention)
	if d.Root.Subject != "LWLock LockManager" {
		t.Fatalf("subject = %q (no query id is known)", d.Root.Subject)
	}
}

// Decoy: a couple of transient WAL write waits.
func TestLWLock_TransientWaitsAreInconclusive(t *testing.T) {
	d := DiagnoseLWLock(samples([]wait{walWrite(2, 9), cpu(3)}, []wait{cpu(4)},
		[]wait{cpu(4)}, []wait{cpu(5)}))
	wantInconclusive(t, d)
	wantStatus(t, d, WALWriteContention, StatusAlternative)
	wantStatus(t, d, LockManagerContention, StatusRuledOut)
}

func TestLWLock_BusyWithoutLWLockWaitsRulesEverythingOut(t *testing.T) {
	d := DiagnoseLWLock(samples([]wait{cpu(30)}, []wait{cpu(30)}, nil, []wait{cpu(28)}))
	wantInconclusive(t, d)
	for _, id := range []NodeID{LockManagerContention, SubtransSLRUContention,
		MultiXactSLRUContention, WALWriteContention, BufferContention} {
		wantStatus(t, d, id, StatusRuledOut)
	}
}

// The dominant class wins when two are hot.
func TestLWLock_DominantClassIsRoot(t *testing.T) {
	both := []wait{walWrite(10, 0), lockMgr(6), cpu(14)}
	d := DiagnoseLWLock(samples(both, both, both, both))
	wantRoot(t, d, WALWriteContention)
	wantStatus(t, d, LockManagerContention, StatusAlternative)
}

// Boundary: 4 waiters in half of the samples is sustained contention;
// 3 waiters, or 4 in fewer than half, is not.
func TestLWLock_SustainedBoundary(t *testing.T) {
	hot, cold := []wait{lockMgr(4)}, []wait{cpu(4)}
	if d := DiagnoseLWLock(samples(hot, hot, cold, cold)); statusOf(d,
		LockManagerContention) != StatusRoot {
		t.Fatalf("4 waiters in 2 of 4 samples: %s", statusOf(d, LockManagerContention))
	}
	three := []wait{lockMgr(3)}
	if d := DiagnoseLWLock(samples(three, three, three, three)); statusOf(d,
		LockManagerContention) != StatusAlternative {
		t.Fatalf("3 waiters: %s", statusOf(d, LockManagerContention))
	}
	if d := DiagnoseLWLock(samples(hot, cold, cold, cold)); statusOf(d,
		LockManagerContention) != StatusAlternative {
		t.Fatalf("4 waiters in 1 of 4 samples: %s", statusOf(d, LockManagerContention))
	}
}

// One sample cannot show sustained contention.
func TestLWLock_SingleSampleIsNotEnough(t *testing.T) {
	d := DiagnoseLWLock(samples([]wait{lockMgr(30)}))
	wantInconclusive(t, d)
	wantMissing(t, d, probes.LWLockWaits, "too_few_samples")
}

func TestLWLock_UnavailableAndMissing(t *testing.T) {
	d := DiagnoseLWLock([]Observation{failedObs("L1", probes.LWLockWaits,
		probes.StatusError, "statement_timeout")})
	wantInconclusive(t, d)
	wantMissing(t, d, probes.LWLockWaits, "statement_timeout")
	none := DiagnoseLWLock(nil)
	wantMissing(t, none, probes.LWLockWaits, "not_collected")
}

// Waits on LWLocks no node models are reported, never forced into a class.
func TestLWLock_UnmodeledLWLockIsObservedNotClassified(t *testing.T) {
	other := []wait{{"LWLock", "ProcArray", 0, 12}, cpu(10)}
	d := DiagnoseLWLock(samples(other, other, other))
	wantInconclusive(t, d)
	wantFact(t, d.Observed, "L3", "ProcArray")
}

func TestLWLockClass_NamesAcrossVersions(t *testing.T) {
	cases := map[string]NodeID{"LockManager": LockManagerContention,
		"lock_manager": LockManagerContention, "LockFastPath": LockManagerContention,
		"SubtransSLRU": SubtransSLRUContention, "SubtransBuffer": SubtransSLRUContention,
		"SubtransControlLock":   SubtransSLRUContention,
		"MultiXactOffsetSLRU":   MultiXactSLRUContention,
		"MultiXactMemberBuffer": MultiXactSLRUContention,
		"WALWrite":              WALWriteContention, "WALInsert": WALWriteContention,
		"WALBufMapping": WALWriteContention, "BufferMapping": BufferContention,
		"BufferContent": BufferContention, "buffer_content": BufferContention}
	for event, want := range cases {
		if got, ok := LWLockClass(event); !ok || got != want {
			t.Errorf("LWLockClass(%q) = %s %v, want %s", event, got, ok, want)
		}
	}
	for _, event := range []string{"ProcArray", "", "PgSleep", "walwrite"} {
		if got, ok := LWLockClass(event); ok {
			t.Errorf("LWLockClass(%q) = %s, want unclassified", event, got)
		}
	}
}

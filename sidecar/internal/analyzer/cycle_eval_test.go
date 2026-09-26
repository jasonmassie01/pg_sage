package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// G2-B02: failed marks win over success, emitted categories count as
// evaluated, and a nil tracker is a safe no-op outside a cycle.
func TestCycleEval_Resolvable(t *testing.T) {
	e := newCycleEval()
	e.evaluated("table_bloat", "xid_wraparound", "lock_chain")
	e.fail("lock_chain", "missing_index")
	got := e.resolvable([]Finding{{Category: "query_tuning"}, {Category: "missing_index"}})
	want := map[string]bool{"table_bloat": true, "xid_wraparound": true, "query_tuning": true}
	if len(got) != len(want) {
		t.Fatalf("resolvable = %v, want %v", got, want)
	}
	for c := range want {
		if !got[c] {
			t.Fatalf("resolvable = %v, missing %s", got, c)
		}
	}
	var nilEval *cycleEval
	nilEval.evaluated("x")
	nilEval.fail("y")
}

// G2-B02: a rule whose input is missing (collection failure) or that was
// skipped for a stats reset must not claim its categories.
func TestRunSnapshotRules_MarksMissingInputFailed(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	cur := &collector.Snapshot{Tables: []collector.TableStats{{SchemaName: "public", RelName: "t"}}}
	a.runSnapshotRules(cur, nil, true)
	got := a.eval.resolvable(nil)
	if !got["table_bloat"] {
		t.Fatalf("table_bloat not evaluated with tables present: %v", got)
	}
	for _, c := range []string{"unused_index", "slow_query", "replication_lag",
		"checkpoint_pressure", "high_total_time"} {
		if got[c] {
			t.Fatalf("%s claimed without input: %v", c, got)
		}
	}
}

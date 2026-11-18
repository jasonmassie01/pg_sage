package changeplan

import (
	"slices"
	"strings"
	"testing"
)

// plan-02c review: a revert is read back against the object the action
// changed, not only the rollback's own target. A rollback that names
// another index or setting never reads back as landed.

// No concurrent access: rollbackMismatch and touchedObject are pure.
func TestRollbackMismatch(t *testing.T) {
	for _, c := range []struct {
		name, forward, rollback string
		mismatch                bool
	}{
		{"create then drop it", "CREATE INDEX CONCURRENTLY idx_a ON public.t (a)",
			"DROP INDEX CONCURRENTLY public.idx_a", false},
		{"quoted rollback name", "CREATE INDEX CONCURRENTLY idx_a ON app.t (a)",
			`DROP INDEX CONCURRENTLY IF EXISTS "app"."idx_a"`, false},
		{"drop then re-create", "DROP INDEX CONCURRENTLY app.idx_a",
			"CREATE INDEX CONCURRENTLY idx_a ON app.t USING btree (a)", false},
		{"setting", "ALTER SYSTEM SET random_page_cost = 1.1",
			"ALTER SYSTEM RESET random_page_cost", false},
		{"forward names nothing known", "ANALYZE public.t", "", false},
		{"unnamed index", "CREATE INDEX CONCURRENTLY ON public.t (a)",
			"DROP INDEX CONCURRENTLY public.t_a_idx", false},
		{"drops another index", "CREATE INDEX CONCURRENTLY idx_a ON public.t (a)",
			"DROP INDEX CONCURRENTLY public.idx_b", true},
		{"same name, other schema", "CREATE INDEX CONCURRENTLY idx_a ON app.t (a)",
			"DROP INDEX CONCURRENTLY public.idx_a", true},
		{"case-sensitive name differs", `CREATE INDEX CONCURRENTLY "Idx_A" ON public.t (a)`,
			"DROP INDEX CONCURRENTLY public.idx_a", true},
		{"re-creates another index", "DROP INDEX CONCURRENTLY public.idx_a",
			"CREATE INDEX CONCURRENTLY idx_b ON public.t (a)", true},
		{"resets another setting", "ALTER SYSTEM SET random_page_cost = 1.1",
			"ALTER SYSTEM RESET seq_page_cost", true},
		{"empty rollback", "CREATE INDEX CONCURRENTLY idx_a ON public.t (a)", "", true},
		{"multi-object rollback", "CREATE INDEX CONCURRENTLY idx_a ON public.t (a)",
			"DROP INDEX public.idx_a, public.idx_b", true},
	} {
		why := rollbackMismatch(c.forward, c.rollback)
		if (why != "") != c.mismatch {
			t.Errorf("%s: rollbackMismatch(%q, %q) = %q, want mismatch %v", c.name,
				c.forward, c.rollback, why, c.mismatch)
		}
		if c.mismatch && !strings.Contains(why, "does not undo") {
			t.Errorf("%s: reason %q, want it to say the rollback does not undo the action",
				c.name, why)
		}
	}
}

// A step whose rollback drops another index than the one it built: the
// rollback runs, but the revert does not read back as landed, so the step
// fails and the plan halts; it is never recorded reverted.
func TestRevertTo_RollbackOfAnotherObjectFails(t *testing.T) {
	f := newEngineFixture(t, "rollback_mismatch")
	in := walkChain(2, 2)
	in.Steps[0].RollbackSQL = "DROP INDEX CONCURRENTLY public.idx_other"
	p := f.approved(in)
	got := f.runVerdicts(p.ID, map[int]string{2: "regressed"}, 30)
	if want := []StepState{StateFailed, StateReverted}; !slices.Equal(states(got), want) ||
		got.Status != StatusHalted {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	r := got.Steps[0].Runtime.StateReason
	if !strings.Contains(r, "does not undo") || !strings.Contains(r, "idx_101_1") {
		t.Fatalf("step 1 reason %q, want the rollback named as not undoing idx_101_1", r)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{2, 1}) {
		t.Fatalf("rolled back %v, want [2 1]", rb)
	}
}

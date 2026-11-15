package analyzer

import (
	"context"
	"testing"
)

// fleetpol-02a on real PostgreSQL: an out-of-cycle run persists and
// resolves like a cycle, for its own categories only.

func outOfCycle(f []Finding, evaluated, incomplete []string) OutOfCycleRun {
	return func(context.Context) (OutOfCycle, bool, error) {
		return OutOfCycle{Findings: f, Evaluated: evaluated, Incomplete: incomplete}, true, nil
	}
}

func TestRunOutOfCycle_OpensResolvesAndReopens(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_oc_exposed", "public.leads", "critical")
	cat := []string{f.Category}
	if err := a.RunOutOfCycle(ctx, outOfCycle([]Finding{f}, cat, nil)); err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); got != 1 {
		t.Fatalf("open rows = %d, want 1", got)
	}
	if err := a.RunOutOfCycle(ctx, outOfCycle(nil, cat, nil)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "resolved"); got != 1 {
		t.Fatalf("resolved rows = %d, want 1", got)
	}
	if err := a.RunOutOfCycle(ctx, outOfCycle([]Finding{f}, cat, nil)); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); got != 1 {
		t.Fatalf("open rows after re-exposure = %d, want 1", got)
	}
}

// An out-of-cycle run never resolves another category's findings, nor
// those of a category it reports as incomplete.
func TestRunOutOfCycle_LeavesOtherAndIncompleteCategories(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	other := lifecycleFinding("test_phase2_oc_other", "public.orders", "warning")
	partial := lifecycleFinding("test_phase2_oc_partial", "public.big", "warning")
	a.finalizeCycle(ctx, []Finding{other, partial},
		map[string]bool{other.Category: true, partial.Category: true})
	run := outOfCycle(nil, []string{"test_phase2_oc_exposed", partial.Category},
		[]string{partial.Category})
	if err := a.RunOutOfCycle(ctx, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, f := range []Finding{other, partial} {
		if got := countFindingRows(t, pool, f.Category, f.ObjectIdentifier, "open"); got != 1 {
			t.Fatalf("%s open rows = %d, want 1 (left untouched)", f.Category, got)
		}
	}
}

// Two runs reporting the same finding keep one row.
func TestRunOutOfCycle_RepeatedRunKeepsOneRow(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_phase2_oc_dup", "public.leads", "critical")
	for i := 0; i < 2; i++ {
		if err := a.RunOutOfCycle(ctx, outOfCycle([]Finding{f}, []string{f.Category},
			nil)); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE category = $1`, f.Category).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("finding rows = %d, want 1", rows)
	}
}

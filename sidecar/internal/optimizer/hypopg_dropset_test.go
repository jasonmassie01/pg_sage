package optimizer

import (
	"context"
	"strings"
	"testing"
)

// review-02a: a drop set is guarded at the caller's bar, not at the fixed
// maxGuardedRegressionPct of the single-index path.
func TestGuardedRegressionsAbove_TheBarIsExclusive(t *testing.T) {
	queries := []QueryInfo{{QueryID: 1}, {QueryID: 2}, {QueryID: 3}}
	before := measurement{costs: map[int64]float64{1: 100, 2: 100, 3: 100},
		uses: map[int64]bool{1: true, 2: true, 3: true}}
	after := map[int64]float64{1: 105, 2: 105.0001, 3: 300}
	guarded, regs := guardedRegressionsAbove(queries, before, after, 5)
	if guarded != 3 || len(regs) != 2 || regs[0].QueryID != 2 || regs[1].QueryID != 3 {
		t.Fatalf("guarded %d, regressions %+v; want 3 guarded, queryids 2 and 3", guarded,
			regs)
	}
	if _, regs := guardedRegressionsAbove(queries, before, after, 200); len(regs) != 0 {
		t.Fatalf("a 200%% bar flagged %+v", regs)
	}
}

func TestGuardedRegressions_SingleIndexPathKeepsItsBar(t *testing.T) {
	queries := []QueryInfo{{QueryID: 1}}
	before := measurement{costs: map[int64]float64{1: 100}, uses: map[int64]bool{1: true}}
	if _, regs := guardedRegressions(queries, before, map[int64]float64{1: 110}); len(regs) != 0 {
		t.Fatalf("10%% rise flagged at the 10%% bar: %+v", regs)
	}
	if _, regs := guardedRegressions(queries, before, map[int64]float64{1: 111}); len(regs) != 1 {
		t.Fatalf("11%% rise not flagged at the 10%% bar")
	}
}

func TestMeasureDropSet_RefusesBadInput(t *testing.T) {
	h := NewHypoPG(nil, noopLog2)
	if _, err := h.MeasureDropSet(context.Background(), []IndexRef{{Name: "x", OID: 1}},
		nil, 10); err == nil || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("nil pool: err = %v", err)
	}
}

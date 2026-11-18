package indexes

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// The index a drop relies on (a set's cover, a duplicate's kept copy) must
// survive the same review: dropping it as unused as well would leave the
// lookups the other drop moved onto it with no index at all.

func TestGenerate_TheCoverOfAConsolidationSetIsNeverDroppedAsUnused(t *testing.T) {
	facts := chainFacts()
	facts[2].Scans = int64(0) // t_abc: every lookup went to t_a and t_ab
	for name, m := range map[string]Measurer{"hypopg": measured(), "no hypopg": nil} {
		pack := mustPack(t, spec("db1", 90, "ok", facts...))
		cands, _, err := New(m).Generate(context.Background(), pack, env("db1"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := byTarget(cands)
		if c, ok := got["public.t_abc"]; ok {
			t.Errorf("%s: the cover t_abc is dropped too (%s %s): nothing would serve "+
				"its set's lookups", name, c.Kind, c.ActionType)
		}
		if len(cands) != 2 || got["public.t_a"].Kind != KindConsolidation ||
			got["public.t_ab"].Kind != KindConsolidation {
			t.Errorf("%s: candidates %+v, want the two consolidation drops only", name, cands)
		}
	}
}

func TestGenerate_TheKeptCopyOfADuplicateIsNeverDroppedAsUnused(t *testing.T) {
	a, b := fact("v", "v_a", "z", 4096), fact("v", "v_b", "z", 4096)
	a.Scans, b.Scans = int64(0), int64(50)
	a.DuplicateOf, b.DuplicateOf = []string{"public.v_b"}, []string{"public.v_a"}
	pack := mustPack(t, spec("db1", 90, "ok", a, b))
	cands, _, err := New(nil).Generate(context.Background(), pack, env("db1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("%d candidates (%+v); want one duplicate drop, the other copy kept",
			len(cands), cands)
	}
	// The copy the workload used is the one kept.
	c := cands[0]
	if c.Kind != KindDuplicate || c.Target != "public.v_a" ||
		!strings.Contains(c.Reason, "public.v_b") {
		t.Fatalf("candidate %s %s (%s), want the unscanned copy v_a dropped as a duplicate "+
			"of v_b", c.Kind, c.Target, c.Reason)
	}
}

func TestGenerate_AWindowUnderTheFloorCallsNoIndexUnused(t *testing.T) {
	// review.window_days is at least 14: an index unscanned for a few days
	// of history is not an unused index, so a younger pack proposes none.
	for days, want := range map[int]int{1: 0, 13: 0, 14: 1, 15: 1} {
		pack := mustPack(t, spec("db1", days, "ok", unusedFact("v_z")))
		cands, cov, err := New(nil).Generate(context.Background(), pack, env("db1"))
		if err != nil {
			t.Fatalf("%d days: %v", days, err)
		}
		if len(cands) != want {
			t.Errorf("%d days: %d candidates (%+v), want %d", days, len(cands), cands, want)
		}
		notes := strings.Join(cov.Notes, " ")
		if want == 0 && (!strings.Contains(notes, "14 days") ||
			cov.Status != candidate.CoveragePartial) {
			t.Errorf("%d days: coverage %+v does not say why no index is unused", days, cov)
		}
	}
}

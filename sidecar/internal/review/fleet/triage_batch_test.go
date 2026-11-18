package fleet

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The triage's batch bound: TriageBatchMembers members a call, and every
// call's prompt within TriageBatchTokens (review-04b review).

// wide is a fleet of n members whose lines are about 2 KB each (long
// names, many generators), so 200 of them overrun the token budget.
func wide(n int, drop ...int) map[string]Rollup {
	out := synth(n, drop...)
	for name, r := range out {
		long := name + strings.Repeat("x", 180)
		r.Database, r.Candidates = long, map[string]int{}
		for g := range 60 {
			r.Candidates[fmt.Sprintf("generator_with_a_long_name_%02d", g)] = g + 1
		}
		delete(out, name)
		out[long] = r
	}
	return out
}

func TestTriage_WideLinesSplitBatchesWithinTheTokenBudget(t *testing.T) {
	m := namesDrops(t)
	var drops []int
	for i := 10; i <= 400; i += 20 {
		drops = append(drops, i)
	}
	rollups := rollupsOf(wide(400, drops...))
	if per := lineBytes(rollups[0]); per < 1500 {
		t.Fatalf("fixture line is %d bytes, want a wide one", per)
	}
	tr := RunTriage(context.Background(), m, rollups, nil)
	if tr.Mode != ModeModel {
		t.Fatalf("mode %s reasons %v, want model", tr.Mode, tr.Reasons)
	}
	if len(tr.Outliers) != len(drops) {
		t.Fatalf("%d outliers, want the %d seeded ones", len(tr.Outliers), len(drops))
	}
	// 400 members at about 2 KB a line fit about 95 a call: more than the
	// two calls the member bound alone gives.
	if m.calls() <= 2 {
		t.Fatalf("%d calls for 400 wide members, want the token budget to split them",
			m.calls())
	}
	seen := map[string]int{}
	for i, p := range m.prompts {
		if len(p)+len(triageSystem) > TriageBatchTokens*4 {
			t.Fatalf("call %d is %d bytes with its system prompt, over %d tokens", i,
				len(p)+len(triageSystem), TriageBatchTokens)
		}
		for _, l := range promptLines(t, p) {
			seen[l.ID]++
		}
	}
	if len(seen) != 400 {
		t.Fatalf("%d members reached the model, want all 400", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("member %s was sent %d times, want once", id, n)
		}
	}
}

func TestBatchesOf_Boundaries(t *testing.T) {
	small := rollupsOf(synth(TriageBatchMembers + 1))
	got := batchesOf(small)
	if len(got) != 2 || len(got[0]) != TriageBatchMembers || len(got[1]) != 1 {
		t.Fatalf("%d small members gave batches of %v, want %d and 1", len(small),
			batchSizes(got), TriageBatchMembers)
	}
	if got := batchesOf(small[:TriageBatchMembers]); len(got) != 1 {
		t.Fatalf("exactly %d members gave %v batches, want one", TriageBatchMembers,
			batchSizes(got))
	}
	if got := batchesOf(nil); len(got) != 0 {
		t.Fatalf("no members gave %v batches, want none", batchSizes(got))
	}
	// A member whose line alone overruns the budget still gets its call.
	huge := small[0]
	huge.Candidates = map[string]int{}
	for g := range 6000 {
		huge.Candidates[fmt.Sprintf("g%05d_%s", g, strings.Repeat("y", 30))] = 1
	}
	got = batchesOf([]Rollup{small[1], huge, small[2]})
	if len(got) != 3 || got[1][0].Database != huge.Database {
		t.Fatalf("an over-budget member gave batches %v, want it alone in the middle",
			batchSizes(got))
	}
}

func batchSizes(bs [][]Rollup) []int {
	out := make([]int, 0, len(bs))
	for _, b := range bs {
		out = append(out, len(b))
	}
	return out
}

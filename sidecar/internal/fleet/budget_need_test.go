package fleet

import (
	"fmt"
	"sync"
	"testing"
)

func sum(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

func TestAllocateByNeed_EvenWhenNeedsAreEqual(t *testing.T) {
	got := AllocateByNeed(900, map[string]float64{"a": 1, "b": 1, "c": 1}, 50, 300)
	for name, v := range got {
		if v != 300 {
			t.Fatalf("%s = %d, want 300 (equal need, equal share)", name, v)
		}
	}
}

func TestAllocateByNeed_FollowsNeedWithinFloorAndCeiling(t *testing.T) {
	w := map[string]float64{"busy": 10, "idle1": 1, "idle2": 1, "idle3": 1}
	got := AllocateByNeed(1000, w, 50, 200)
	even := 1000 / 4
	if got["busy"] != 2*even {
		t.Fatalf("busy = %d, want the 200%% ceiling %d", got["busy"], 2*even)
	}
	for _, name := range []string{"idle1", "idle2", "idle3"} {
		if got[name] < even/2 {
			t.Fatalf("%s = %d, below the 50%% floor %d", name, got[name], even/2)
		}
	}
	if s := sum(got); s > 1000 {
		t.Fatalf("allocations sum to %d, over the 1000 cap", s)
	}
	if got["busy"] <= got["idle1"] {
		t.Fatal("need must earn a larger share")
	}
}

func TestAllocateByNeed_NeverExceedsTheCap(t *testing.T) {
	for n := 1; n <= 13; n++ {
		for _, total := range []int{0, 1, 7, 100, 99_999, 1_000_000} {
			w := map[string]float64{}
			for i := 0; i < n; i++ {
				w[fmt.Sprintf("db%d", i)] = float64(i*i + 1)
			}
			for _, fc := range [][2]int{{0, 100}, {50, 300}, {100, 100}, {10, 1000}} {
				got := AllocateByNeed(total, w, fc[0], fc[1])
				if s := sum(got); s > total {
					t.Fatalf("n=%d total=%d floor/ceil=%v: sum %d > cap", n, total, fc, s)
				}
				if len(got) != n {
					t.Fatalf("n=%d: %d allocations", n, len(got))
				}
				for name, v := range got {
					if v < 0 {
						t.Fatalf("%s negative allocation %d", name, v)
					}
				}
			}
		}
	}
}

func TestAllocateByNeed_FloorBoundaries(t *testing.T) {
	w := map[string]float64{"busy": 1000, "idle": 1}
	// floor 100% = even split no matter the need.
	got := AllocateByNeed(1000, w, 100, 300)
	if got["busy"] != 500 || got["idle"] != 500 {
		t.Fatalf("floor 100%% = %v, want 500/500", got)
	}
	// floor 0% lets need take everything up to the ceiling.
	got = AllocateByNeed(1000, w, 0, 200)
	if got["busy"] < 999 {
		t.Fatalf("floor 0%%, ceiling 200%%: busy = %d, want about 1000", got["busy"])
	}
	if got["idle"] > 1 {
		t.Fatalf("floor 0%%: idle = %d, want about 0", got["idle"])
	}
}

func TestAllocateByNeed_SpareFromCeilingsIsRedistributed(t *testing.T) {
	// Two busy databases cap at the ceiling; the remainder goes to the rest
	// instead of being lost.
	w := map[string]float64{"b1": 100, "b2": 100, "i1": 1, "i2": 1}
	got := AllocateByNeed(400, w, 25, 150)
	if got["b1"] != 150 || got["b2"] != 150 {
		t.Fatalf("busy = %d/%d, want the 150 ceiling", got["b1"], got["b2"])
	}
	if s := sum(got); s < 398 {
		t.Fatalf("sum = %d, spare capacity was dropped (want ~400)", s)
	}
}

func TestAllocateByNeed_ZeroNegativeAndEmptyWeights(t *testing.T) {
	if got := AllocateByNeed(100, nil, 50, 300); len(got) != 0 {
		t.Fatalf("no databases = %v", got)
	}
	got := AllocateByNeed(100, map[string]float64{"a": 0, "b": -3}, 50, 300)
	if got["a"] != 50 || got["b"] != 50 {
		t.Fatalf("non-positive weights must count as equal need, got %v", got)
	}
	got = AllocateByNeed(-10, map[string]float64{"a": 1}, 50, 300)
	if got["a"] != 0 {
		t.Fatalf("negative total = %v, want 0", got)
	}
}

func TestFleetBudget_NeedSplitAppliesAndSurvivesRegister(t *testing.T) {
	b := NewBudget(1000, []string{"a", "b"})
	b.SetSplit(SplitNeed, 50, 300)
	b.SetNeeds(map[string]float64{"a": 9, "b": 1})
	if b.Allocation("a") <= b.Allocation("b") {
		t.Fatalf("a=%d b=%d, the needier database must get more",
			b.Allocation("a"), b.Allocation("b"))
	}
	b.Register("c")
	if s := b.Allocation("a") + b.Allocation("b") + b.Allocation("c"); s > 1000 {
		t.Fatalf("after register sum = %d > 1000", s)
	}
	if b.Allocation("c") == 0 {
		t.Fatal("a new database must get at least its floor")
	}
	if b.Allocation("a") <= b.Allocation("c") {
		t.Fatal("register must keep the measured needs")
	}
	snap := b.Snapshot()
	if snap["a"].Weight != 9 || snap["c"].Weight != 1 {
		t.Fatalf("snapshot weights = %+v", snap)
	}
	b.Unregister("a")
	if b.Allocation("b")+b.Allocation("c") > 1000 {
		t.Fatal("unregister broke the cap")
	}
}

func TestFleetBudget_EvenSplitIgnoresNeeds(t *testing.T) {
	b := NewBudget(1000, []string{"a", "b"})
	b.SetSplit(SplitEven, 50, 300)
	b.SetNeeds(map[string]float64{"a": 9, "b": 1})
	if b.Allocation("a") != 500 || b.Allocation("b") != 500 {
		t.Fatalf("even = %d/%d, want 500/500", b.Allocation("a"), b.Allocation("b"))
	}
	if b.Split() != SplitEven {
		t.Fatalf("split = %q", b.Split())
	}
}

func TestFleetBudget_UnknownSplitFallsBackToEven(t *testing.T) {
	b := NewBudget(100, []string{"a", "b"})
	b.SetSplit("bogus", 50, 300)
	b.SetNeeds(map[string]float64{"a": 9, "b": 1})
	if b.Allocation("a") != 50 || b.Split() != SplitEven {
		t.Fatalf("bogus split: a=%d split=%q", b.Allocation("a"), b.Split())
	}
}

func TestFleetBudget_NeedsForUnknownDatabasesAreIgnored(t *testing.T) {
	b := NewBudget(100, []string{"a"})
	b.SetSplit(SplitNeed, 50, 300)
	b.SetNeeds(map[string]float64{"ghost": 50})
	if _, has := b.Snapshot()["ghost"]; has {
		t.Fatal("a need must not register a database")
	}
	if b.Allocation("a") != 100 {
		t.Fatalf("a = %d, want the whole budget", b.Allocation("a"))
	}
}

func TestFleetBudget_ShrunkAllocationStopsSpending(t *testing.T) {
	b := NewBudget(1000, []string{"a", "b"})
	b.SetSplit(SplitNeed, 10, 300)
	b.Spend("a", 400)
	b.SetNeeds(map[string]float64{"a": 1, "b": 100})
	if b.Allocation("a") >= 400 || b.CanSpend("a", 1) {
		t.Fatalf("a = %d after its need fell; over its allocation it must not spend",
			b.Allocation("a"))
	}
	if b.Used("a") != 400 {
		t.Fatalf("rebalancing must keep usage, got %d", b.Used("a"))
	}
}

func TestFleetBudget_ConcurrentNeedsAndSpend(t *testing.T) {
	b := NewBudget(10_000, []string{"a", "b", "c"})
	b.SetSplit(SplitNeed, 50, 300)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			b.SetNeeds(map[string]float64{"a": float64(n), "b": 1, "c": 2})
		}(i)
		go func() {
			defer wg.Done()
			if b.CanSpend("a", 10) {
				b.Spend("a", 10)
			}
		}()
	}
	wg.Wait()
	total := 0
	for _, u := range b.Snapshot() {
		total += u.Allocation
	}
	if total > 10_000 {
		t.Fatalf("concurrent rebalances broke the cap: %d", total)
	}
}

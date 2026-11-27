package fleet

import (
	"sync"
	"testing"
)

// llm-03: Hold and Settle make an LLM call's estimate reset-aware. A
// ResetDaily between them already discarded the hold, so Settle charges
// the full actual instead of actual-held.

func TestBudgetHold_SettleSameEpochChargesActual(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	epoch, ok := b.Hold("a", 400)
	if !ok {
		t.Fatal("400 of 1000 must fit")
	}
	if got := b.Used("a"); got != 400 {
		t.Fatalf("used while held = %d, want 400", got)
	}
	b.Settle("a", 400, 150, epoch)
	if got := b.Used("a"); got != 150 {
		t.Fatalf("used after settle = %d, want 150", got)
	}
}

func TestBudgetHold_SettleAfterResetChargesFullActual(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	epoch, _ := b.Hold("a", 400)
	b.ResetDaily()
	b.Spend("a", 20)
	b.Settle("a", 400, 150, epoch)
	if got := b.Used("a"); got != 170 {
		t.Fatalf("used = %d, want 20 since reset + 150 actual", got)
	}
}

func TestBudgetHold_ReleaseAfterResetKeepsNewSpend(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	epoch, _ := b.Hold("a", 400)
	b.ResetDaily()
	b.Spend("a", 20)
	b.Settle("a", 400, 0, epoch)
	if got := b.Used("a"); got != 20 {
		t.Fatalf("used = %d, want the 20 spent since the reset", got)
	}
}

func TestBudgetHold_BoundaryAndUnknownDatabase(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	if _, ok := b.Hold("a", 1000); !ok {
		t.Fatal("exactly the allocation must fit")
	}
	if _, ok := b.Hold("a", 1); ok {
		t.Fatal("one token over the allocation must be refused")
	}
	if got := b.Used("a"); got != 1000 {
		t.Fatalf("a refused hold changed usage: %d", got)
	}
	if _, ok := b.Hold("missing", 1); ok {
		t.Fatal("an unregistered database must be refused")
	}
	b.Settle("missing", 1, 1, 0) // must not panic or create the database
	if got := b.Allocation("missing"); got != 0 {
		t.Fatalf("settle created an allocation: %d", got)
	}
}

func TestBudgetHold_EpochAdvancesOnReset(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	first := b.Epoch()
	b.ResetDaily()
	if b.Epoch() != first+1 {
		t.Fatalf("epoch = %d, want %d after one reset", b.Epoch(), first+1)
	}
}

func TestBudgetHold_ConcurrentHoldsNeverExceedAllocation(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := b.Hold("a", 100); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 10 || b.Used("a") != 1000 {
		t.Fatalf("admitted %d holds, used %d; want 10 and 1000", admitted, b.Used("a"))
	}
}

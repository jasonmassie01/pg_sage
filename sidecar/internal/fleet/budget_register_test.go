package fleet

import "testing"

func TestBudgetRegister_AddsDatabaseAfterStartup(t *testing.T) {
	b := NewBudget(100, nil)
	if b.CanSpend("late", 1) {
		t.Fatal("unregistered database must not spend")
	}

	b.Register("late")

	if got := b.Allocation("late"); got != 100 {
		t.Fatalf("allocation = %d, want 100", got)
	}
	if !b.CanSpend("late", 50) {
		t.Fatal("registered database should be able to spend 50 of 100")
	}
}

func TestBudgetRegister_RebalancesAndKeepsUsage(t *testing.T) {
	b := NewBudget(100, []string{"a"})
	b.Spend("a", 30)

	b.Register("b")

	if got := b.Allocation("a"); got != 50 {
		t.Fatalf("a allocation = %d, want 50", got)
	}
	if got := b.Allocation("b"); got != 50 {
		t.Fatalf("b allocation = %d, want 50", got)
	}
	if got := b.Used("a"); got != 30 {
		t.Fatalf("a used = %d, want 30 (rebalance must keep usage)", got)
	}
	if b.CanSpend("a", 21) {
		t.Fatal("a has 20 tokens left and must not spend 21")
	}
}

func TestBudgetRegister_IdempotentForKnownDatabase(t *testing.T) {
	b := NewBudget(90, []string{"a", "b", "c"})
	b.Spend("a", 10)

	b.Register("a")

	if got := b.Allocation("a"); got != 30 {
		t.Fatalf("allocation = %d, want 30", got)
	}
	if got := b.Used("a"); got != 10 {
		t.Fatalf("used = %d, want 10", got)
	}
}

func TestBudgetUnregister_ReturnsShareToRemaining(t *testing.T) {
	b := NewBudget(100, []string{"a", "b"})

	b.Unregister("a")

	if b.CanSpend("a", 1) {
		t.Fatal("unregistered database must not spend")
	}
	if got := b.Allocation("b"); got != 100 {
		t.Fatalf("b allocation = %d, want 100", got)
	}
}

func TestBudgetRename_MovesAllocation(t *testing.T) {
	b := NewBudget(100, []string{"a", "x"})
	b.Spend("a", 5)

	b.Unregister("a")
	b.Register("b")

	if got := b.Allocation("b"); got != 50 {
		t.Fatalf("b allocation = %d, want 50", got)
	}
	if got := b.Used("b"); got != 0 {
		t.Fatalf("renamed database starts a fresh window, used = %d", got)
	}
	if got := b.Allocation("a"); got != 0 {
		t.Fatalf("old name allocation = %d, want 0", got)
	}
}

func TestBudgetSnapshot_ReportsEveryDatabase(t *testing.T) {
	b := NewBudget(100, []string{"a", "b"})
	b.Spend("b", 7)

	snap := b.Snapshot()

	if len(snap) != 2 {
		t.Fatalf("snapshot size = %d, want 2", len(snap))
	}
	if snap["b"].Used != 7 || snap["b"].Allocation != 50 {
		t.Fatalf("b snapshot = %+v, want used 7 allocation 50", snap["b"])
	}
	if snap["a"].Used != 0 || snap["a"].Allocation != 50 {
		t.Fatalf("a snapshot = %+v, want used 0 allocation 50", snap["a"])
	}
}

func TestBudgetRegister_ZeroTotalNeverSpends(t *testing.T) {
	b := NewBudget(0, nil)
	b.Register("a")
	if b.CanSpend("a", 1) {
		t.Fatal("zero total budget must not allow spending")
	}
}

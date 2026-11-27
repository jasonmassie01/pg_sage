package fleet

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fleet-10: Restore adds the usage a previous process recorded today, so
// a restart does not hand a database a fresh allocation.

func TestBudgetRestore_SpendExactlyTheRestOfTheAllocation(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	if err := b.Restore("a", 400); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := b.Used("a"); got != 400 {
		t.Fatalf("used after restore = %d, want 400", got)
	}
	if !b.CanSpend("a", 600) {
		t.Fatal("600 more must fit: 400 restored + 600 = the 1000 allocation")
	}
	b.Spend("a", 600)
	if b.CanSpend("a", 1) {
		t.Fatal("one token past the allocation must be refused after restore")
	}
	if _, ok := b.Hold("a", 1); ok {
		t.Fatal("a hold past the allocation must be refused after restore")
	}
}

func TestBudgetRestore_AddsToUsageAlreadySpent(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	b.Spend("a", 50)
	if err := b.Restore("a", 400); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := b.Used("a"); got != 450 {
		t.Fatalf("used = %d, want 450 (50 spent here + 400 restored)", got)
	}
}

func TestBudgetRestore_HeldUntilRegister(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	if err := b.Restore("late", 300); err != nil {
		t.Fatalf("restore before register: %v", err)
	}
	if got := b.Used("late"); got != 0 {
		t.Fatalf("an unregistered database reports used %d, want 0", got)
	}
	if b.CanSpend("late", 1) {
		t.Fatal("an unregistered database must still be refused")
	}
	b.Register("late")
	if got := b.Used("late"); got != 300 {
		t.Fatalf("used after register = %d, want the held 300", got)
	}
	// The held restore is applied once, not again on a repeated Register.
	b.Register("late")
	if got := b.Used("late"); got != 300 {
		t.Fatalf("used after a second register = %d, want 300", got)
	}
	if got := b.Used("a"); got != 0 {
		t.Fatalf("restore for another database leaked into a: used %d", got)
	}
}

func TestBudgetRestore_NegativeIsRejected(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	b.Spend("a", 100)
	err := b.Restore("a", -1)
	if err == nil {
		t.Fatal("a negative restore must be rejected")
	}
	if !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "-1") {
		t.Fatalf("error %q must name the database and the value", err)
	}
	if got := b.Used("a"); got != 100 {
		t.Fatalf("a rejected restore changed used to %d, want 100", got)
	}
	if err := b.Restore("late", -5); err == nil {
		t.Fatal("a negative restore for an unregistered database must be rejected")
	}
	b.Register("late")
	if got := b.Used("late"); got != 0 {
		t.Fatalf("a rejected held restore was applied: used %d", got)
	}
}

func TestBudgetRestore_ZeroIsANoOp(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	if err := b.Restore("a", 0); err != nil {
		t.Fatalf("restore 0: %v", err)
	}
	if got := b.Used("a"); got != 0 {
		t.Fatalf("used = %d, want 0", got)
	}
}

// The midnight reset drops a held restore: it belonged to the old day.
func TestBudgetRestore_ResetDailyDropsHeldRestore(t *testing.T) {
	b := NewBudget(1000, []string{"a"})
	if err := b.Restore("a", 400); err != nil {
		t.Fatal(err)
	}
	if err := b.Restore("late", 300); err != nil {
		t.Fatal(err)
	}
	b.ResetDaily()
	b.Register("late")
	if got := b.Used("late"); got != 0 {
		t.Fatalf("yesterday's held restore applied after reset: used %d", got)
	}
	if got := b.Used("a"); got != 0 {
		t.Fatalf("used after reset = %d, want 0", got)
	}
}

// SnapshotDay reports the UTC day the usage belongs to with the usage.
func TestBudgetSnapshotDay_NamesTodayAndUsage(t *testing.T) {
	b := NewBudget(1000, []string{"a", "b"})
	b.Spend("a", 70)
	day, snap := b.SnapshotDay()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if !day.Equal(today) {
		t.Fatalf("snapshot day = %v, want %v", day, today)
	}
	if snap["a"].Used != 70 || snap["b"].Used != 0 || len(snap) != 2 {
		t.Fatalf("snapshot = %+v, want a=70 b=0", snap)
	}
	if snap["a"].Allocation != 500 {
		t.Fatalf("allocation = %d, want 500", snap["a"].Allocation)
	}
}

// Restores racing spends on the same database lose nothing.
func TestBudgetRestore_ConcurrentWithSpend(t *testing.T) {
	b := NewBudget(1000000, []string{"a"})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); b.Spend("a", 10) }()
		go func() { defer wg.Done(); _ = b.Restore("a", 5) }()
	}
	wg.Wait()
	if got := b.Used("a"); got != 50*15 {
		t.Fatalf("used = %d, want %d", got, 50*15)
	}
}

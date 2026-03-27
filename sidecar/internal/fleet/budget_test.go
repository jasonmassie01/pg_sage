package fleet

import "testing"

func TestBudget_EqualAllocation(t *testing.T) {
	b := NewBudget(100000, []string{"a", "b", "c", "d"})
	if b.Allocation("a") != 25000 {
		t.Errorf("expected 25000, got %d", b.Allocation("a"))
	}
}

func TestBudget_CanSpend_WithinBudget(t *testing.T) {
	b := NewBudget(100000, []string{"a"})
	if !b.CanSpend("a", 50000) {
		t.Error("should be within budget")
	}
}

func TestBudget_CanSpend_ExceedsBudget(t *testing.T) {
	b := NewBudget(100000, []string{"a"})
	b.Spend("a", 95000)
	if b.CanSpend("a", 10000) {
		t.Error("should exceed budget")
	}
}

func TestBudget_CanSpend_UnknownDB(t *testing.T) {
	b := NewBudget(100000, []string{"a"})
	if b.CanSpend("nope", 1) {
		t.Error("unknown DB should return false")
	}
}

func TestBudget_Spend_UpdatesUsed(t *testing.T) {
	b := NewBudget(100000, []string{"a"})
	b.Spend("a", 5000)
	if b.Used("a") != 5000 {
		t.Errorf("expected 5000, got %d", b.Used("a"))
	}
}

func TestBudget_ResetDaily(t *testing.T) {
	b := NewBudget(100000, []string{"a", "b"})
	b.Spend("a", 50000)
	b.Spend("b", 30000)
	b.ResetDaily()
	if b.Used("a") != 0 || b.Used("b") != 0 {
		t.Errorf("expected 0 after reset, got a=%d b=%d",
			b.Used("a"), b.Used("b"))
	}
}

func TestBudget_CrossDatabaseIsolation(t *testing.T) {
	b := NewBudget(100000, []string{"a", "b"})
	b.Spend("a", 50000) // a: 50K/50K
	if !b.CanSpend("b", 25000) {
		t.Error("b should be unaffected by a's spending")
	}
}

func TestBudget_ExactBoundary(t *testing.T) {
	b := NewBudget(100000, []string{"a"})
	// allocation = 100K, spend exactly 100K
	if !b.CanSpend("a", 100000) {
		t.Error("should allow spending exact allocation")
	}
	b.Spend("a", 100000)
	if b.CanSpend("a", 1) {
		t.Error("should not allow any more")
	}
}

func TestBudget_EmptyDatabases(t *testing.T) {
	b := NewBudget(100000, []string{})
	if b.CanSpend("a", 1) {
		t.Error("no databases registered")
	}
}

func TestBudget_Used_UnknownDB(t *testing.T) {
	b := NewBudget(100000, []string{"a"})
	if b.Used("nope") != 0 {
		t.Error("unknown DB should return 0")
	}
}

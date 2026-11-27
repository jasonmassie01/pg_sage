package llm

import (
	"errors"
	"testing"
	"time"
)

// llm-03: the global Budget follows the client rule: a day turn or a
// Reset zeroes spend and keeps in-flight holds, so a settle never drives
// the hold negative.
func TestGlobalBudget_RolloverKeepsHolds(t *testing.T) {
	b := NewBudget()
	b.SetCeiling(20000, "configured")
	if err := b.hold(15000); err != nil {
		t.Fatalf("hold: %v", err)
	}
	b.mu.Lock()
	b.used = 4000
	b.day = budgetDay(time.Now()) - 1
	b.mu.Unlock()

	if err := b.hold(6000); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("15,000 held + 6,000 under 20,000 after rollover = %v", err)
	}
	if s := b.Status(); s.Used != 0 || s.Reserved != 15000 {
		t.Fatalf("after rollover = %+v, want used 0 and the hold kept", s)
	}
	b.settle(15000, 100)
	if s := b.Status(); s.Used != 100 || s.Reserved != 0 {
		t.Fatalf("after settle = %+v, want used 100, reserved 0", s)
	}
}

func TestGlobalBudget_ResetKeepsHolds(t *testing.T) {
	b := NewBudget()
	b.SetCeiling(20000, "configured")
	if err := b.hold(15000); err != nil {
		t.Fatalf("hold: %v", err)
	}
	b.Reset()
	if err := b.hold(6000); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("hold over the cap after Reset = %v", err)
	}
	b.settle(15000, 0)
	if s := b.Status(); s.Used != 0 || s.Reserved != 0 {
		t.Fatalf("after release = %+v, want zero", s)
	}
}

// Invalid input: a negative ceiling is treated as unlimited, like 0.
func TestGlobalBudget_NegativeCeilingIsUnlimited(t *testing.T) {
	b := NewBudget()
	b.SetCeiling(-5, "configured")
	if err := b.hold(1 << 30); err != nil {
		t.Fatalf("hold under a negative (unlimited) ceiling: %v", err)
	}
	if s := b.Status(); s.Ceiling != 0 {
		t.Fatalf("ceiling = %d, want 0 (unlimited)", s.Ceiling)
	}
}

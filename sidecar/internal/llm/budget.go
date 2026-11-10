package llm

import (
	"fmt"
	"sync"
	"time"
)

// Budget is a process-wide daily token ceiling shared by every client the
// sidecar can spend through, charged after each client's own checks. The
// per-client llm.token_budget_daily copies add up; this bounds the sum.
// A ceiling of 0 is unlimited. Like a client, a UTC day turn or Reset
// zeroes spend and keeps the holds of calls still in flight.
type Budget struct {
	mu       sync.Mutex
	ceiling  int64
	source   string
	used     int64
	reserved int64
	day      int64
}

// BudgetStatus is the /api/v1/llm/status view of the global ceiling.
type BudgetStatus struct {
	Ceiling   int64  `json:"ceiling"`
	Used      int64  `json:"used"`
	Reserved  int64  `json:"reserved"`
	Source    string `json:"source"`
	Exhausted bool   `json:"exhausted"`
}

// NewBudget returns an unlimited budget for today.
func NewBudget() *Budget { return &Budget{day: budgetDay(time.Now())} }

// SetCeiling installs the ceiling and names where it came from. A
// negative ceiling is treated as 0 (unlimited).
func (b *Budget) SetCeiling(ceiling int64, source string) {
	if ceiling < 0 {
		ceiling = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ceiling, b.source = ceiling, source
}

func (b *Budget) rolloverLocked() {
	if today := budgetDay(time.Now()); today != b.day {
		b.used, b.day = 0, today
	}
}

func (b *Budget) hold(tokens int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rolloverLocked()
	if b.ceiling > 0 && b.used+b.reserved+tokens > b.ceiling {
		return budgetError(fmt.Sprintf(
			"global token budget exhausted (%d reserved or used/%d, %s); "+
				"raise llm.global_token_budget_daily or wait for the UTC day",
			b.used+b.reserved, b.ceiling, b.source))
	}
	b.reserved += tokens
	return nil
}

// settle replaces a hold with actual usage (0 releases it).
func (b *Budget) settle(held, actual int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rolloverLocked()
	b.reserved -= held
	b.used += actual
}

// Reset zeroes today's spend; in-flight holds stay.
func (b *Budget) Reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used, b.day = 0, budgetDay(time.Now())
}

// Status reports the ceiling, today's spend and in-flight holds. A nil
// Budget reports zero values.
func (b *Budget) Status() BudgetStatus {
	if b == nil {
		return BudgetStatus{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rolloverLocked()
	return BudgetStatus{
		Ceiling: b.ceiling, Used: b.used, Reserved: b.reserved, Source: b.source,
		Exhausted: b.ceiling > 0 && b.used >= b.ceiling,
	}
}

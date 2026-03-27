package fleet

import "sync"

// FleetBudget tracks per-database LLM token spending.
type FleetBudget struct {
	TotalDaily int
	perDB      map[string]*dbBudget
	mu         sync.Mutex
}

type dbBudget struct {
	allocation int
	used       int
}

// NewBudget creates a fleet budget with equal allocation.
func NewBudget(totalDaily int, databases []string) *FleetBudget {
	b := &FleetBudget{
		TotalDaily: totalDaily,
		perDB:      make(map[string]*dbBudget, len(databases)),
	}
	alloc := 0
	if len(databases) > 0 {
		alloc = totalDaily / len(databases)
	}
	for _, name := range databases {
		b.perDB[name] = &dbBudget{allocation: alloc}
	}
	return b
}

// CanSpend checks if a database has budget for the given tokens.
func (b *FleetBudget) CanSpend(database string, tokens int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	db := b.perDB[database]
	if db == nil {
		return false
	}
	return db.used+tokens <= db.allocation
}

// Spend records token usage for a database.
func (b *FleetBudget) Spend(database string, tokens int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if db := b.perDB[database]; db != nil {
		db.used += tokens
	}
}

// Used returns the current usage for a database.
func (b *FleetBudget) Used(database string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if db := b.perDB[database]; db != nil {
		return db.used
	}
	return 0
}

// Allocation returns the allocation for a database.
func (b *FleetBudget) Allocation(database string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if db := b.perDB[database]; db != nil {
		return db.allocation
	}
	return 0
}

// ResetDaily resets all usage to zero.
func (b *FleetBudget) ResetDaily() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, db := range b.perDB {
		db.used = 0
	}
}

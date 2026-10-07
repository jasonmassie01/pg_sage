package fleet

import "sync"

// FleetBudget tracks per-database LLM token spending.
type FleetBudget struct {
	TotalDaily int
	perDB      map[string]*dbBudget
	mu         sync.Mutex
	// split is SplitEven (the default) or SplitNeed; floorPct and
	// ceilingPct bound a need-based share as percentages of the even one.
	split      string
	floorPct   int
	ceilingPct int
}

type dbBudget struct {
	allocation int
	used       int
	weight     float64 // measured need; 1 until measured
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
		b.perDB[name] = &dbBudget{allocation: alloc, weight: 1}
	}
	return b
}

// Register adds a database to the budget (idempotent) and rebalances every
// allocation to an equal share. Databases added or renamed after startup
// must be registered, otherwise CanSpend denies them.
func (b *FleetBudget) Register(database string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.perDB[database] == nil {
		b.perDB[database] = &dbBudget{weight: 1}
	}
	b.rebalanceLocked()
}

// Unregister removes a deleted or renamed database and returns its share to
// the remaining databases.
func (b *FleetBudget) Unregister(database string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.perDB, database)
	b.rebalanceLocked()
}

// rebalanceLocked splits TotalDaily equally, or by measured need under
// SplitNeed; usage is preserved.
func (b *FleetBudget) rebalanceLocked() {
	if len(b.perDB) == 0 {
		return
	}
	if b.split == SplitNeed {
		weights := make(map[string]float64, len(b.perDB))
		for name, db := range b.perDB {
			weights[name] = db.weight
		}
		for name, alloc := range AllocateByNeed(b.TotalDaily, weights, b.floorPct,
			b.ceilingPct) {
			b.perDB[name].allocation = alloc
		}
		return
	}
	alloc := b.TotalDaily / len(b.perDB)
	for _, db := range b.perDB {
		db.allocation = alloc
	}
}

// BudgetUsage is a point-in-time view of one database's allocation.
type BudgetUsage struct {
	Allocation int     `json:"allocation"`
	Used       int     `json:"used"`
	Weight     float64 `json:"weight"`
}

// Snapshot returns every registered database's allocation and usage.
func (b *FleetBudget) Snapshot() map[string]BudgetUsage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]BudgetUsage, len(b.perDB))
	for name, db := range b.perDB {
		out[name] = BudgetUsage{Allocation: db.allocation, Used: db.used,
			Weight: db.weight}
	}
	return out
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
		if db.used < 0 {
			db.used = 0
		}
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

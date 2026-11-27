package fleet

import (
	"sync"
	"time"
)

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
	// epoch counts ResetDaily calls, so a hold taken before a reset is not
	// subtracted from the new day's usage (Hold, Settle).
	epoch uint64
	// day is the UTC day the usage belongs to (SnapshotDay); pending holds
	// restored usage of databases not registered yet (Restore).
	day     time.Time
	pending map[string]int
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
		day:        utcDay(time.Now()),
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
		b.perDB[database] = &dbBudget{weight: 1, used: b.pending[database]}
		delete(b.pending, database)
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

// Snapshot returns every registered database's allocation and usage; a nil
// budget (none configured) returns nil.
func (b *FleetBudget) Snapshot() map[string]BudgetUsage {
	if b == nil {
		return nil
	}
	_, out := b.SnapshotDay()
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

// ResetDaily resets all usage to zero, drops restores still held for
// unregistered databases and starts a new hold epoch.
func (b *FleetBudget) ResetDaily() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.epoch++
	b.day, b.pending = utcDay(time.Now()), nil
	for _, db := range b.perDB {
		db.used = 0
	}
}

// Epoch identifies the current reset period.
func (b *FleetBudget) Epoch() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.epoch
}

// Hold atomically checks and charges an in-flight estimate for a database
// and returns the epoch it was charged in. An unregistered database or an
// estimate past the allocation is refused and charges nothing.
func (b *FleetBudget) Hold(database string, tokens int) (uint64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	db := b.perDB[database]
	if db == nil || db.used+tokens > db.allocation {
		return b.epoch, false
	}
	db.used += tokens
	return b.epoch, true
}

// Settle replaces a hold taken in epoch with actual usage (0 releases it).
// After a ResetDaily the hold is already gone, so the full actual is
// charged; otherwise actual-held.
func (b *FleetBudget) Settle(database string, held, actual int, epoch uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	db := b.perDB[database]
	if db == nil {
		return
	}
	delta := actual
	if epoch == b.epoch {
		delta -= held
	}
	db.used += delta
	if db.used < 0 {
		db.used = 0
	}
}

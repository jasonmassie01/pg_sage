package fleet

import (
	"fmt"
	"time"
)

// Restore adds usage that an earlier process of this sidecar recorded
// today (the control-database ledger, fleet-10), so a restart does not
// hand the database a fresh allocation. The ledger calls it once per
// start, with today's rows only. A database not registered yet keeps the
// usage until Register; ResetDaily drops it. Negative usage is rejected.
func (b *FleetBudget) Restore(database string, used int) error {
	if used < 0 {
		return fmt.Errorf("fleet budget restore for %q: negative usage %d", database, used)
	}
	if used == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if db := b.perDB[database]; db != nil {
		db.used += used
		return nil
	}
	if b.pending == nil {
		b.pending = make(map[string]int)
	}
	b.pending[database] += used
	return nil
}

// SnapshotDay returns the UTC day the usage belongs to with every
// registered database's allocation and usage, read together. Until the
// midnight reset runs, the day is still the previous one.
func (b *FleetBudget) SnapshotDay() (time.Time, map[string]BudgetUsage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]BudgetUsage, len(b.perDB))
	for name, db := range b.perDB {
		out[name] = BudgetUsage{Allocation: db.allocation, Used: db.used,
			Weight: db.weight}
	}
	return b.day, out
}

func utcDay(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

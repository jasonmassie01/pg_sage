package retention

import (
	"context"
	"github.com/pg-sage/sidecar/internal/config"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// History in the meta database (history.store: meta). The two history
// tables of every monitored database live in the meta database's
// sage.snapshots and sage.query_store. A per-database cleaner leaves them
// alone (its monitored database's copies are removed by `history migrate
// --cleanup`); one store cleaner per process ages them out by the
// process-wide windows (a day expires for every database at once) and
// holds sage.snapshots under the sum of the databases' caps.

// ForHistoryStore makes the cleaner the history store's: it runs only the
// history rules, on its pool (the meta database), with the snapshot cap
// summed over the databases registered in the store.
func (c *Cleaner) ForHistoryStore() *Cleaner {
	c.storeMode = true
	return c
}

// historyElsewhere reports a per-database cleaner whose database keeps its
// history in the meta database.
func (c *Cleaner) historyElsewhere() bool {
	return !c.storeMode && histstore.Resolve(c.pool).Scoped()
}

// rules are the purge rules this cleaner runs.
func (c *Cleaner) rules() []purgeRule {
	all := purgeRules(c.cfg)
	if !c.storeMode && !c.historyElsewhere() {
		return all
	}
	out := make([]purgeRule, 0, len(all))
	for _, r := range all {
		if (r.partitioned != nil) == c.storeMode {
			out = append(out, r)
		}
	}
	return out
}

// storeCap is the store's snapshot cap: each database's cap (pct of its
// size, never below the floor) summed; 0 when the cap is off or no
// database is registered.
func storeCap(dbs []histstore.StoreDatabase, pct int) int64 {
	if pct <= 0 {
		return 0
	}
	var total int64
	for _, d := range dbs {
		total += max(max(d.DBBytes, 0)*int64(pct)/100, config.MinSnapshotCapBytes)
	}
	return total
}

// storeCapFor refreshes the sizes of the databases this process monitors
// and sums the caps of the databases registered within the snapshot
// window (an offline database keeps its last known size until its history
// ages out).
func (c *Cleaner) storeCapFor(ctx context.Context) (int64, bool) {
	c.refreshStoreSizes(ctx)
	window := time.Duration(max(c.cfg.Retention.SnapshotsDays, 1)) * 24 * time.Hour
	dbs, err := histstore.StoreDatabases(ctx, c.pool, time.Now().Add(-window))
	if err != nil {
		c.logFn("WARN", "retention: read the history store's databases for the snapshot "+
			"cap: %v", err)
		return 0, false
	}
	limit := storeCap(dbs, c.cfg.Retention.SnapshotsMaxPct)
	return limit, limit > 0
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// refreshStoreSizes records the current size of every monitored database
// registered in this store.
func (c *Cleaner) refreshStoreSizes(ctx context.Context) {
	for _, reg := range histstore.Registrations() {
		if !reg.Store.Scoped() || reg.Store.DB() != any(c.pool) {
			continue
		}
		q, ok := reg.Monitored.(rowQuerier)
		if !ok {
			continue
		}
		var bytes int64
		if err := q.QueryRow(ctx, `SELECT pg_catalog.pg_database_size(
			pg_catalog.current_database())`).Scan(&bytes); err != nil {
			c.logFn("WARN", "retention: read the size of %s for the history store's cap: %v",
				reg.Name, err)
			continue
		}
		if err := histstore.UpdateDatabaseSize(ctx, reg.Store, bytes); err != nil {
			c.logFn("WARN", "retention: %v", err)
		}
	}
}

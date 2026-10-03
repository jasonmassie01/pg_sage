package autonomy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// Structural scan cadence (measured.md M9, v1.8.3): the scan joins
// pg_attribute with pg_class (299-314 ms and ~512 MB of catalog pages on
// lifeos, ~17 times an hour) and its answer changes only with DDL. It
// reruns when the change counters of the catalogs it reads moved, at most
// every structuralMinInterval, and at least every structuralMaxAge (the
// counters are statistics: reset, disabled or flushed late, the hourly
// floor still catches the change).
const (
	structuralMinInterval = 5 * time.Minute
	structuralMaxAge      = time.Hour
)

// catalogChangeSQL sums the row changes of the catalogs the structural
// scan reads. DDL inserts, updates or deletes their rows; VACUUM and
// ANALYZE update pg_class in place, which is not counted.
const catalogChangeSQL = `/* pg_sage */
SELECT COALESCE(sum(n_tup_ins + n_tup_upd + n_tup_del), 0)::int8
  FROM pg_catalog.pg_stat_sys_tables
 WHERE relid IN ('pg_catalog.pg_class'::pg_catalog.regclass,
                 'pg_catalog.pg_attribute'::pg_catalog.regclass,
                 'pg_catalog.pg_namespace'::pg_catalog.regclass,
                 'pg_catalog.pg_type'::pg_catalog.regclass)`

// structuralCache is the last structural scan: when it ran, the catalog
// change counters then, and its answer.
type structuralCache struct {
	mu        sync.Mutex
	scanned   bool
	at        time.Time
	watermark int64
	items     []schemaguard.Invariant
}

// detectStructuralPathologies returns the structural invariants, from
// the last scan while the catalog has not changed.
func (d postgresSchemaDetector) detectStructuralPathologies(
	ctx context.Context,
) ([]schemaguard.Invariant, error) {
	c := d.structural
	if c == nil {
		return d.scanStructuralPathologies(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var mark int64
	if err := d.pool.QueryRow(ctx, catalogChangeSQL).Scan(&mark); err != nil {
		return nil, fmt.Errorf("read catalog change counters: %w", err)
	}
	now := d.now()
	age := now.Sub(c.at)
	fresh := c.scanned && age < structuralMaxAge &&
		(mark == c.watermark || age < structuralMinInterval)
	if !fresh {
		items, err := d.scanStructuralPathologies(ctx)
		if err != nil {
			return nil, err
		}
		c.scanned, c.at, c.watermark, c.items = true, now, mark, items
	}
	return append([]schemaguard.Invariant(nil), c.items...), nil
}

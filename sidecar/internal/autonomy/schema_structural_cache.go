package autonomy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// Structural scan cadence (measured.md M9, v1.8.3): the scan's answer
// changes only with DDL. A pass runs when the change counters of the
// catalogs it reads moved, at most every structuralMinInterval, and at
// least every structuralMaxAge (the counters are statistics: reset,
// disabled or flushed late, the hourly floor still catches the change).
//
// Incremental passes (v2.3.x): applications that create temporary tables,
// partitions or staging tables move the counters constantly, and every
// pass aggregated every column of every user table (~300 ms on lifeos,
// ~12 times an hour). A pass now keeps each table's column summary by
// oid and summarizes again only the tables that are new or whose pg_class
// row changed. Column DDL that leaves pg_class alone (RENAME COLUMN, DROP
// COLUMN, ALTER COLUMN TYPE without a rewrite) updates pg_attribute rows,
// so a pass after pg_attribute's update counter moved, and the hourly
// floor, also compare every table's column versions (verified). Every
// structuralFullInterval a full pass summarizes every table again.
const (
	structuralMinInterval  = 5 * time.Minute
	structuralMaxAge       = time.Hour
	structuralFullInterval = 24 * time.Hour
)

// catalogChangeSQL sums the row changes of the catalogs the structural
// scan reads, and pg_attribute's updates alone. DDL inserts, updates or
// deletes their rows; VACUUM and ANALYZE update pg_class in place, which
// is not counted.
const catalogChangeSQL = `/* pg_sage */
SELECT COALESCE(sum(n_tup_ins + n_tup_upd + n_tup_del), 0)::int8,
       COALESCE(sum(n_tup_upd) FILTER (
         WHERE relid = 'pg_catalog.pg_attribute'::pg_catalog.regclass), 0)::int8
  FROM pg_catalog.pg_stat_sys_tables
 WHERE relid IN ('pg_catalog.pg_class'::pg_catalog.regclass,
                 'pg_catalog.pg_attribute'::pg_catalog.regclass,
                 'pg_catalog.pg_namespace'::pg_catalog.regclass,
                 'pg_catalog.pg_type'::pg_catalog.regclass)`

// catalogMarks are the catalog change counters: every counted change,
// and pg_attribute's updates.
type catalogMarks struct {
	changes          int64
	attributeUpdates int64
}

// structuralPass is what a cycle does.
type structuralPass int

const (
	// structuralPassNone reuses the last answer.
	structuralPassNone structuralPass = iota
	// structuralPassChanged summarizes new tables and changed pg_class rows.
	structuralPassChanged
	// structuralPassVerified also compares every table's column versions.
	structuralPassVerified
	// structuralPassFull summarizes every table.
	structuralPassFull
)

func (p structuralPass) String() string {
	return [...]string{"none", "changed", "verified", "full"}[p]
}

// structuralCache is the last structural pass: when it ran, when the
// last full pass ran, the catalog change counters then, the tables' column
// summaries and the text types they were counted with, and its answer.
type structuralCache struct {
	mu        sync.Mutex
	scanned   bool
	at        time.Time
	fullAt    time.Time
	marks     catalogMarks
	tables    map[uint32]structuralTable
	textTypes []uint32
	items     []schemaguard.Invariant
}

// plan decides the pass a cycle at now runs given the counters. A clock
// that moved backwards cannot date the cache, so it passes in full.
func (c *structuralCache) plan(now time.Time, marks catalogMarks) structuralPass {
	if !c.scanned || now.Before(c.at) || now.Sub(c.fullAt) >= structuralFullInterval {
		return structuralPassFull
	}
	age := now.Sub(c.at)
	switch {
	case age >= structuralMaxAge:
		return structuralPassVerified
	case marks.changes == c.marks.changes || age < structuralMinInterval:
		return structuralPassNone
	case marks.attributeUpdates != c.marks.attributeUpdates:
		return structuralPassVerified
	}
	return structuralPassChanged
}

// detectStructuralPathologies returns the structural invariants, from
// the last pass while the catalog has not changed. A detector without a
// cache passes in full every time.
func (d postgresSchemaDetector) detectStructuralPathologies(
	ctx context.Context,
) ([]schemaguard.Invariant, error) {
	c := d.structural
	if c == nil {
		next, err := d.readStructuralSnapshot(ctx, structuralPassFull, structuralSnapshot{})
		if err != nil {
			return nil, err
		}
		return structuralInvariants(next.tables), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var marks catalogMarks
	if err := d.pool.QueryRow(ctx, catalogChangeSQL).Scan(&marks.changes,
		&marks.attributeUpdates); err != nil {
		return nil, fmt.Errorf("read catalog change counters: %w", err)
	}
	now := d.now()
	if pass := c.plan(now, marks); pass != structuralPassNone {
		next, err := d.readStructuralSnapshot(ctx, pass,
			structuralSnapshot{tables: c.tables, textTypes: c.textTypes})
		if err != nil {
			return nil, err
		}
		if pass == structuralPassFull {
			c.fullAt = now
		}
		c.scanned, c.at, c.marks = true, now, marks
		c.tables, c.textTypes = next.tables, next.textTypes
		c.items = structuralInvariants(next.tables)
	}
	return append([]schemaguard.Invariant(nil), c.items...), nil
}

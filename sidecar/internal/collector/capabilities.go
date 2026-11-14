package collector

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
)

// Capabilities is what the monitored database can report, re-derived every
// cycle (collection-02): pg_stat_statements created after startup, dropped
// and re-created, updated, or a major upgrade behind the same endpoint (an
// RDS or Aurora upgrade keeps it) is followed without a restart. The
// startup checks only seed it.
type Capabilities struct {
	// PGVersionNum is server_version_num, e.g. 170009 for PG 17.9.
	PGVersionNum int
	// StatStatementsView is whether pg_stat_statements resolves here.
	StatStatementsView bool
	// StatStatementsVersion is the extension's extversion ("" when absent).
	StatStatementsVersion string
	// WALColumns, PlanTimeColumns and BlockIOTime say which optional
	// pg_stat_statements columns each cycle selects.
	WALColumns      bool
	PlanTimeColumns bool
	BlockIOTime     bool
	blk             blockTimeExprs
}

// blockTimeExprs are the per-query block read/write time expressions.
type blockTimeExprs struct{ read, write string }

var zeroBlockTime = blockTimeExprs{read: "0", write: "0"}

// capKey is what the per-cycle probe reads; the columns are probed again
// only when it changes.
type capKey struct {
	version    int
	viewOID    uint32
	extVersion string
	schema     string // pg_stat_statements' schema; "" when not installed
}

// capState is guarded by Collector.mu.
type capState struct {
	caps      Capabilities
	key       capKey
	attempted bool // a refresh has run, successful or not
	probed    bool // a key probe has succeeded
	cached    bool // caps hold the column probe of key
	failing   bool // a probe-failure streak is in progress (warned once)
}

// capabilityKeySQL is the one statement a steady-state cycle spends here.
// It finds the view in the extension's own schema (pg_extension), never
// through the search_path, and returns that schema for the statements
// reads.
const capabilityKeySQL = sageTag + `/* capabilities: key */
SELECT current_setting('server_version_num')::int,
       COALESCE((SELECT c.oid FROM pg_catalog.pg_class c
                  WHERE c.relnamespace = e.extnamespace
                    AND c.relname = 'pg_stat_statements'), 0::oid),
       COALESCE(e.extversion, ''), COALESCE(n.nspname::text, '')
  FROM (SELECT 1) one
  LEFT JOIN pg_catalog.pg_extension e ON e.extname = 'pg_stat_statements'
  LEFT JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace`

// capabilityColumnsSQL finds the optional columns the installed view
// exposes (they depend on the extension version, not only the server's):
// WAL and plan time from 1.8, block I/O time renamed to shared_/local_ in
// 1.11.
const capabilityColumnsSQL = sageTag + `/* capabilities: columns */
SELECT attname::text FROM pg_attribute
 WHERE attrelid = $1::oid AND attnum > 0 AND NOT attisdropped
   AND attname IN ('wal_bytes', 'total_plan_time',
                   'shared_blk_read_time', 'blk_read_time')`

// seedCapabilities are the startup checks' findings, used until the first
// probe succeeds.
func seedCapabilities(version int, cfg *config.Config) Capabilities {
	return Capabilities{
		PGVersionNum:       version,
		StatStatementsView: cfg.HasWALColumns || cfg.HasPlanTimeColumns,
		WALColumns:         cfg.HasWALColumns,
		PlanTimeColumns:    cfg.HasPlanTimeColumns,
		blk:                zeroBlockTime,
	}
}

// Capabilities returns the current capabilities (thread-safe).
func (c *Collector) Capabilities() Capabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.caps.caps
}

func (c *Collector) probeQuerier() catalogQuerier {
	if c.probeDB != nil {
		return c.probeDB
	}
	return timedCatalogQuerier{collector: c}
}

// refreshCapabilities runs the key probe and, when the key changed or the
// last column probe found nothing, the column probe. A failed probe keeps
// the last capabilities.
func (c *Collector) refreshCapabilities(ctx context.Context) {
	c.mu.Lock()
	c.caps.attempted = true
	c.mu.Unlock()
	db := c.probeQuerier()
	var key capKey
	if err := db.QueryRow(ctx, capabilityKeySQL).Scan(
		&key.version, &key.viewOID, &key.extVersion, &key.schema); err != nil {
		c.capabilityProbeFailed("server version and pg_stat_statements", err)
		return
	}
	c.mu.RLock()
	unchanged := c.caps.cached && c.caps.key == key
	c.mu.RUnlock()
	if unchanged {
		c.capabilityProbeSucceeded()
		return
	}
	next, cached, err := deriveCapabilities(ctx, db, key)
	if err != nil {
		c.capabilityProbeFailed("pg_stat_statements columns", err)
		return
	}
	c.storeCapabilities(key, next, cached)
}

// ReprobeCapabilities runs the capability probe now, outside the cycle,
// and returns the capabilities it leaves (the last ones when it fails).
// The first look re-check calls it so its report and the collector agree.
func (c *Collector) ReprobeCapabilities(ctx context.Context) Capabilities {
	c.refreshCapabilities(ctx)
	return c.Capabilities()
}

// queryCapabilities are the capabilities for a statements read; a read
// before any refresh (a direct call) probes first. After a failed refresh
// it keeps the last capabilities: one timeout per cycle, not two.
func (c *Collector) queryCapabilities(ctx context.Context) Capabilities {
	c.mu.RLock()
	attempted := c.caps.attempted
	c.mu.RUnlock()
	if !attempted {
		c.refreshCapabilities(ctx)
	}
	return c.Capabilities()
}

// invalidateCapabilities forces the next refresh to re-derive everything.
// It keeps pg_stat_statements' schema: the statements reads before that
// refresh still name the view where the last probe found it.
func (c *Collector) invalidateCapabilities() {
	c.mu.Lock()
	c.caps.key, c.caps.cached = capKey{schema: c.caps.key.schema}, false
	c.mu.Unlock()
}

// deriveCapabilities builds the capabilities of key. A probe that found no
// view (or no column) is not cached: the next refresh probes again.
func deriveCapabilities(ctx context.Context, db catalogQuerier,
	key capKey) (Capabilities, bool, error) {
	caps := Capabilities{PGVersionNum: key.version, StatStatementsView: key.viewOID != 0,
		StatStatementsVersion: key.extVersion, blk: zeroBlockTime}
	if key.viewOID == 0 {
		return caps, false, nil
	}
	cols, err := statementColumns(ctx, db, key.viewOID)
	if err != nil {
		return Capabilities{}, false, err
	}
	caps.WALColumns = cols["wal_bytes"]
	caps.PlanTimeColumns = cols["total_plan_time"]
	caps.blk = blockTimeFor(cols)
	caps.BlockIOTime = caps.blk != zeroBlockTime
	return caps, len(cols) > 0, nil
}

func statementColumns(ctx context.Context, db catalogQuerier,
	viewOID uint32) (map[string]bool, error) {
	rows, err := db.Query(ctx, capabilityColumnsSQL, viewOID)
	if err != nil {
		return nil, fmt.Errorf("probe pg_stat_statements columns: %w", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan pg_stat_statements column: %w", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("probe pg_stat_statements columns: %w", err)
	}
	return cols, nil
}

// blockTimeFor picks the block I/O time columns (G1-B16):
// pg_stat_statements 1.11 (PG17) split blk_read_time into
// shared_/local_blk_read_time; without either the time reads zero.
func blockTimeFor(cols map[string]bool) blockTimeExprs {
	switch {
	case cols["shared_blk_read_time"]:
		return blockTimeExprs{
			read:  "shared_blk_read_time + local_blk_read_time",
			write: "shared_blk_write_time + local_blk_write_time",
		}
	case cols["blk_read_time"]:
		return blockTimeExprs{read: "blk_read_time", write: "blk_write_time"}
	}
	return zeroBlockTime
}

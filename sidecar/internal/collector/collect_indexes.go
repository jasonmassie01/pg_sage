package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// collectIndexes pages through pg_stat_user_indexes by indexrelid (see
// collectTables; lifeos: 35,439 indexes). Definitions come from the
// collector's cache and are read only for new or changed indexes
// (measured.md M3: pg_get_indexdef for 35k indexes every minute); the
// ExactSizeTopN largest indexes are measured exactly.
func (c *Collector) collectIndexes(ctx context.Context) ([]IndexStats, error) {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	if c.indexDefs == nil {
		c.indexDefs = newIndexDefCache()
	}
	var watermark int64
	if err := c.catalogQueryRow(ctx, indexDefWatermarkSQL).Scan(&watermark); err != nil {
		return nil, fmt.Errorf("collect indexes: read catalog update counters: %w", err)
	}
	c.indexDefs.beginPass(watermark, c.clock(), c.defMaxAge())
	scratch := &scratchConn{pool: c.pool}
	defer func() {
		if err := scratch.close(); err != nil {
			c.logFn("WARN", "collector: %v", err)
		}
	}()
	batchSize := collectorBatchSize(c.cfg)
	var result []IndexStats
	var cursor uint32
	for {
		batch, err := c.collectIndexBatch(ctx, scratch, cursor, batchSize)
		if err != nil {
			return nil, fmt.Errorf("collect indexes: %w", err)
		}
		result = append(result, batch...)
		if len(batch) < batchSize {
			break
		}
		cursor = batch[len(batch)-1].IndexRelID
	}
	c.indexDefs.endPass()
	c.refineIndexSizes(ctx, result)
	return result, nil
}

func (c *Collector) defMaxAge() time.Duration {
	if c.indexDefMaxAge <= 0 {
		return IndexDefMaxAge
	}
	return c.indexDefMaxAge
}

// collectIndexBatch reads one page and fills in definitions, fetching
// only those the cache does not hold at the page's catalog version.
func (c *Collector) collectIndexBatch(
	ctx context.Context, scratch *scratchConn, cursor uint32, batchSize int,
) ([]IndexStats, error) {
	batch, versions, err := c.readIndexPage(ctx, cursor, batchSize)
	if err != nil {
		return nil, err
	}
	var missing []uint32
	missingAt := map[uint32]int{}
	for i := range batch {
		oid := batch[i].IndexRelID
		if def, ok := c.indexDefs.lookup(oid, versions[i]); ok {
			batch[i].IndexDef = def
			continue
		}
		missing = append(missing, oid)
		missingAt[oid] = i
	}
	if len(missing) == 0 {
		return batch, nil
	}
	var db catalogread.Beginner = c.pool
	if len(missing) >= indexDefScratchMin {
		conn, err := scratch.get(ctx)
		if err != nil {
			return nil, err
		}
		db = conn
	}
	defs, err := c.fetchIndexDefs(ctx, db, missing)
	if err != nil {
		return nil, err
	}
	for oid, def := range defs {
		i, ok := missingAt[oid]
		if !ok {
			continue
		}
		batch[i].IndexDef = def
		c.indexDefs.store(oid, versions[i], def)
	}
	return batch, nil
}

func (c *Collector) readIndexPage(
	ctx context.Context, cursor uint32, batchSize int,
) ([]IndexStats, []string, error) {
	rows, err := c.catalogQuery(ctx, indexStatsSQL, cursor, batchSize)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var result []IndexStats
	var versions []string
	for rows.Next() {
		var idx IndexStats
		var version string
		if err := rows.Scan(
			&idx.SchemaName, &idx.RelName, &idx.IndexRelName,
			&idx.IdxScan, &idx.IdxTupRead, &idx.IdxTupFetch,
			&idx.IndexBytes,
			&idx.IsUnique, &idx.IsPrimary, &idx.IsValid,
			&idx.IndexType, &idx.IndexRelID, &idx.LastIdxScan, &version,
		); err != nil {
			return nil, nil, err
		}
		result = append(result, idx)
		versions = append(versions, version)
	}
	return result, versions, rows.Err()
}

func (c *Collector) fetchIndexDefs(
	ctx context.Context, db catalogread.Beginner, oids []uint32,
) (map[uint32]string, error) {
	rows, err := c.catalogQueryVia(ctx, db, indexDefsSQL, oids)
	if err != nil {
		return nil, fmt.Errorf("index definitions: %w", err)
	}
	defer rows.Close()
	out := make(map[uint32]string, len(oids))
	for rows.Next() {
		var oid uint32
		var def string
		if err := rows.Scan(&oid, &def); err != nil {
			return nil, fmt.Errorf("index definitions: %w", err)
		}
		out[oid] = def
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index definitions: %w", err)
	}
	return out, nil
}

// refineIndexSizes measures the largest indexes exactly (best effort).
func (c *Collector) refineIndexSizes(ctx context.Context, indexes []IndexStats) {
	oids := make([]uint32, len(indexes))
	bytes := make([]int64, len(indexes))
	at := make(map[uint32]int, len(indexes))
	for i, idx := range indexes {
		oids[i], bytes[i], at[idx.IndexRelID] = idx.IndexRelID, idx.IndexBytes, i
	}
	top := topByBytes(oids, bytes, c.exactTopN)
	if len(top) == 0 {
		return
	}
	exact, err := c.exactSizes(ctx, exactIndexSizesSQL, top, 1)
	if err != nil {
		c.logFn("WARN", "collector: exact index sizes skipped this cycle "+
			"(relpages estimates kept): %v", err)
		return
	}
	for oid, v := range exact {
		indexes[at[oid]].IndexBytes = v[0]
	}
}

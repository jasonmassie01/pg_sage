package collector

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// recordQueryStore writes per-queryid samples to sage.query_store so
// windowed latency can be computed for verify-and-revert (F1) and
// plan-regression detection (A5). Non-fatal on error.
func (c *Collector) recordQueryStore(ctx context.Context, snap *Snapshot) {
	if len(snap.Queries) == 0 {
		return
	}
	samples := make([]querystore.Sample, 0, len(snap.Queries))
	for _, q := range snap.Queries {
		if q.QueryID == 0 {
			continue
		}
		samples = append(samples, querystore.Sample{
			QueryID:     q.QueryID,
			Calls:       q.Calls,
			TotalExecMs: q.TotalExecTime,
			MeanExecMs:  q.MeanExecTime,
			Rows:        q.Rows,
			StatsEpoch:  snap.StatsEpoch,
		})
	}
	if err := querystore.Record(ctx, c.pool, samples); err != nil {
		c.logFn("WARN", "query_store record failed: %v", err)
	}
}

// blockTimeExprs are the per-query block read/write time expressions.
type blockTimeExprs struct{ read, write string }

// blockTimeColumns picks the block I/O time columns the installed
// pg_stat_statements exposes (G1-B16). pg_stat_statements 1.11 (PG17)
// split blk_read_time into shared_/local_blk_read_time; older versions
// expose blk_read_time. The answer is cached after a successful probe.
func (c *Collector) blockTimeColumns(ctx context.Context) blockTimeExprs {
	c.mu.RLock()
	cached := c.blkTime
	c.mu.RUnlock()
	if cached != nil {
		return *cached
	}
	exprs := blockTimeExprs{read: "0", write: "0"}
	rows, err := c.catalogQuery(ctx, blockTimeColumnsSQL)
	if err != nil {
		c.logFn("WARN", "probe pg_stat_statements block time columns: %v", err)
		return exprs
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			c.logFn("WARN", "scan pg_stat_statements column: %v", err)
			return exprs
		}
		switch name {
		case "shared_blk_read_time":
			return c.cacheBlockTime(blockTimeExprs{
				read:  "shared_blk_read_time + local_blk_read_time",
				write: "shared_blk_write_time + local_blk_write_time",
			})
		case "blk_read_time":
			exprs = blockTimeExprs{read: "blk_read_time", write: "blk_write_time"}
		}
	}
	if err := rows.Err(); err != nil {
		c.logFn("WARN", "probe pg_stat_statements block time columns: %v", err)
		return blockTimeExprs{read: "0", write: "0"}
	}
	return c.cacheBlockTime(exprs)
}

func (c *Collector) cacheBlockTime(exprs blockTimeExprs) blockTimeExprs {
	c.mu.Lock()
	c.blkTime = &exprs
	c.mu.Unlock()
	return exprs
}

// queryStatsTemplate picks the SQL variant for the available columns.
func queryStatsTemplate(hasWAL, hasPlan bool) string {
	switch {
	case hasWAL && hasPlan:
		return queryStatsWithWALAndPlanTimeSQL
	case hasWAL:
		return queryStatsWithWALSQL
	case hasPlan:
		return queryStatsWithPlanTimeSQL
	}
	return queryStatsSQL
}

func (c *Collector) collectQueries(ctx context.Context) ([]QueryStats, error) {
	hasWAL := c.cfg.HasWALColumns
	hasPlan := c.cfg.HasPlanTimeColumns
	tpl := queryStatsTemplate(hasWAL, hasPlan)

	limit := c.cfg.Collector.MaxQueries
	if limit <= 0 {
		limit = 500
	}
	blk := c.blockTimeColumns(ctx)
	sql := fmt.Sprintf(tpl, blk.read, blk.write, limit)

	rows, err := c.catalogQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []QueryStats
	for rows.Next() {
		var q QueryStats
		dest := []any{
			&q.QueryID, &q.Query, &q.Calls,
			&q.TotalExecTime, &q.MeanExecTime, &q.MinExecTime, &q.MaxExecTime,
			&q.StddevExecTime, &q.Rows,
			&q.SharedBlksHit, &q.SharedBlksRead,
			&q.SharedBlksDirtied, &q.SharedBlksWritten,
			&q.TempBlksRead, &q.TempBlksWritten,
			&q.BlkReadTime, &q.BlkWriteTime,
		}
		if hasWAL {
			dest = append(dest, &q.WALRecords, &q.WALFpi, &q.WALBytes)
		}
		if hasPlan {
			dest = append(dest, &q.TotalPlanTime, &q.MeanPlanTime)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		result = append(result, q)
	}
	return result, rows.Err()
}

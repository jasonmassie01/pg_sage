package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// recordQueryStore writes per-queryid samples to sage.query_store so
// windowed latency can be computed for verify-and-revert (F1) and
// plan-regression detection (A5): one statement per cycle, holding only
// the queries whose counters moved (and an hourly keyframe of the rest).
// Non-fatal on error.
func (c *Collector) recordQueryStore(ctx context.Context, snap *Snapshot) {
	if len(snap.Queries) == 0 {
		return
	}
	c.ensurePartitions(ctx, time.Now())
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
	if _, err := c.queries.Record(ctx, c.pool, samples, snap.CollectedAt); err != nil {
		c.logFn("WARN", "query_store record failed: %v", err)
	}
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
	caps := c.queryCapabilities(ctx)
	hasWAL, hasPlan := caps.WALColumns, caps.PlanTimeColumns
	tpl := queryStatsTemplate(hasWAL, hasPlan)

	limit := c.cfg.Collector.MaxQueries
	if limit <= 0 {
		limit = 500
	}
	sql := fmt.Sprintf(tpl, caps.blk.read, caps.blk.write, limit)

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

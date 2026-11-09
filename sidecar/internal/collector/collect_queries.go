package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
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

// collectQueries reads the top MaxQueries application statements of this
// database: counters first, texts from the cache (statements_text.go).
func (c *Collector) collectQueries(ctx context.Context) ([]QueryStats, error) {
	return c.collectQueriesVia(ctx, timedCatalogQuerier{collector: c})
}

// collectQueriesVia is collectQueries over db. The counters are not
// published without their texts: a failed text read fails the step.
func (c *Collector) collectQueriesVia(ctx context.Context,
	db catalogQuerier) ([]QueryStats, error) {
	caps := c.queryCapabilities(ctx)
	limit := c.maxQueries()
	candidates, err := readQueryCounters(ctx, db, caps, limit)
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_statements counters: %w", err)
	}
	ids := make([]int64, len(candidates))
	for i, q := range candidates {
		ids[i] = q.QueryID
	}
	texts, err := c.stmtTexts.resolve(ctx, db, ids, c.clock(),
		stmtTextEntriesPerQuery*limit)
	if err != nil {
		return nil, err
	}
	return applicationQueries(candidates, texts, limit), nil
}

// maxQueries is collector.max_queries, 500 when unset or invalid.
func (c *Collector) maxQueries() int {
	if limit := c.cfg.Collector.MaxQueries; limit > 0 {
		return limit
	}
	return 500
}

// candidateHeadroom x MaxQueries statements are ranked, so that MaxQueries
// remain once pg_sage's own are left out.
const candidateHeadroom = 2

// readQueryCounters reads the top candidateHeadroom x limit queryids by total time, with
// their counters and without their texts.
func readQueryCounters(ctx context.Context, db catalogQuerier, caps Capabilities,
	limit int) ([]QueryStats, error) {
	hasWAL, hasPlan := caps.WALColumns, caps.PlanTimeColumns
	sql := fmt.Sprintf(queryStatsTemplate(hasWAL, hasPlan), caps.blk.read, caps.blk.write,
		candidateHeadroom*limit)
	rows, err := db.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []QueryStats
	for rows.Next() {
		var q QueryStats
		if err := rows.Scan(queryCounterDest(&q, hasWAL, hasPlan)...); err != nil {
			return nil, err
		}
		result = append(result, q)
	}
	return result, rows.Err()
}

// queryCounterDest is the scan targets of one queryStatsSelect row.
func queryCounterDest(q *QueryStats, hasWAL, hasPlan bool) []any {
	dest := []any{
		&q.QueryID, &q.Calls,
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
	return dest
}

// applicationQueries keeps, in rank order, the first limit candidates whose
// text is known and not pg_sage's own (selfmonitor.IsStatementText, the
// StatementExclusionSQL predicate). A candidate without a text cannot be
// attributed and is left out. The result is empty, not nil, when none
// remains.
func applicationQueries(candidates []QueryStats, texts map[int64]string,
	limit int) []QueryStats {
	out := make([]QueryStats, 0, min(len(candidates), limit))
	for _, q := range candidates {
		text, ok := texts[q.QueryID]
		if !ok || selfmonitor.IsStatementText(text) {
			continue
		}
		q.Query = text
		out = append(out, q)
		if len(out) == limit {
			break
		}
	}
	return out
}

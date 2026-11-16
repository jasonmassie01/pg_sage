package collector

import (
	"cmp"
	"context"
	"fmt"
	"slices"
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
		c.noteQueryStoreFailure(err)
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

// collectQueries reads the MaxQueries application statements of this
// database the ranked read admits: counters first, texts from the cache
// (statements_text.go).
func (c *Collector) collectQueries(ctx context.Context) ([]QueryStats, error) {
	return c.queriesVia(ctx, timedCatalogQuerier{collector: c}, nil)
}

// collectQueriesVia is collectQueries over db.
func (c *Collector) collectQueriesVia(ctx context.Context,
	db catalogQuerier) ([]QueryStats, error) {
	return c.queriesVia(ctx, db, nil)
}

// queriesVia reads the queries over db and, when totals is not nil, the
// database's statement totals the same read carries (evid-01a); they are
// unknown (zero) when the read fails or returns no row. The counters are
// not published without their texts: a failed text read fails the step.
func (c *Collector) queriesVia(ctx context.Context, db catalogQuerier,
	totals *statementTotals) ([]QueryStats, error) {
	var read statementTotals
	if totals != nil {
		*totals = statementTotals{}
	}
	caps := c.queryCapabilities(ctx)
	names, err := c.pgss()
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_statements counters: %w", err)
	}
	admission := admissionFor(c.maxQueries())
	ranked := rankedRead{admission: admission, totals: &read,
		pins: c.pinnedQueryIDs(ctx, admission.pinCap), own: c.stmtTexts.ownIDs()}
	candidates, err := readQueryCounters(ctx, db, names.fn, caps, ranked)
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_statements counters: %w", err)
	}
	ids := make([]int64, len(candidates))
	for i, q := range candidates {
		ids[i] = q.QueryID
	}
	texts, err := c.stmtTexts.resolve(ctx, db, names.fn, ids, c.clock(),
		stmtTextEntriesPerQuery*admission.limit)
	if err != nil {
		return nil, err
	}
	if totals != nil {
		*totals = read
	}
	return applicationQueries(candidates, texts, admission.limit), nil
}

// maxQueries is collector.max_queries, 500 when unset or invalid.
func (c *Collector) maxQueries() int {
	if limit := c.cfg.Collector.MaxQueries; limit > 0 {
		return limit
	}
	return 500
}

// candidateHeadroom x MaxQueries statements are read, so that MaxQueries
// remain once pg_sage's own are left out.
const candidateHeadroom = 2

// rankedRead is one cycle's admission: the arm sizes, the pinned queryids
// and the queryids known to be pg_sage's own (left out of the mean and io
// arms). Both lists are sent as arrays, empty rather than NULL.
type rankedRead struct {
	admission queryAdmission
	pins, own []int64
	// totals, when set, receives the statement totals every row carries.
	totals *statementTotals
}

// statementTotals are the database's pg_stat_statements totals over its
// entries less pg_sage's known own (evid-01a); known is false when no row
// carried them.
type statementTotals struct {
	execMs  float64
	calls   int64
	entries int
	known   bool
}

// applyStatementTotals copies the statements epoch and, when known, the
// queries step's statement totals into the system category, after the
// steps (the system step replaces System).
func applyStatementTotals(snap *Snapshot) {
	snap.System.StatementsEpoch = snap.StatsEpoch
	t := snap.stmtTotals
	if !t.known {
		t = statementTotals{}
	}
	snap.System.StatementsTotalExecMs = t.execMs
	snap.System.StatementsCalls = t.calls
	snap.System.StatementsEntries = t.entries
}

// readQueryCounters reads the admitted queryids (queryStatsFrom), pinned
// first, with their counters and admission and without their texts. fn
// is the qualified pg_stat_statements function (the template's %[6]s).
func readQueryCounters(ctx context.Context, db catalogQuerier, fn string,
	caps Capabilities, read rankedRead) ([]QueryStats, error) {
	hasWAL, hasPlan := caps.WALColumns, caps.PlanTimeColumns
	pins, own := nonNil(read.pins), nonNil(read.own)
	a := read.admission
	sql := fmt.Sprintf(queryStatsTemplate(hasWAL, hasPlan), caps.blk.read, caps.blk.write,
		a.total(len(pins)), a.perArm, a.candidates(len(pins)), fn)
	rows, err := db.Query(ctx, sql, pins, own)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []QueryStats
	var t statementTotals
	for rows.Next() {
		var q QueryStats
		dest := append(queryCounterDest(&q, hasWAL, hasPlan),
			&t.execMs, &t.calls, &t.entries, &q.AdmittedBy)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		result = append(result, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if read.totals != nil && len(result) > 0 {
		t.known = true
		*read.totals = t
	}
	return result, nil
}

// nonNil is ids, or an empty slice for nil: pgx sends nil as NULL, and
// "queryid <> ALL(NULL)" is never true.
func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
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

// applicationQueries keeps, in admission order (pinned, then the other
// arms, then by total time), the first limit candidates whose text is
// known and not pg_sage's own (selfmonitor.IsStatementText, the
// StatementExclusionSQL predicate), and returns them by total time, as
// before collection-07. A candidate without a text cannot be attributed
// and is left out. The result is empty, not nil, when none remains.
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
	slices.SortStableFunc(out, func(a, b QueryStats) int {
		return cmp.Compare(b.TotalExecTime, a.TotalExecTime)
	})
	return out
}

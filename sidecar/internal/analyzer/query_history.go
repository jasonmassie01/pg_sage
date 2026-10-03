package analyzer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// maxHistorySamples caps how many 'queries' snapshots the regression
// baseline samples, however long the lookback.
const maxHistorySamples = 100

// historyDecodeBatch bounds the snapshots one statement decodes.
const historyDecodeBatch = 16

// defaultRegressionLookbackDays applies when the configured lookback is
// zero or negative (it used to read nothing at all).
const defaultRegressionLookbackDays = 7

// The regression baseline (Phase 0 item 8, performance gate offender 4 in
// v1.8.3) samples the lookback's non-empty 'queries' snapshots: every one
// when there are at most maxHistorySamples, otherwise the first of each
// time bucket (lookback / maxHistorySamples wide, on fixed boundaries).
// Either way a snapshot stays in the sample until it leaves the window,
// so each is decoded (a delta row rebuilt against its keyframe in
// PL/pgSQL, ~1.35 ms for 60 statements) once and remembered: a steady
// cycle decodes only the snapshots it has not seen. The sample is found
// by index (idx_snapshots_category), never by ranking the window.

// historyListSQL lists the window's first $2 non-empty snapshots.
var historyListSQL = `/* pg_sage */
SELECT s.id FROM sage.snapshots s
 WHERE s.category = 'queries' AND s.collected_at > $1
   AND ` + snapstore.NonEmptySQL("s") + `
 ORDER BY s.collected_at, s.id LIMIT $2`

// historyBucketsSQL picks the first non-empty snapshot of each bucket.
var historyBucketsSQL = `/* pg_sage */
SELECT p.id
  FROM unnest($2::timestamptz[], $3::timestamptz[]) AS b(lo, hi)
 CROSS JOIN LATERAL (
       SELECT s.id FROM sage.snapshots s
        WHERE s.category = 'queries' AND s.collected_at >= b.lo
          AND s.collected_at < b.hi AND s.collected_at > $1
          AND ` + snapstore.NonEmptySQL("s") + `
        ORDER BY s.collected_at, s.id LIMIT 1) p
 ORDER BY b.lo`

// historyDecodeSQL reads the (queryid, mean_exec_time) pairs of the
// snapshots $1 (each rebuilt through the snapshot accessor; a delta whose
// keyframe is gone reads NULL). Documents that are not arrays and
// elements without an integer queryid and a numeric mean_exec_time are
// skipped.
var historyDecodeSQL = `/* pg_sage */
WITH d AS MATERIALIZED (
    SELECT s.id, ` + snapstore.DataSQL("s") + ` AS doc
      FROM sage.snapshots s WHERE s.id = ANY($1::int8[])
)
SELECT d.id, (e->>'queryid')::bigint, (e->>'mean_exec_time')::float8
  FROM d
 CROSS JOIN LATERAL jsonb_array_elements(
           CASE WHEN jsonb_typeof(d.doc) = 'array' THEN d.doc
                ELSE '[]'::jsonb END) AS e
 WHERE (e->>'queryid') ~ '^-?[0-9]+$'
   AND jsonb_typeof(e->'mean_exec_time') = 'number'`

// queryMean is one statement's mean execution time in one snapshot.
type queryMean struct {
	qid  int64
	mean float64
}

// historyCache remembers the decoded snapshots of the current sample.
type historyCache struct {
	mu    sync.Mutex
	picks map[int64][]queryMean
}

// buildHistoricalAverages returns the per-queryid average mean_exec_time
// over the sampled snapshots of the regression lookback. On a query error
// it marks query_regression failed and returns nil.
func (a *Analyzer) buildHistoricalAverages(
	ctx context.Context,
) map[int64]float64 {
	days := a.cfg.Analyzer.RegressionLookbackDays
	if days <= 0 {
		days = defaultRegressionLookbackDays
	}
	avgs, err := a.history.averages(ctx, a, time.Duration(days)*24*time.Hour)
	if err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: history query: %v", err)
		return nil
	}
	return avgs
}

// averages samples the lookback, decodes the snapshots not yet seen and
// averages each queryid over the sample.
func (h *historyCache) averages(ctx context.Context, a *Analyzer,
	lookback time.Duration) (map[int64]float64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	picks, err := samplePicks(ctx, a, lookback)
	if err != nil {
		return nil, err
	}
	kept := make(map[int64][]queryMean, len(picks))
	var missing []int64
	for _, id := range picks {
		if v, ok := h.picks[id]; ok {
			kept[id] = v
		} else {
			missing = append(missing, id)
		}
	}
	for start := 0; start < len(missing); start += historyDecodeBatch {
		batch := missing[start:min(start+historyDecodeBatch, len(missing))]
		if err := decodeHistory(ctx, a, batch, kept); err != nil {
			return nil, err
		}
	}
	h.picks = kept
	sums, counts := map[int64]float64{}, map[int64]int{}
	for _, id := range picks {
		for _, m := range kept[id] {
			sums[m.qid] += m.mean
			counts[m.qid]++
		}
	}
	avgs := make(map[int64]float64, len(sums))
	for qid, sum := range sums {
		avgs[qid] = sum / float64(counts[qid])
	}
	return avgs, nil
}

// samplePicks returns the sample's snapshot ids, oldest first.
func samplePicks(ctx context.Context, a *Analyzer, lookback time.Duration) ([]int64,
	error) {
	now := time.Now()
	since := now.Add(-lookback)
	all, err := queryIDs(ctx, a, historyListSQL, since, maxHistorySamples+1)
	if err != nil || len(all) <= maxHistorySamples {
		return all, err
	}
	lo, hi := historyBuckets(since, now, lookback/maxHistorySamples)
	return queryIDs(ctx, a, historyBucketsSQL, since, lo, hi)
}

// historyBuckets are the buckets of width covering (since, now], at most
// maxHistorySamples of them (the oldest, partial one is dropped first).
func historyBuckets(since, now time.Time, width time.Duration) ([]time.Time,
	[]time.Time) {
	var lo, hi []time.Time
	for t := since.Truncate(width); t.Before(now); t = t.Add(width) {
		lo, hi = append(lo, t), append(hi, t.Add(width))
	}
	if n := len(lo) - maxHistorySamples; n > 0 {
		lo, hi = lo[n:], hi[n:]
	}
	return lo, hi
}

func queryIDs(ctx context.Context, a *Analyzer, sql string, args ...any) ([]int64,
	error) {
	rows, err := a.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan history snapshot id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// decodeHistory reads the batch's (queryid, mean) pairs into kept; a
// snapshot without a usable element is kept as decoded and empty.
func decodeHistory(ctx context.Context, a *Analyzer, batch []int64,
	kept map[int64][]queryMean) error {
	for _, id := range batch {
		kept[id] = nil
	}
	rows, err := a.pool.Query(ctx, historyDecodeSQL, batch)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var m queryMean
		if err := rows.Scan(&id, &m.qid, &m.mean); err != nil {
			return fmt.Errorf("scan history: %w", err)
		}
		kept[id] = append(kept[id], m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate history: %w", err)
	}
	return nil
}

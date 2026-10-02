package analyzer

import (
	"context"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// maxHistorySamples caps how many 'queries' snapshots the regression
// baseline expands per cycle, however long the lookback.
const maxHistorySamples = 100

// defaultRegressionLookbackDays applies when the configured lookback is
// zero or negative (it used to read nothing at all).
const defaultRegressionLookbackDays = 7

// historicalAveragesSQL is the regression baseline, bounded in SQL (Phase
// 0 item 8, like the forecaster fix). It numbers the lookback's non-empty
// snapshots by time reading only ids, timestamps and the cheap emptiness
// test (no full document is detoasted), keeps every ceil(total/$2)-th one,
// i.e. at most $2 evenly spaced snapshots, and decodes only those through
// the snapshot accessor (a delta row is rebuilt against its keyframe; a
// row whose keyframe is gone reads NULL and is skipped). Documents that
// are not arrays and elements without an integer queryid and a numeric
// mean_exec_time are skipped.
var historicalAveragesSQL = `/* pg_sage */
WITH ranked AS (
    SELECT s.id,
           row_number() OVER (ORDER BY s.collected_at, s.id) AS rn,
           count(*) OVER () AS total
      FROM sage.snapshots s
     WHERE s.category = 'queries'
       AND s.collected_at > now() - make_interval(days => $1)
       AND ` + snapstore.NonEmptySQL("s") + `
), picked AS (
    SELECT id FROM ranked
     WHERE (rn - 1) % GREATEST(1, ceil(total::numeric / $2::int)::bigint) = 0
), docs AS (
    SELECT ` + snapstore.DataSQL("s") + ` AS doc
      FROM sage.snapshots s
      JOIN picked p ON p.id = s.id
)
SELECT (e->>'queryid')::bigint, avg((e->>'mean_exec_time')::float8)
  FROM docs d
 CROSS JOIN LATERAL jsonb_array_elements(
           CASE WHEN jsonb_typeof(d.doc) = 'array' THEN d.doc
                ELSE '[]'::jsonb END) AS e
 WHERE (e->>'queryid') ~ '^-?[0-9]+$'
   AND jsonb_typeof(e->'mean_exec_time') = 'number'
 GROUP BY 1`

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
	rows, err := a.pool.Query(ctx, historicalAveragesSQL, days, maxHistorySamples)
	if err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: history query: %v", err)
		return nil
	}
	defer rows.Close()
	avgs := make(map[int64]float64)
	for rows.Next() {
		var qid int64
		var avg float64
		if err := rows.Scan(&qid, &avg); err != nil {
			a.evalFail("query_regression")
			a.logFn("ERROR", "analyzer: scan history: %v", err)
			return nil
		}
		avgs[qid] = avg
	}
	if err := rows.Err(); err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: iterate history: %v", err)
		return nil
	}
	return avgs
}

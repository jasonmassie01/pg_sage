package analyzer

import (
	"context"
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// historySamples is how many 'queries' snapshots the regression baseline
// averages over, evenly spaced across the lookback.
const historySamples = 100

// buildHistoricalAverages computes, per queryid, the average
// mean_exec_time over historySamples evenly spaced 'queries' snapshots of
// the regression lookback. Only the picked snapshots are read, through the
// snapshot accessor (a delta row is decoded against its keyframe).
func (a *Analyzer) buildHistoricalAverages(ctx context.Context) map[int64]float64 {
	ids, ok := a.queryHistoryIDs(ctx)
	if !ok {
		return nil
	}
	docs, ok := a.queryHistoryDocs(ctx, downsample(ids, historySamples))
	if !ok {
		return nil
	}
	type queryEntry struct {
		QueryID        int64   `json:"queryid"`
		MeanExecTimeMs float64 `json:"mean_exec_time"` // collector.QueryStats field (G1-B06)
	}
	sums := make(map[int64]float64)
	counts := make(map[int64]int)
	for _, data := range docs {
		var entries []queryEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			a.logFn("WARN", "analyzer: skip unreadable queries snapshot: %v", err)
			continue
		}
		for _, e := range entries {
			sums[e.QueryID] += e.MeanExecTimeMs
			counts[e.QueryID]++
		}
	}
	avgs := make(map[int64]float64, len(sums))
	for qid, sum := range sums {
		if c := counts[qid]; c > 0 {
			avgs[qid] = sum / float64(c)
		}
	}
	return avgs
}

// queryHistoryIDs lists the lookback's 'queries' snapshot ids, newest
// first. A failure fails the query_regression evaluation.
func (a *Analyzer) queryHistoryIDs(ctx context.Context) ([]int64, bool) {
	rows, err := a.pool.Query(ctx,
		`/* pg_sage */ SELECT id FROM sage.snapshots
		 WHERE category = 'queries'
		   AND collected_at > now() - make_interval(days => $1)
		 ORDER BY collected_at DESC, id DESC`,
		a.cfg.Analyzer.RegressionLookbackDays,
	)
	if err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: history query: %v", err)
		return nil, false
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			a.evalFail("query_regression")
			a.logFn("ERROR", "analyzer: scan history id: %v", err)
			return nil, false
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: iterate history: %v", err)
		return nil, false
	}
	return ids, true
}

// queryHistoryDocs reads the documents of ids, in the order of ids. A row
// whose keyframe is gone (NULL) is skipped.
func (a *Analyzer) queryHistoryDocs(ctx context.Context, ids []int64) ([][]byte, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	rows, err := a.pool.Query(ctx, `/* pg_sage */ SELECT id, `+snapstore.DataSQL("")+
		` FROM sage.snapshots WHERE id = ANY($1)`, ids)
	if err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: history documents: %v", err)
		return nil, false
	}
	defer rows.Close()
	byID := make(map[int64][]byte, len(ids))
	for rows.Next() {
		var id int64
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			a.evalFail("query_regression")
			a.logFn("ERROR", "analyzer: scan history document: %v", err)
			return nil, false
		}
		byID[id] = data
	}
	if err := rows.Err(); err != nil {
		a.evalFail("query_regression")
		a.logFn("ERROR", "analyzer: iterate history documents: %v", err)
		return nil, false
	}
	docs := make([][]byte, 0, len(ids))
	for _, id := range ids {
		if data := byID[id]; data != nil {
			docs = append(docs, data)
		}
	}
	return docs, true
}

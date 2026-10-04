package shadow

import (
	"context"
	"regexp"
	"time"

	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The what-if source (c): an index create whose targeted queries still
// run is planned with and without a hypothetical index (HypoPG, the
// optimizer's own session: savepoint per query, bounded statement
// timeout). It plans the queries; it never builds an index. Errors make
// no fact: the decision waits for other evidence or the horizon.

var concurrently = regexp.MustCompile(`(?i)\s+concurrently\b`)

// queryTextsSQL reads the targets' text and cost from pg_stat_statements.
const queryTextsSQL = `/* pg_sage */ SELECT s.queryid, min(s.query), sum(s.calls)::bigint,
	sum(s.total_exec_time)::float8
	FROM pg_stat_statements s
	WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
	  AND s.queryid = ANY($1)
	GROUP BY s.queryid`

// evaluate is p's what-if, nil when it cannot be evaluated (budget spent,
// targets idle or unknown, HypoPG absent, an error).
func (w *whatIf) evaluate(ctx context.Context, p pendingDecision) *hypoFact {
	if w.budget <= 0 {
		return nil
	}
	running := w.stillRunning(ctx, p)
	if len(running) == 0 {
		return nil
	}
	queries := w.queries(ctx, running)
	if len(queries) == 0 || !w.hypo.IsAvailable(ctx) {
		return nil
	}
	w.budget--
	ddl := concurrently.ReplaceAllString(p.SQL, "")
	res, err := w.hypo.Validate(ctx, optimizer.Recommendation{DDL: ddl}, queries)
	verdict, reason := optimizer.WhatIfVerdict(res, err, w.s.opts.HypoPGMinPct)
	if err != nil {
		w.s.logf("shadow: what-if of decision %d: %v", p.ID, err)
	}
	return &hypoFact{Verdict: verdict, Improvement: res.Improvement, Reason: reason}
}

// stillRunning are the targets with calls since the decision was recorded.
func (w *whatIf) stillRunning(ctx context.Context, p pendingDecision) []int64 {
	ids := p.Prediction.TargetQueryIDs
	if len(ids) == 0 {
		return nil
	}
	measured, err := verify.NewPostgresObservationSource(w.s.pool).QueryMeasurements(ctx,
		ids, p.RecordedAt, p.now.Add(time.Second))
	if err != nil {
		w.s.logf("shadow: are the targets of decision %d still running: %v", p.ID, err)
		return nil
	}
	var out []int64
	for _, id := range ids {
		if measured[id].Samples > 0 {
			out = append(out, id)
		}
	}
	return out
}

// queries are the running targets as the what-if plans them.
func (w *whatIf) queries(ctx context.Context, ids []int64) []optimizer.QueryInfo {
	rows, err := w.s.pool.Query(ctx, queryTextsSQL, ids)
	if err != nil {
		w.s.logf("shadow: read target query texts: %v", err)
		return nil
	}
	defer rows.Close()
	var out []optimizer.QueryInfo
	for rows.Next() {
		var q optimizer.QueryInfo
		if err := rows.Scan(&q.QueryID, &q.Text, &q.Calls, &q.TotalTimeMs); err != nil {
			w.s.logf("shadow: scan target query text: %v", err)
			return nil
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		w.s.logf("shadow: read target query texts: %v", err)
		return nil
	}
	return out
}

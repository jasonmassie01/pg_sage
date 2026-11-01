package analyzer

import "context"

// planPairsSQL reads the newest two plans of each query captured in the
// last seven days, with matching source/resource/compute and capture flags. Each query's plans are read newest first through
// idx_explain_queryid and only two of them: the rule used to number every
// plan of the window, detoasting each plan_json (static audit A_snapshots
// P5). Plans captured without a query text, cost or execution time
// (nullable columns) read as empty and zero instead of failing the pass.
const planPairsSQL = `/* pg_sage */
WITH q AS (
    SELECT DISTINCT e.queryid FROM sage.explain_cache e
     WHERE e.source <> 'lakebase_plan_history' AND e.captured_at > now() - interval '7 days'
)
SELECT q.queryid, COALESCE(c.query_text, ''), c.plan_json,
       COALESCE(c.total_cost, 0), COALESCE(c.execution_time, 0),
       p.plan_json, COALESCE(p.total_cost, 0), COALESCE(p.execution_time, 0)
  FROM q
 CROSS JOIN LATERAL (
       SELECT e.id, e.query_text, e.plan_json, e.total_cost, e.execution_time, e.source,
              e.provider_scope_hash, e.provider_compute_id, e.analyze_enabled,
              e.buffers_enabled, e.timing_enabled, e.comparison_key
         FROM sage.explain_cache e
        WHERE e.source <> 'lakebase_plan_history' AND e.queryid = q.queryid AND e.captured_at > now() - interval '7 days'
        ORDER BY e.captured_at DESC, e.id DESC LIMIT 1) c
 CROSS JOIN LATERAL (
       SELECT e.plan_json, e.total_cost, e.execution_time
         FROM sage.explain_cache e
        WHERE e.source <> 'lakebase_plan_history' AND e.queryid = q.queryid AND e.captured_at > now() - interval '7 days'
          AND e.source = c.source
          AND (c.source <> 'lakebase_plan_history' OR
               (c.comparison_key IS NOT NULL AND e.comparison_key = c.comparison_key))
          AND e.provider_scope_hash = c.provider_scope_hash
          AND e.provider_compute_id = c.provider_compute_id
          AND e.analyze_enabled IS NOT DISTINCT FROM c.analyze_enabled
          AND e.buffers_enabled IS NOT DISTINCT FROM c.buffers_enabled
          AND e.timing_enabled IS NOT DISTINCT FROM c.timing_enabled
          AND e.id <> c.id
        ORDER BY e.captured_at DESC, e.id DESC LIMIT 1) p`

// checkPlanRegression loads the two most recent plans per query
// from explain_cache and runs plan regression detection.
func (a *Analyzer) checkPlanRegression(ctx context.Context) []Finding {
	rows, err := a.pool.Query(ctx, planPairsSQL)
	if err != nil {
		a.evalFail("plan_regression")
		a.logFn("ERROR", "analyzer: plan_regression query: %v", err)
		return nil
	}
	defer rows.Close()
	var pairs []planPair
	for rows.Next() {
		var p planPair
		if err := rows.Scan(&p.QueryID, &p.QueryText,
			&p.CurrentPlan, &p.CurrentCost, &p.CurrentTime,
			&p.PreviousPlan, &p.PreviousCost, &p.PreviousTime); err != nil {
			a.logFn("WARN", "analyzer: scan plan pair: %v", err)
			continue
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		a.evalFail("plan_regression")
		a.logFn("ERROR", "analyzer: iterate plan pairs: %v", err)
	}
	return rulePlanRegression(pairs)
}

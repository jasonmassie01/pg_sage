package agenttools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Bounds of a top-queries request.
const (
	defaultTopLimit = 10
	maxTopLimit     = 50
	// maxTopScan is how many statements are read before exclusion.
	maxTopScan = 500
)

// TopQueriesRequest asks for the heaviest statements of the database.
type TopQueriesRequest struct {
	Limit        int    `json:"limit,omitempty"`
	OrderBy      string `json:"order_by,omitempty"`
	IncludePlans bool   `json:"include_plans,omitempty"`
}

// TopQueriesResult is the workload, without pg_sage's own statements.
type TopQueriesResult struct {
	Queries       []TopQuery `json:"queries"`
	ExcludedCount int        `json:"excluded_count"`
	Source        string     `json:"source"`
}

// TopQuery is one statement with its totals and attribution tags.
type TopQuery struct {
	QueryID     QueryID           `json:"queryid"`
	Query       string            `json:"query"`
	Calls       int64             `json:"calls"`
	TotalTimeMs float64           `json:"total_time_ms"`
	MeanTimeMs  float64           `json:"mean_time_ms"`
	Rows        int64             `json:"rows"`
	Plan        *CachedPlan       `json:"plan,omitempty"`
	Tags        map[string]string `json:"sqlcommenter,omitempty"`
}

// CachedPlan is the newest plan pg_sage captured for a statement.
type CachedPlan struct {
	Source     string    `json:"source"`
	CapturedAt time.Time `json:"captured_at"`
	TotalCost  float64   `json:"total_cost"`
	PlanHash   string    `json:"plan_hash,omitempty"`
	NodeTypes  []string  `json:"node_types"`
}

var topOrders = map[string]bool{"total_time": true, "mean_time": true, "calls": true}

const topQueriesSQL = `/* pg_sage */ SELECT ` + statementColumns + `
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid IS NOT NULL
GROUP BY queryid
ORDER BY CASE $1
    WHEN 'calls' THEN sum(calls)::float8
    WHEN 'mean_time' THEN sum(total_exec_time) / NULLIF(sum(calls), 0)
    ELSE sum(total_exec_time) END DESC NULLS LAST, queryid
LIMIT $2`

// TopQueries returns the heaviest statements of the current database by
// total time (default), mean time or calls, without excluded statements.
func (t *Tools) TopQueries(ctx context.Context, req TopQueriesRequest,
) (TopQueriesResult, error) {
	if err := t.ready(); err != nil {
		return TopQueriesResult{}, err
	}
	limit, order, err := topRequest(req)
	if err != nil {
		return TopQueriesResult{}, err
	}
	res, err := t.readTop(ctx, order, limit)
	if err != nil {
		return TopQueriesResult{}, err
	}
	if req.IncludePlans {
		if err := t.attachPlans(ctx, res.Queries); err != nil {
			return TopQueriesResult{}, err
		}
	}
	return res, nil
}

func topRequest(req TopQueriesRequest) (int, string, error) {
	limit, order := req.Limit, req.OrderBy
	if limit == 0 {
		limit = defaultTopLimit
	}
	if limit < 1 || limit > maxTopLimit {
		return 0, "", invalid("limit %d is outside 1..%d", req.Limit, maxTopLimit)
	}
	if order == "" {
		order = "total_time"
	}
	if !topOrders[order] {
		return 0, "", invalid("order_by %q is not total_time, mean_time or calls", order)
	}
	return limit, order, nil
}

func (t *Tools) readTop(ctx context.Context, order string, limit int,
) (TopQueriesResult, error) {
	rows, err := t.pool.Query(ctx, topQueriesSQL, order, maxTopScan)
	if err != nil {
		return TopQueriesResult{}, statementsError(err)
	}
	defer rows.Close()
	res := TopQueriesResult{Queries: []TopQuery{}, Source: "pg_stat_statements"}
	for rows.Next() {
		var s statement
		if err := rows.Scan(&s.ID, &s.Text, &s.Calls, &s.TotalTimeMs, &s.Rows); err != nil {
			return TopQueriesResult{}, fmt.Errorf("scan pg_stat_statements: %w", err)
		}
		if t.opts.Excluded(s.Text) {
			res.ExcludedCount++
			continue
		}
		if len(res.Queries) < limit {
			res.Queries = append(res.Queries, topQuery(s))
		}
	}
	if err := rows.Err(); err != nil {
		return TopQueriesResult{}, statementsError(err)
	}
	return res, nil
}

func topQuery(s statement) TopQuery {
	q := TopQuery{QueryID: QueryID(s.ID), Query: s.Text, Calls: s.Calls,
		TotalTimeMs: s.TotalTimeMs, MeanTimeMs: s.meanMs(), Rows: s.Rows}
	if tags := ParseSQLCommenter(s.Text); len(tags) > 0 {
		q.Tags = tags
	}
	return q
}

const cachedPlansSQL = `/* pg_sage */ SELECT DISTINCT ON (queryid) queryid, source,
    captured_at, COALESCE(total_cost, 0)::float8, COALESCE(plan_hash, ''), plan_json
FROM sage.explain_cache
WHERE queryid = ANY($1)
ORDER BY queryid, captured_at DESC, id DESC`

// attachPlans sets each query's newest cached plan, when there is one.
func (t *Tools) attachPlans(ctx context.Context, queries []TopQuery) error {
	ids := make([]int64, len(queries))
	for i, q := range queries {
		ids[i] = int64(q.QueryID)
	}
	rows, err := t.pool.Query(ctx, cachedPlansSQL, ids)
	if err != nil {
		return fmt.Errorf("read sage.explain_cache: %w", err)
	}
	defer rows.Close()
	plans := make(map[int64]*CachedPlan, len(ids))
	for rows.Next() {
		var id int64
		var raw []byte
		p := &CachedPlan{}
		if err := rows.Scan(&id, &p.Source, &p.CapturedAt, &p.TotalCost, &p.PlanHash,
			&raw); err != nil {
			return fmt.Errorf("scan sage.explain_cache: %w", err)
		}
		p.NodeTypes = planNodeTypes(raw)
		plans[id] = p
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read sage.explain_cache: %w", err)
	}
	for i := range queries {
		queries[i].Plan = plans[int64(queries[i].QueryID)]
	}
	return nil
}

// planNodeTypes lists the distinct "Node Type" values of an EXPLAIN JSON
// plan, outermost first. A plan that is not JSON has none.
func planNodeTypes(raw []byte) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case []any:
			for _, e := range n {
				walk(e)
			}
		case map[string]any:
			if typ, ok := n["Node Type"].(string); ok && !seen[typ] {
				seen[typ] = true
				out = append(out, typ)
			}
			walk(n["Plan"])
			walk(n["Plans"])
		}
	}
	walk(doc)
	return out
}

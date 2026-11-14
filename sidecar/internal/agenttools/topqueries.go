package agenttools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Bounds of a top-queries request.
const (
	defaultTopLimit = 10
	maxTopLimit     = 50
	// maxTopScan is how many statements are read before a custom exclusion.
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
	// JSON is the captured EXPLAIN document; top_queries leaves it out,
	// the workload explorer's query page shows it.
	JSON json.RawMessage `json:"-"`
}

var topOrders = map[string]bool{"total_time": true, "mean_time": true, "calls": true}

// topQueriesSQL reads the heaviest statements of the current database.
// With $3 true the workload rule (workload.AdviceSQL) is applied before
// the LIMIT and the statements it leaves out are only counted: pg_sage's
// own statements (its schema bootstrap alone is hundreds) must not fill
// the bounded scan and hide the workload. The first column is that count;
// the query columns are NULL when no statement is listed. rel is the
// qualified pg_stat_statements view.
func topQueriesSQL(rel string) string {
	return `/* pg_sage */ WITH s AS (
    SELECT queryid, min(query) AS query, sum(calls)::bigint AS calls,
        sum(total_exec_time)::float8 AS total_ms, sum(rows)::bigint AS nrows
    FROM ` + rel + `
    WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
      AND queryid IS NOT NULL
    GROUP BY queryid),
w AS (SELECT s.*, (NOT $3::boolean OR (` + workload.AdviceSQL("s.query") + `)) AS listed
    FROM s),
q AS (SELECT * FROM w WHERE listed
    ORDER BY ` + topOrderSQL + `, queryid LIMIT $2)
SELECT (SELECT count(*) FROM w WHERE NOT listed)::bigint,
    q.queryid, q.query, q.calls, q.total_ms, q.nrows
FROM (SELECT 1) one LEFT JOIN q ON true
ORDER BY ` + topOrderSQL + `, q.queryid`
}

// topOrderSQL is the ORDER BY key of $1 (an order from topOrders).
const topOrderSQL = `CASE $1
    WHEN 'calls' THEN calls::float8
    WHEN 'mean_time' THEN total_ms / NULLIF(calls, 0)
    ELSE total_ms END DESC NULLS LAST`

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
	rel, err := extschema.StatementsRel(ctx, t.pool, "pg_stat_statements")
	if err != nil {
		return TopQueriesResult{}, statementsError(err)
	}
	rows, err := t.pool.Query(ctx, topQueriesSQL(rel), order, maxTopScan, t.sqlWorkload)
	if err != nil {
		return TopQueriesResult{}, statementsError(err)
	}
	defer rows.Close()
	res := TopQueriesResult{Queries: []TopQuery{}, Source: "pg_stat_statements"}
	var sqlExcluded int
	for rows.Next() {
		s, listed, err := scanTopRow(rows, &sqlExcluded)
		if err != nil {
			return TopQueriesResult{}, err
		}
		if !listed {
			continue
		}
		q, excluded := t.present(s)
		if excluded {
			res.ExcludedCount++
			continue
		}
		if len(res.Queries) < limit {
			res.Queries = append(res.Queries, q)
		}
	}
	if err := rows.Err(); err != nil {
		return TopQueriesResult{}, statementsError(err)
	}
	res.ExcludedCount += sqlExcluded
	return res, nil
}

// scanTopRow reads one topQueriesSQL row: it stores the count of
// statements the SQL left out and reports whether the row has a statement.
func scanTopRow(rows pgx.Rows, sqlExcluded *int) (statement, bool, error) {
	var (
		excluded int64
		id       *int64
		text     *string
		calls    *int64
		total    *float64
		n        *int64
	)
	if err := rows.Scan(&excluded, &id, &text, &calls, &total, &n); err != nil {
		return statement{}, false, fmt.Errorf("scan pg_stat_statements: %w", err)
	}
	*sqlExcluded = int(excluded)
	if id == nil || text == nil || calls == nil || total == nil || n == nil {
		return statement{}, false, nil
	}
	return statement{ID: *id, Text: *text, Calls: *calls, TotalTimeMs: *total,
		Rows: *n}, true, nil
}

// present is how the agent tools and the workload explorer show a
// statement: its normalized text and tags, and whether the workload rule
// leaves it out (pg_sage's own statements).
func (t *Tools) present(s statement) (TopQuery, bool) {
	return topQuery(s), t.opts.Excluded(s.Text)
}

// Statement is one statement as present shows it.
type Statement struct {
	Query    string
	Tags     map[string]string
	Excluded bool
}

// Statements reads the statements of ids from pg_stat_statements as
// top_queries shows them; an id without a statement (evicted, or never
// tracked) is absent. A missing or unloaded extension is ErrUnavailable.
func (t *Tools) Statements(ctx context.Context, ids []int64) (map[int64]Statement, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	byID, err := t.statementsByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]Statement, len(byID))
	for id, s := range byID {
		q, excluded := t.present(s)
		out[id] = Statement{Query: q.Query, Tags: q.Tags, Excluded: excluded}
	}
	return out, nil
}

func topQuery(s statement) TopQuery {
	q := TopQuery{QueryID: QueryID(s.ID), Query: s.Text, Calls: s.Calls,
		TotalTimeMs: s.TotalTimeMs, MeanTimeMs: s.meanMs(), Rows: s.Rows}
	if tags := sqlast.ParseSQLCommenter(s.Text); len(tags) > 0 {
		q.Tags = tags
	}
	return q
}

const cachedPlansSQL = `/* pg_sage */ SELECT DISTINCT ON (queryid) queryid, source,
    captured_at, COALESCE(total_cost, 0)::float8, COALESCE(plan_hash, ''), plan_json
FROM sage.explain_cache
WHERE queryid = ANY($1) AND source <> 'lakebase_plan_history'
ORDER BY queryid, captured_at DESC, id DESC`

// attachPlans sets each query's newest cached plan, when there is one.
func (t *Tools) attachPlans(ctx context.Context, queries []TopQuery) error {
	ids := make([]int64, len(queries))
	for i, q := range queries {
		ids[i] = int64(q.QueryID)
	}
	plans, err := t.Plans(ctx, ids)
	if err != nil {
		return err
	}
	for i := range queries {
		queries[i].Plan = plans[int64(queries[i].QueryID)]
	}
	return nil
}

// Plans reads the newest plan pg_sage captured for each of ids (not a
// provider's plan history); an id without one is absent.
func (t *Tools) Plans(ctx context.Context, ids []int64) (map[int64]*CachedPlan, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	rows, err := t.pool.Query(ctx, cachedPlansSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("read sage.explain_cache: %w", err)
	}
	defer rows.Close()
	plans := make(map[int64]*CachedPlan, len(ids))
	for rows.Next() {
		var id int64
		var raw []byte
		p := &CachedPlan{}
		if err := rows.Scan(&id, &p.Source, &p.CapturedAt, &p.TotalCost, &p.PlanHash,
			&raw); err != nil {
			return nil, fmt.Errorf("scan sage.explain_cache: %w", err)
		}
		p.NodeTypes = planNodeTypes(raw)
		if json.Valid(raw) {
			p.JSON = raw
		}
		plans[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sage.explain_cache: %w", err)
	}
	return plans, nil
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

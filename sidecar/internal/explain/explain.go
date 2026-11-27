// Package explain provides natural language EXPLAIN for PostgreSQL queries.
// It runs EXPLAIN (ANALYZE) via a read-only transaction, extracts plan nodes,
// and caches results in sage.explain_results. ANALYZE executes the query,
// so it runs only for statements the analyze guard proves free of side
// effects (analyze_guard.go); everything else is explained plan-only.
package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/planfresh"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// ErrExplainInvalidRequest wraps errors caused by client-side input
// problems (missing query, DDL, a query_id with no statement). Handlers
// surface these as a 400 response and expose the message verbatim —
// the caller needs it to fix their request. Any other error from
// Explain() originates in PG execution and must be treated as a 500
// with the internal message scrubbed.
var ErrExplainInvalidRequest = errors.New("invalid explain request")

// ---------- request / response types ----------

// ExplainRequest is the API input.
type ExplainRequest struct {
	Query    string   `json:"query,omitempty"`
	QueryID  QueryID  `json:"query_id,omitempty"`
	PlanOnly bool     `json:"plan_only,omitempty"`
	Params   []string `json:"params,omitempty"`
}

// ExplainResult is the API output. SlowBecause and Recommendations hold
// the rule entries first, then the model's; Insights tags each entry with
// its source ("rules" or "llm"). PlanSource is "explain" (planned now) or
// "explain_cache" (a captured executed plan, PlanCapture naming the
// capture), and PlanAgeS the plan's age in seconds (tuning-10).
type ExplainResult struct {
	Query           string          `json:"query"`
	PlanJSON        json.RawMessage `json:"plan_json"`
	Summary         string          `json:"summary"`
	SlowBecause     []string        `json:"slow_because"`
	Recommendations []string        `json:"recommendations"`
	Insights        []Insight       `json:"insights"`
	NodeBreakdown   []NodeExplain   `json:"node_breakdown"`
	EstimatedCost   float64         `json:"estimated_cost"`
	ActualTimeMs    *float64        `json:"actual_time_ms,omitempty"`
	PlanningTimeMs  *float64        `json:"planning_time_ms,omitempty"`
	Note            string          `json:"note,omitempty"`
	// AnalyzeRefused is why ANALYZE was requested but not run; the plan
	// is then estimate-only.
	AnalyzeRefused string     `json:"analyze_refused,omitempty"`
	CachedAt       *time.Time `json:"cached_at,omitempty"`
	PlanSource     string     `json:"plan_source,omitempty"`
	PlanCapture    string     `json:"plan_capture,omitempty"`
	PlanAgeS       *int64     `json:"plan_age_s,omitempty"`
}

// NodeExplain explains a single plan node. TimeMs is inclusive of the
// node's children and summed over its loops (Actual Total Time x Loops).
type NodeExplain struct {
	NodeType    string   `json:"node_type"`
	Relation    string   `json:"relation,omitempty"`
	Description string   `json:"description"`
	TimeMs      *float64 `json:"time_ms,omitempty"`
	Loops       *int64   `json:"loops,omitempty"`
	Rows        int64    `json:"rows"`
	RowEstimate int64    `json:"row_estimate"`
	Warning     string   `json:"warning,omitempty"`
}

// ---------- Explainer ----------

// Explainer runs EXPLAIN against a PostgreSQL pool and produces
// human-readable plan breakdowns.
type Explainer struct {
	pool      *pgxpool.Pool
	cfg       *config.ExplainConfig
	logFn     func(string, string, ...any)
	llmClient *llm.Client // optional; nil disables LLM enhancement
	// inspect parses a query for the analyze guard; nil means
	// sqlast.InspectReadQuery (tests substitute it).
	inspect func(string) (sqlast.ReadQuery, error)
	// planMaxAge bounds a captured plan's age (0: planfresh default).
	planMaxAge time.Duration
	// now is the clock; nil means time.Now.
	now func() time.Time
}

// New creates an Explainer without LLM support.
func New(
	pool *pgxpool.Pool,
	cfg *config.ExplainConfig,
	logFn func(string, string, ...any),
) *Explainer {
	return &Explainer{pool: pool, cfg: cfg, logFn: logFn, inspect: sqlast.InspectReadQuery}
}

// NewWithLLM creates an Explainer with optional LLM enhancement.
// If llmClient is nil, the explainer behaves identically to New.
func NewWithLLM(
	pool *pgxpool.Pool,
	cfg *config.ExplainConfig,
	llmClient *llm.Client,
	logFn func(string, string, ...any),
) *Explainer {
	return &Explainer{
		pool: pool, cfg: cfg,
		llmClient: llmClient, logFn: logFn, inspect: sqlast.InspectReadQuery,
	}
}

// ---------- core method ----------

// Explain validates the request, runs EXPLAIN, extracts nodes,
// caches, and returns the result.
func (ex *Explainer) Explain(
	ctx context.Context, req ExplainRequest,
) (*ExplainResult, error) {
	// 1. Validate input; a query_id resolves to its statement text and,
	// when one is fresh and executed, its captured plan.
	req, captured, err := ex.target(ctx, req)
	if err != nil {
		return nil, err
	}

	// 2. Single read-statement allowlist (G6-B01). The body is the
	// query without a trailing terminator so it can be embedded in
	// EXPLAIN / PREPARE.
	body, err := explainBody(req.Query)
	if err != nil {
		return nil, err
	}
	req.Query = body
	if captured != nil {
		return ex.capturedResult(ctx, body, *captured), nil
	}

	// 3. Check cache. The key separates plan-only from ANALYZE requests.
	dbName := ex.databaseName()
	key := cacheKey(req.Query, req.Params, req.PlanOnly)
	cached, err := ex.checkCache(ctx, key, dbName)
	if err != nil {
		ex.logFn("WARN", "explain: cache lookup failed: %v", err)
	}
	if cached != nil {
		ageCached(cached, ex.clock())
		return cached, nil
	}

	// 4-6. Execute EXPLAIN and build the result.
	result, err := ex.execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("explain: %w", err)
	}
	// 6b-7. Enhance with LLM (graceful degradation on failure) and cache;
	// an LLM failure fallback is cached only briefly.
	ttl := ex.cacheTTLMinutes(ex.enhanceWithLLM(ctx, result))
	if cacheErr := ex.saveCache(ctx, key, dbName, result, ttl); cacheErr != nil {
		ex.logFn("WARN", "explain: cache save failed: %v", cacheErr)
	}
	return result, nil
}

// execute runs EXPLAIN in the mode the request allows. Parameterized
// queries are always plan-only; ANALYZE needs the analyze guard's consent.
func (ex *Explainer) execute(
	ctx context.Context, req ExplainRequest,
) (*ExplainResult, error) {
	nParams, err := paramPlaceholders(req.Query)
	if err != nil {
		return nil, err
	}
	hasParams := nParams > 0
	var planJSON json.RawMessage
	var analyzed bool
	var refused string
	if hasParams {
		planJSON, err = ex.runExplainParameterized(ctx, req.Query, req.Params, nParams)
	} else {
		planJSON, analyzed, refused, err = ex.runExplain(ctx, req.Query, !req.PlanOnly)
	}
	if err != nil {
		return nil, err
	}
	result := buildResult(req.Query, planJSON, analyzed)
	result.Note = explainNote(req.PlanOnly, hasParams, refused)
	result.AnalyzeRefused = refused
	fresh := int64(0)
	result.PlanSource, result.PlanAgeS = planfresh.SourceExplain, &fresh
	ex.applyRules(ctx, result, analyzed)
	return result, nil
}

// target checks the request and, for a query_id, sets the query to the
// statement's text and returns its captured plan (nil: explain afresh).
func (ex *Explainer) target(ctx context.Context, req ExplainRequest,
) (ExplainRequest, *planfresh.Plan, error) {
	if err := checkTarget(req); err != nil || req.QueryID == 0 {
		return req, nil, err
	}
	text, plan, err := ex.resolve(ctx, req)
	if err != nil {
		return req, nil, fmt.Errorf("explain: %w", err)
	}
	req.Query = text
	return req, plan, nil
}

// checkTarget requires exactly one of query and query_id.
func checkTarget(req ExplainRequest) error {
	switch {
	case req.Query == "" && req.QueryID == 0:
		return fmt.Errorf("%w: query or query_id is required", ErrExplainInvalidRequest)
	case req.Query != "" && req.QueryID != 0:
		return fmt.Errorf("%w: give query or query_id, not both", ErrExplainInvalidRequest)
	}
	return nil
}

// ageCached adds the time since a cached result was stored to its plan age.
func ageCached(r *ExplainResult, now time.Time) {
	if r.PlanAgeS == nil || r.CachedAt == nil {
		return
	}
	age := *r.PlanAgeS + max(int64(now.Sub(*r.CachedAt)/time.Second), 0)
	r.PlanAgeS = &age
}

// ---------- EXPLAIN execution ----------

func (ex *Explainer) runExplain(
	ctx context.Context, query string, analyze bool,
) (json.RawMessage, bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, ex.timeout())
	defer cancel()

	conn, err := ex.pool.Acquire(ctx)
	if err != nil {
		return nil, false, "", fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if err = ex.prepareConn(ctx, conn); err != nil {
		return nil, false, "", err
	}
	defer func() { _, _ = conn.Exec(ctx, "ROLLBACK") }()

	refused := ""
	if analyze {
		refused = ex.guardAnalyze(ctx, conn, query)
		analyze = refused == ""
	}
	plan, err := collectPlanJSON(ctx, conn, explainSQL(query, analyze))
	return plan, analyze, refused, err
}

const explainStmtName = "_sage_explain"

// runExplainParameterized handles queries with nParams $N placeholders
// (typically from pg_stat_statements). PG cannot EXPLAIN these
// directly. We use PREPARE + EXPLAIN EXECUTE with NULL values to
// get the plan. If the caller provided explicit params, those are
// used instead of NULLs.
//
// PREPARE is sent over the extended protocol, which PostgreSQL
// restricts to a single statement, so a body such as
// "SELECT $1; COMMIT; DELETE ..." cannot leave the READ ONLY
// transaction (G6-B01).
func (ex *Explainer) runExplainParameterized(
	ctx context.Context, query string, params []string, nParams int,
) (json.RawMessage, error) {
	paramList, err := explainParamList(nParams, params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ex.timeout())
	defer cancel()

	conn, err := ex.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if err = ex.prepareConn(ctx, conn); err != nil {
		return nil, err
	}
	// Without a value for every parameter the custom plan folds "x = NULL"
	// to a constant-false Result; the generic plan is the query's real one.
	if hasUnboundParam(nParams, params) {
		if _, err = conn.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
			return nil, fmt.Errorf("set plan_cache_mode: %w", err)
		}
	}
	// Prepared statements survive ROLLBACK, and DEALLOCATE fails in
	// an aborted transaction, so release it after the rollback.
	defer func() {
		_, _ = conn.Exec(ctx, "ROLLBACK")
		_, _ = conn.Exec(ctx, "DEALLOCATE "+explainStmtName)
	}()

	prepSQL := fmt.Sprintf("PREPARE %s AS %s", explainStmtName, query)
	_, err = conn.Conn().PgConn().ExecParams(
		ctx, prepSQL, nil, nil, nil, nil).Close()
	if err != nil {
		return nil, fmt.Errorf("prepare parameterized: %w", err)
	}
	explainExec := fmt.Sprintf(
		"EXPLAIN (FORMAT JSON) EXECUTE %s(%s)",
		explainStmtName, paramList,
	)
	return collectPlanJSON(ctx, conn, explainExec)
}

// explainParamList renders the n EXECUTE arguments: provided params as
// escaped SQL literals, NULL otherwise. EXECUTE arguments are SQL
// expressions, so user input must never be interpolated raw.
func explainParamList(n int, params []string) (string, error) {
	values := make([]string, n)
	for i := range values {
		values[i] = "NULL"
		if i < len(params) && params[i] != "" {
			literal, err := explainParamLiteral(params[i])
			if err != nil {
				return "", err
			}
			values[i] = literal
		}
	}
	return strings.Join(values, ", "), nil
}

// hasUnboundParam reports one of n placeholders without a value (planned
// as NULL).
func hasUnboundParam(n int, params []string) bool {
	for i := 0; i < n; i++ {
		if i >= len(params) || params[i] == "" {
			return true
		}
	}
	return false
}

// prepareConn opens a read-only transaction with statement_timeout, on
// the application's search_path (apppath.Pin), never pg_sage's own: the
// analyze guard and the plan resolve the statement's names on it.
func (ex *Explainer) prepareConn(
	ctx context.Context, conn *pgxpool.Conn,
) error {
	if _, err := conn.Exec(ctx, "BEGIN READ ONLY"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if _, err := conn.Exec(ctx, statementTimeoutSQL(ex.timeout())); err != nil {
		return fmt.Errorf("set statement_timeout: %w", err)
	}
	if _, err := conn.Exec(
		ctx, "SET LOCAL transaction_read_only = on",
	); err != nil {
		return fmt.Errorf("set transaction_read_only: %w", err)
	}
	return apppath.Pin(ctx, conn)
}

func explainSQL(query string, analyze bool) string {
	prefix := "EXPLAIN (FORMAT JSON)"
	if analyze {
		prefix = "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)"
	}
	return prefix + " " + query
}

func explainParamLiteral(value string) (string, error) {
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf(
			"%w: parameter values cannot contain NUL bytes",
			ErrExplainInvalidRequest,
		)
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

// collectPlanJSON reads EXPLAIN output rows into a validated RawMessage.
func collectPlanJSON(
	ctx context.Context, conn *pgxpool.Conn, sql string,
) (json.RawMessage, error) {
	rows, err := conn.Query(ctx, sql, pgx.QueryExecModeExec)
	if err != nil {
		return nil, fmt.Errorf("execute explain: %w", err)
	}
	defer rows.Close()

	var planText string
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("scan plan row: %w", err)
		}
		planText += line
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read plan rows: %w", err)
	}
	if planText == "" {
		return nil, fmt.Errorf("empty plan returned")
	}
	raw := json.RawMessage(planText)
	if !json.Valid(raw) {
		return nil, fmt.Errorf("plan JSON is invalid")
	}
	return raw, nil
}

// ---------- result builder ----------

func buildResult(
	query string, planJSON json.RawMessage, analyzed bool,
) *ExplainResult {
	planJSON = normalizePlan(planJSON)
	nodes := extractNodes(planJSON)

	result := &ExplainResult{
		Query:           query,
		PlanJSON:        planJSON,
		SlowBecause:     []string{},
		Recommendations: []string{},
		Insights:        []Insight{},
		NodeBreakdown:   nodes,
	}
	result.Summary = "the plan could not be read"
	if len(nodes) > 0 {
		result.Summary = nodes[0].Description
	}

	// Top-level cost from the root plan node; the statement's time is its
	// Execution Time, else (older captures) the root node's time.
	type topLevel struct {
		Plan struct {
			TotalCost  float64 `json:"Total Cost"`
			ActualTime float64 `json:"Actual Total Time"`
		} `json:"Plan"`
		PlanningTime  *float64 `json:"Planning Time"`
		ExecutionTime *float64 `json:"Execution Time"`
	}
	var plans []topLevel
	if err := json.Unmarshal(planJSON, &plans); err == nil && len(plans) > 0 {
		result.EstimatedCost = plans[0].Plan.TotalCost
		if analyzed {
			at := plans[0].Plan.ActualTime
			if plans[0].ExecutionTime != nil {
				at = *plans[0].ExecutionTime
			}
			result.ActualTimeMs = &at
			result.PlanningTimeMs = plans[0].PlanningTime
		}
	}

	if !analyzed {
		result.Note = "EXPLAIN without ANALYZE (query has parameters)"
	}

	return result
}

// ---------- helpers ----------

// databaseName extracts the database name from the pool config.
func (ex *Explainer) databaseName() string {
	cc := ex.pool.Config().ConnConfig
	if cc != nil && cc.Database != "" {
		return cc.Database
	}
	return ""
}

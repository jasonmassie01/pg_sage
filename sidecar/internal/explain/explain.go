// Package explain provides natural language EXPLAIN for PostgreSQL queries.
// It runs EXPLAIN (ANALYZE) via a read-only transaction, extracts plan nodes,
// and caches results in sage.explain_results.
package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// ErrExplainInvalidRequest wraps errors caused by client-side input
// problems (missing query, DDL, unsupported query_id lookup). Handlers
// surface these as a 400 response and expose the message verbatim —
// the caller needs it to fix their request. Any other error from
// Explain() originates in PG execution and must be treated as a 500
// with the internal message scrubbed.
var ErrExplainInvalidRequest = errors.New("invalid explain request")

// ---------- request / response types ----------

// ExplainRequest is the API input.
type ExplainRequest struct {
	Query    string   `json:"query,omitempty"`
	QueryID  int64    `json:"query_id,omitempty"`
	PlanOnly bool     `json:"plan_only,omitempty"`
	Params   []string `json:"params,omitempty"`
}

// ExplainResult is the API output.
type ExplainResult struct {
	Query           string          `json:"query"`
	PlanJSON        json.RawMessage `json:"plan_json"`
	Summary         string          `json:"summary"`
	SlowBecause     []string        `json:"slow_because"`
	Recommendations []string        `json:"recommendations"`
	NodeBreakdown   []NodeExplain   `json:"node_breakdown"`
	EstimatedCost   float64         `json:"estimated_cost"`
	ActualTimeMs    *float64        `json:"actual_time_ms,omitempty"`
	PlanningTimeMs  *float64        `json:"planning_time_ms,omitempty"`
	Note            string          `json:"note,omitempty"`
	CachedAt        *time.Time      `json:"cached_at,omitempty"`
}

// NodeExplain explains a single plan node.
type NodeExplain struct {
	NodeType    string   `json:"node_type"`
	Relation    string   `json:"relation,omitempty"`
	Description string   `json:"description"`
	TimeMs      *float64 `json:"time_ms,omitempty"`
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
}

// New creates an Explainer without LLM support.
func New(
	pool *pgxpool.Pool,
	cfg *config.ExplainConfig,
	logFn func(string, string, ...any),
) *Explainer {
	return &Explainer{pool: pool, cfg: cfg, logFn: logFn}
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
		llmClient: llmClient, logFn: logFn,
	}
}

// ---------- core method ----------

// Explain validates the request, runs EXPLAIN, extracts nodes,
// caches, and returns the result.
func (ex *Explainer) Explain(
	ctx context.Context, req ExplainRequest,
) (*ExplainResult, error) {
	// 1. Validate input.
	if req.Query == "" && req.QueryID == 0 {
		return nil, fmt.Errorf(
			"%w: query or query_id is required",
			ErrExplainInvalidRequest)
	}
	if req.QueryID != 0 {
		return nil, fmt.Errorf(
			"%w: query_id lookup not yet implemented",
			ErrExplainInvalidRequest)
	}

	// 2. Single read-statement allowlist (G6-B01). The body is the
	// query without a trailing terminator so it can be embedded in
	// EXPLAIN / PREPARE.
	body, err := explainBody(req.Query)
	if err != nil {
		return nil, err
	}
	req.Query = body

	// 3. Check cache.
	dbName := ex.databaseName()
	hash := queryHash(req.Query, req.Params)

	cached, err := ex.checkCache(ctx, hash, dbName)
	if err != nil {
		ex.logFn("WARN", "explain: cache lookup failed: %v", err)
	}
	if cached != nil {
		return cached, nil
	}

	// 4. Determine mode.
	hasParams := hasParamPlaceholder(req.Query)
	useAnalyze := !req.PlanOnly && !hasParams

	// 5. Execute EXPLAIN.
	var planJSON json.RawMessage
	if hasParams {
		planJSON, err = ex.runExplainParameterized(
			ctx, req.Query, req.Params)
	} else {
		planJSON, err = ex.runExplain(ctx, req.Query, useAnalyze)
	}
	if err != nil {
		return nil, fmt.Errorf("explain: %w", err)
	}

	// 6. Build result.
	result := buildResult(req.Query, planJSON, useAnalyze)

	// 6b. Enhance with LLM (graceful degradation on failure).
	ex.enhanceWithLLM(ctx, result)

	// 7. Cache result.
	if cacheErr := ex.saveCache(ctx, hash, dbName, result); cacheErr != nil {
		ex.logFn("WARN", "explain: cache save failed: %v", cacheErr)
	}

	return result, nil
}

// ---------- EXPLAIN execution ----------

func (ex *Explainer) runExplain(
	ctx context.Context, query string, analyze bool,
) (json.RawMessage, error) {
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
	defer func() { _, _ = conn.Exec(ctx, "ROLLBACK") }()

	explainSQL := explainSQL(query, analyze)
	return collectPlanJSON(ctx, conn, explainSQL)
}

func (ex *Explainer) timeout() time.Duration {
	timeout := time.Duration(ex.cfg.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return timeout
}

const explainStmtName = "_sage_explain"

// runExplainParameterized handles queries with $N placeholders
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
	ctx context.Context, query string, params []string,
) (json.RawMessage, error) {
	paramList, err := explainParamList(query, params)
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

// explainParamList renders EXECUTE arguments: provided params as
// escaped SQL literals, NULL otherwise. EXECUTE arguments are SQL
// expressions, so user input must never be interpolated raw.
func explainParamList(query string, params []string) (string, error) {
	values := make([]string, countParamPlaceholders(query))
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

// countParamPlaceholders returns the highest $N placeholder number
// found in the query (e.g. "$3" means 3 parameters).
func countParamPlaceholders(query string) int {
	matches := paramNumRe.FindAllStringSubmatch(query, -1)
	max := 0
	for _, m := range matches {
		if len(m) > 1 {
			var n int
			if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil {
				if n > max {
					max = n
				}
			}
		}
	}
	return max
}

// prepareConn opens a read-only transaction with statement_timeout.
func (ex *Explainer) prepareConn(
	ctx context.Context, conn *pgxpool.Conn,
) error {
	if _, err := conn.Exec(ctx, "BEGIN READ ONLY"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	stmtTimeout := fmt.Sprintf(
		"SET LOCAL statement_timeout = '%dms'", ex.cfg.TimeoutMs,
	)
	if _, err := conn.Exec(ctx, stmtTimeout); err != nil {
		return fmt.Errorf("set statement_timeout: %w", err)
	}
	if _, err := conn.Exec(
		ctx, "SET LOCAL transaction_read_only = on",
	); err != nil {
		return fmt.Errorf("set transaction_read_only: %w", err)
	}
	return nil
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
	nodes := extractNodes(planJSON)

	result := &ExplainResult{
		Query:           query,
		PlanJSON:        planJSON,
		Summary:         "Raw plan (LLM analysis pending)",
		SlowBecause:     []string{},
		Recommendations: []string{},
		NodeBreakdown:   nodes,
	}

	// Extract top-level cost and timing from the root plan node.
	type topLevel struct {
		Plan struct {
			TotalCost    float64 `json:"Total Cost"`
			ActualTime   float64 `json:"Actual Total Time"`
			PlanningTime float64 `json:"Planning Time"`
		} `json:"Plan"`
		PlanningTime  *float64 `json:"Planning Time"`
		ExecutionTime *float64 `json:"Execution Time"`
	}
	var plans []topLevel
	if err := json.Unmarshal(planJSON, &plans); err == nil && len(plans) > 0 {
		result.EstimatedCost = plans[0].Plan.TotalCost
		if analyzed {
			at := plans[0].Plan.ActualTime
			result.ActualTimeMs = &at
			if plans[0].PlanningTime != nil {
				result.PlanningTimeMs = plans[0].PlanningTime
			}
		}
	}

	if !analyzed {
		result.Note = "EXPLAIN without ANALYZE (query has parameters)"
	}

	return result
}

// ---------- helpers ----------

var paramRe = regexp.MustCompile(`\$\d`)
var paramNumRe = regexp.MustCompile(`\$(\d+)`)

// hasParamPlaceholder returns true if the query contains $1-style placeholders.
func hasParamPlaceholder(query string) bool {
	return paramRe.MatchString(query)
}

// queryHash computes an FNV-64a hash of the normalised query and params.
func queryHash(query string, params []string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(normalizeQuery(query)))
	for _, p := range params {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(p))
	}
	return int64(h.Sum64())
}

var wsRe = regexp.MustCompile(`\s+`)
var commentRe = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*`)

// normalizeQuery strips comments and collapses whitespace.
func normalizeQuery(query string) string {
	q := commentRe.ReplaceAllString(query, " ")
	q = wsRe.ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}

// databaseName extracts the database name from the pool config.
func (ex *Explainer) databaseName() string {
	cc := ex.pool.Config().ConnConfig
	if cc != nil && cc.Database != "" {
		return cc.Database
	}
	return ""
}

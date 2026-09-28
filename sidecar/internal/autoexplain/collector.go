package autoexplain

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// CollectorConfig holds configuration for the auto_explain
// collector.
type CollectorConfig struct {
	CollectIntervalSeconds int
	MaxPlansPerCycle       int
	LogMinDurationMs       int
	PreferSessionLoad      bool
}

// Collector ingests auto_explain plans and stores them in
// sage.explain_cache.
type Collector struct {
	pool  *pgxpool.Pool
	cfg   CollectorConfig
	avail *Availability
	logFn func(string, string, ...any)
	// serverVersionNum caches server_version_num (0 = not read yet).
	serverVersionNum int
}

// NewCollector creates a Collector wired to the given pool.
func NewCollector(
	pool *pgxpool.Pool,
	cfg CollectorConfig,
	avail *Availability,
	logFn func(string, string, ...any),
) *Collector {
	return &Collector{
		pool:  pool,
		cfg:   cfg,
		avail: avail,
		logFn: logFn,
	}
}

// Run starts the collection loop, blocking until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	seconds := c.cfg.CollectIntervalSeconds
	if seconds <= 0 {
		// time.NewTicker panics on a non-positive interval (G1-B30).
		c.logFn("WARN", "autoexplain: invalid collect interval %ds; using default %ds",
			seconds, config.DefaultAutoExplainCollectInterval)
		seconds = config.DefaultAutoExplainCollectInterval
	}
	interval := time.Duration(seconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.Collect(ctx); err != nil {
				c.logFn(
					"WARN", "autoexplain: collect cycle: %v", err,
				)
			}
		case <-ctx.Done():
			return
		}
	}
}

// Collect finds slow queries without recent plans and captures
// execution plans for them on-demand.
func (c *Collector) Collect(ctx context.Context) error {
	rows, err := c.pool.Query(ctx, candidateSQL,
		float64(c.cfg.LogMinDurationMs),
		c.cfg.MaxPlansPerCycle,
	)
	if err != nil {
		return fmt.Errorf("query slow queries: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		queryID int64
		query   string
	}
	var candidates []candidate
	for rows.Next() {
		var cand candidate
		if err := rows.Scan(&cand.queryID, &cand.query); err != nil {
			return fmt.Errorf("scan candidate: %w", err)
		}
		if selfmonitor.IsQueryText(cand.query) {
			continue
		}
		candidates = append(candidates, cand)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate candidates: %w", err)
	}

	for _, cand := range candidates {
		if !isExplainable(cand.query) {
			continue
		}
		if err := c.captureOnDemand(
			ctx, cand.queryID, cand.query,
		); err != nil {
			c.logFn(
				"WARN", "autoexplain: capture queryid=%d: %v", cand.queryID, err,
			)
		}
	}
	return nil
}

const candidateSQL = `
		SELECT s.queryid, s.query
		FROM pg_stat_statements s
		LEFT JOIN sage.explain_cache e
			ON e.queryid = s.queryid
			AND e.captured_at > now() - interval '1 day'
		WHERE s.mean_exec_time > $1
			AND s.calls > 10
			AND s.dbid = (
				SELECT oid FROM pg_database
				WHERE datname = current_database()
			)
			AND COALESCE(s.query, '') NOT ILIKE '%pg_sage%'
			AND COALESCE(s.query, '') !~* '(^|[^[:alnum:]_])("?sage"?)[[:space:]]*\.'
			AND e.id IS NULL
		ORDER BY s.mean_exec_time DESC
		LIMIT $2`

// captureOnDemand runs EXPLAIN on a single query inside a read-only,
// rolled-back transaction so there are no side effects.
//
// Normalized pg_stat_statements text carries $n placeholders. Binding
// every parameter to NULL (the previous approach) constant-folds most
// predicates to "One-Time Filter: false", a degenerate plan that was then
// stored as auto_explain evidence and preferred over better sources
// (G1-B09). PG16+ captures the GENERIC_PLAN and labels it generic_plan;
// older servers skip parameterized statements entirely.
func (c *Collector) captureOnDemand(
	ctx context.Context,
	queryID int64,
	query string,
) error {
	if err := sanitize.RejectMultiStatement(query); err != nil {
		return fmt.Errorf("unsafe query text: %w", err)
	}
	source := "auto_explain"
	generic := parameterCount(query) > 0
	if generic {
		version, err := c.serverVersion(ctx)
		if err != nil {
			return err
		}
		if version < 160000 {
			c.logFn("DEBUG", "autoexplain: skip parameterized queryid=%d: "+
				"GENERIC_PLAN requires PostgreSQL 16+", queryID)
			return nil
		}
		source = "generic_plan"
	}
	planJSON, err := c.explainReadOnly(ctx, query, generic)
	if err != nil {
		return err
	}
	totalCost, execTime := extractPlanMetrics(planJSON)
	return c.storePlan(ctx, queryID, query, planJSON, source, totalCost, execTime)
}

// explainReadOnly returns the EXPLAIN JSON for query. auto_explain
// settings are applied with SET LOCAL so they end with the transaction
// instead of leaking into the pooled session (G1-B15). The connection is
// released before the plan is stored.
func (c *Collector) explainReadOnly(
	ctx context.Context, query string, generic bool,
) ([]byte, error) {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only tx, always rolled back

	if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		return nil, fmt.Errorf("set read only: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '5s'"); err != nil {
		return nil, fmt.Errorf("set statement_timeout: %w", err)
	}
	if c.avail != nil && (c.avail.SessionLoad || c.avail.SharedPreload ||
		c.avail.AlreadyLoaded) {
		scfg := DefaultSessionConfig(c.cfg.LogMinDurationMs)
		if err := ConfigureTransaction(ctx, tx, c.avail, scfg); err != nil {
			return nil, fmt.Errorf("configure auto_explain: %w", err)
		}
	}
	explain := "EXPLAIN (FORMAT JSON) "
	if generic {
		explain = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	return scanPlanJSON(ctx, tx, explain+query)
}

// scanPlanJSON runs one already-validated EXPLAIN over the simple query
// protocol: pgx's extended protocol rejects intentionally unbound $n
// placeholders, which GENERIC_PLAN needs.
func scanPlanJSON(ctx context.Context, tx pgx.Tx, sql string) ([]byte, error) {
	results, err := tx.Conn().PgConn().Exec(ctx, sql).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("explain: %w", err)
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		return nil, fmt.Errorf("explain: expected one JSON plan row")
	}
	return results[0].Rows[0][0], nil
}

// serverVersion returns server_version_num, cached after the first read.
func (c *Collector) serverVersion(ctx context.Context) (int, error) {
	if c.serverVersionNum > 0 {
		return c.serverVersionNum, nil
	}
	var raw string
	if err := c.pool.QueryRow(ctx, "SHOW server_version_num").Scan(&raw); err != nil {
		return 0, fmt.Errorf("read server_version_num: %w", err)
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse server_version_num %q: %w", raw, err)
	}
	c.serverVersionNum = version
	return version, nil
}

var parameterPlaceholder = regexp.MustCompile(`\$([1-9][0-9]*)`)

func parameterCount(query string) int {
	maxParam := 0
	for _, match := range parameterPlaceholder.FindAllStringSubmatch(query, -1) {
		param, err := strconv.Atoi(match[1])
		if err == nil && param > maxParam {
			maxParam = param
		}
	}
	return maxParam
}

// storePlan inserts a captured plan into sage.explain_cache.
func (c *Collector) storePlan(
	ctx context.Context,
	queryID int64,
	queryText string,
	planJSON []byte,
	source string,
	totalCost float64,
	execTime float64,
) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO sage.explain_cache
			(queryid, query_text, plan_json, source,
			 total_cost, execution_time)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		queryID, queryText, planJSON, source, totalCost, execTime,
	)
	if err != nil {
		return fmt.Errorf("insert explain_cache: %w", err)
	}
	return nil
}

// extractPlanMetrics pulls total_cost and execution_time from
// EXPLAIN JSON output.
func extractPlanMetrics(planJSON []byte) (float64, float64) {
	var wrapper []struct {
		Plan struct {
			TotalCost float64 `json:"Total Cost"`
		} `json:"Plan"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(planJSON, &wrapper); err != nil {
		return 0, 0
	}
	if len(wrapper) == 0 {
		return 0, 0
	}
	return wrapper[0].Plan.TotalCost, wrapper[0].ExecutionTime
}

// isExplainable returns true if the query is a DML statement that
// can be wrapped in EXPLAIN. Utility commands (SET, VACUUM, COPY,
// DDL) are filtered out.
func isExplainable(query string) bool {
	q := strings.TrimSpace(query)
	if q == "" {
		return false
	}
	upper := strings.ToUpper(q)
	for _, prefix := range explainablePrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

var explainablePrefixes = []string{
	"SELECT",
	"INSERT",
	"UPDATE",
	"DELETE",
	"WITH",
}

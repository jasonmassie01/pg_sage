package autoexplain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sanitize"
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
	interval := time.Duration(
		c.cfg.CollectIntervalSeconds,
	) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.Collect(ctx); err != nil {
				c.logFn(
					"WARN", "autoexplain",
					"collect cycle: %v", err,
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
	rows, err := c.pool.Query(ctx, `
		SELECT s.queryid, s.query
		FROM pg_stat_statements s
		LEFT JOIN sage.explain_cache e
			ON e.queryid = s.queryid
			AND e.captured_at > now() - interval '1 day'
		WHERE s.mean_exec_time > $1
			AND s.calls > 10
			AND e.id IS NULL
		ORDER BY s.mean_exec_time DESC
		LIMIT $2`,
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
				"WARN", "autoexplain",
				"capture queryid=%d: %v", cand.queryID, err,
			)
		}
	}
	return nil
}

// captureOnDemand runs EXPLAIN on a single query inside a
// rolled-back transaction so there are no side effects.
func (c *Collector) captureOnDemand(
	ctx context.Context,
	queryID int64,
	query string,
) error {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if c.avail.SessionLoad || c.avail.SharedPreload {
		scfg := DefaultSessionConfig(c.cfg.LogMinDurationMs)
		if err := ConfigureSession(
			ctx, conn, c.avail, scfg,
		); err != nil {
			return fmt.Errorf("configure session: %w", err)
		}
	}

	if err := sanitize.RejectMultiStatement(query); err != nil {
		return fmt.Errorf("unsafe query text: %w", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	_, _ = tx.Exec(ctx, "SET LOCAL statement_timeout = '5s'")
	_, _ = tx.Exec(ctx, "SET TRANSACTION READ ONLY")

	explainSQL := fmt.Sprintf(
		"EXPLAIN (FORMAT JSON) %s", query,
	)
	var planJSON []byte
	if err := tx.QueryRow(ctx, explainSQL).Scan(
		&planJSON,
	); err != nil {
		return fmt.Errorf("explain: %w", err)
	}

	totalCost, execTime := extractPlanMetrics(planJSON)
	return c.storePlan(
		ctx, queryID, query, planJSON, totalCost, execTime,
	)
}

// storePlan inserts a captured plan into sage.explain_cache.
func (c *Collector) storePlan(
	ctx context.Context,
	queryID int64,
	queryText string,
	planJSON []byte,
	totalCost float64,
	execTime float64,
) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO sage.explain_cache
			(queryid, query_text, plan_json, source,
			 total_cost, execution_time)
		VALUES ($1, $2, $3, 'auto_explain', $4, $5)`,
		queryID, queryText, planJSON, totalCost, execTime,
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

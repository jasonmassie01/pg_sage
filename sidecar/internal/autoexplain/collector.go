package autoexplain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/workload"
)

// CollectorConfig holds configuration for the auto_explain
// collector.
type CollectorConfig struct {
	CollectIntervalSeconds int
	MaxPlansPerCycle       int
	LogMinDurationMs       int
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
	// statementsRel overrides the qualified pg_stat_statements view, which
	// is otherwise resolved from the catalog each cycle: a test seam for a
	// fixture table with chosen candidates.
	statementsRel string
}

// statements is the qualified pg_stat_statements view the cycle reads.
func (c *Collector) statements(ctx context.Context) (string, error) {
	if c.statementsRel != "" {
		return c.statementsRel, nil
	}
	return extschema.StatementsRel(ctx, c.pool, "pg_stat_statements")
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
	ctx, done := selfbudget.Process().TrackFor(ctx, "auto_explain")
	defer done()
	rel, err := c.statements(ctx)
	if err != nil {
		return fmt.Errorf("query slow queries: %w", err)
	}
	rows, err := c.pool.Query(ctx, candidateSQL(rel),
		float64(c.cfg.LogMinDurationMs),
		c.cfg.MaxPlansPerCycle,
	)
	if err != nil {
		return fmt.Errorf("query slow queries: %w", err)
	}
	defer rows.Close()

	var candidates []candidate
	for rows.Next() {
		var cand candidate
		if err := rows.Scan(&cand.queryID, &cand.query); err != nil {
			return fmt.Errorf("scan candidate: %w", err)
		}
		if workload.Excluded(cand.query) {
			continue
		}
		candidates = append(candidates, cand)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate candidates: %w", err)
	}

	c.captureCandidates(ctx, candidates)
	return nil
}

// candidate is one slow statement without a recent plan.
type candidate struct {
	queryID int64
	query   string
}

// captureCandidates captures a plan for each explainable candidate. A
// candidate whose relation lock is busy (DDL queued on it) is skipped for
// this cycle and retried on the next one; the cycle reports the skips once.
func (c *Collector) captureCandidates(ctx context.Context, candidates []candidate) {
	skipped := 0
	for _, cand := range candidates {
		if ok, _ := workload.Plannable(cand.query); !ok {
			continue
		}
		if c.reportCapture(cand.queryID, c.captureOnDemand(ctx, cand.queryID, cand.query)) {
			skipped++
		}
	}
	if skipped > 0 {
		c.logFn("INFO", "autoexplain: %d candidates skipped: lock busy", skipped)
	}
}

// reportCapture logs a capture's outcome and reports whether it was
// skipped for a busy lock: that is DEBUG, every other failure is WARN.
func (c *Collector) reportCapture(queryID int64, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errLockBusy):
		c.logFn("DEBUG", "autoexplain: skip queryid=%d this cycle: %v", queryID, err)
		return true
	default:
		c.logFn("WARN", "autoexplain: capture queryid=%d: %v", queryID, err)
		return false
	}
}

// candidateSQL reads slow statements from rel, the qualified
// pg_stat_statements view, that have no plan from the last day or whose
// newest plan predates an ANALYZE (auto or manual) of a relation it reads:
// a plan flip after a statistics refresh is then seen within one capture
// interval (guard-02c). Each statement's newest plan is read through
// idx_explain_queryid.
func candidateSQL(rel string) string {
	return `
		SELECT s.queryid, s.query
		FROM ` + rel + ` s
		LEFT JOIN LATERAL (
			SELECT e.captured_at, e.plan_json FROM sage.explain_cache e
			 WHERE e.queryid = s.queryid AND e.source <> 'lakebase_plan_history'
			   AND e.captured_at > now() - interval '1 day'
			 ORDER BY e.captured_at DESC LIMIT 1) e ON true
		WHERE s.mean_exec_time > $1
			AND s.calls > 10
			AND s.dbid = (
				SELECT oid FROM pg_database
				WHERE datname = current_database()
			)
			AND ` + workload.AdviceSQL("s.query") + `
			AND (e.captured_at IS NULL OR EXISTS (
				SELECT 1
				  FROM jsonb_path_query(e.plan_json, 'strict $.**."Relation Name"') AS r(name)
				  JOIN pg_catalog.pg_stat_user_tables t ON t.relname = r.name #>> '{}'
				 WHERE GREATEST(t.last_analyze, t.last_autoanalyze) > e.captured_at))
		ORDER BY s.mean_exec_time DESC
		LIMIT $2`
}

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
	source := estimateSource
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
		return lockBusy(err)
	}
	totalCost, execTime := extractPlanMetrics(planJSON)
	return c.storePlan(ctx, queryID, query, planJSON, source, totalCost, execTime)
}

// estimateSource labels an on-demand plain EXPLAIN: an estimate, never
// an executed plan. auto_explain logs only executed statements, so the
// capture is not an auto_explain plan (rca-04); the auto_explain log's own
// plans are stored as auto_explain_log.
const estimateSource = "explain_estimate"

// sqlStateLockNotAvailable is the SQLSTATE of a lock_timeout.
const sqlStateLockNotAvailable = "55P03"

// errLockBusy marks a capture that gave up waiting for a relation lock.
var errLockBusy = errors.New("relation lock busy")

// lockBusy wraps a lock_timeout in errLockBusy, keeping the server error;
// any other error is returned as it is.
func lockBusy(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlStateLockNotAvailable {
		return fmt.Errorf("%w: %w", errLockBusy, err)
	}
	return err
}

// The on-demand EXPLAIN is a probe of the monitored database, bounded like
// the SRE probes (rca-05). Planning takes AccessShare or RowExclusive locks:
// queued behind an ALTER TABLE it would hold the statement's other locks
// for the whole statement_timeout, so it gives up after the shared probe
// lock timeout. The idle timeout ends the session if the transaction is
// ever left open, so it cannot keep its snapshot or its locks.
var (
	explainLockTimeoutSQL = fmt.Sprintf("SET LOCAL lock_timeout = '%dms'",
		selfmonitor.ProbeLockTimeout.Milliseconds())
	explainIdleTimeoutSQL = "SET LOCAL idle_in_transaction_session_timeout = '10s'"
)

// explainReadOnly returns the EXPLAIN JSON for query. The bounds are
// applied with SET LOCAL so they end with the transaction instead of
// leaking into the pooled session (G1-B15). No auto_explain setting is
// sent: auto_explain never logs an EXPLAIN statement (rca-04). The
// connection is released before the plan is stored.
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
	if _, err := tx.Exec(ctx, explainLockTimeoutSQL); err != nil {
		return nil, fmt.Errorf("set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, explainIdleTimeoutSQL); err != nil {
		return nil, fmt.Errorf("set idle_in_transaction_session_timeout: %w", err)
	}
	// The statement is the application's: plan it on its search_path, not
	// pg_sage's pinned one (security-01c-n).
	if err := apppath.Pin(ctx, tx); err != nil {
		return nil, err
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

// planHashParam fingerprints a plan for sage.explain_cache.plan_hash,
// naming partitions by their roots as read from db, the monitored
// database the plan ran on (collection-09). A plan that cannot be
// fingerprinted is stored with NULL. When the roots cannot be read the
// hash is the unresolved v2u fingerprint and the read error is returned
// beside it for the caller to log.
func planHashParam(ctx context.Context, db planhash.Beginner, planJSON []byte) (*string, error) {
	roots, resolveErr := planhash.Resolve(ctx, db, planJSON)
	h, err := planhash.ComputeWith(planJSON, planhash.Options{Roots: roots})
	if err != nil {
		return nil, nil
	}
	return &h, resolveErr
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
	hash, resolveErr := planHashParam(ctx, c.pool, planJSON)
	if resolveErr != nil {
		c.logFn("WARN", "autoexplain: query %d plan stored with an unresolved "+
			"fingerprint: %v", queryID, resolveErr)
	}
	_, err := c.pool.Exec(ctx, `
		INSERT INTO sage.explain_cache
			(queryid, query_text, plan_json, source,
			 total_cost, execution_time, plan_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		queryID, queryText, planJSON, source, totalCost, execTime, hash,
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

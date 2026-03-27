package executor

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Executor runs the autonomous remediation loop after each analyzer cycle.
type Executor struct {
	pool      *pgxpool.Pool
	cfg       *config.Config
	analyzer  *analyzer.Analyzer
	rampStart time.Time
	logFn     func(string, string, ...any)
}

// New creates a new Executor.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	a *analyzer.Analyzer,
	rampStart time.Time,
	logFn func(string, string, ...any),
) *Executor {
	return &Executor{
		pool:      pool,
		cfg:       cfg,
		analyzer:  a,
		rampStart: rampStart,
		logFn:     logFn,
	}
}

// RunCycle is called after each analyzer cycle to evaluate and execute
// any actionable findings.
func (e *Executor) RunCycle(ctx context.Context, isReplica bool) {
	emergencyStop := CheckEmergencyStop(ctx, e.pool)
	if emergencyStop {
		e.logFn("executor", "emergency stop active — skipping cycle")
		return
	}

	findings := e.analyzer.Findings()

	for _, f := range findings {
		if f.RecommendedSQL == "" {
			continue
		}

		if f.Severity == "info" {
			continue
		}

		if !ShouldExecute(f, e.cfg, e.rampStart, isReplica, emergencyStop) {
			continue
		}

		findingID := e.lookupFindingID(ctx, f)
		if findingID <= 0 {
			continue
		}

		if CheckHysteresis(ctx, e.pool, findingID,
			e.cfg.Trust.RollbackCooldownDays) {
			e.logFn("executor",
				"skipping %q — rolled back recently (cooldown)",
				f.Title,
			)
			continue
		}

		beforeState := e.snapshotBeforeState(ctx)

		ddlTimeout := e.cfg.Safety.DDLTimeout()
		var execErr error
		if NeedsConcurrently(f.RecommendedSQL) || NeedsTopLevel(f.RecommendedSQL) {
			execErr = ExecConcurrently(
				ctx, e.pool, f.RecommendedSQL, ddlTimeout,
			)
		} else {
			execErr = ExecInTransaction(
				ctx, e.pool, f.RecommendedSQL, ddlTimeout,
			)
		}

		actionID := e.logAction(ctx, f, findingID, beforeState, execErr)
		if execErr != nil {
			e.logFn("executor",
				"execution failed for %q: %v", f.Title, execErr,
			)
			continue
		}

		e.logFn("executor", "executed %q (action %d)", f.Title, actionID)

		// Post-check: verify index validity after CREATE INDEX.
		if NeedsConcurrently(f.RecommendedSQL) {
			idxName := extractIndexName(f.RecommendedSQL)
			if idxName != "" {
				valid, err := optimizer.CheckIndexValid(
					ctx, e.pool, idxName,
				)
				if err != nil {
					e.logFn("executor",
						"post-check failed for index %s: %v",
						idxName, err,
					)
				} else if !valid {
					e.logFn("executor",
						"CRITICAL: index %s is INVALID after creation",
						idxName,
					)
				}
			}
		}

		// Handle INCLUDE upgrade: DROP old index after verifying new one.
		if dropOld, ok := f.Detail["drop_ddl"].(string); ok && dropOld != "" {
			idxName := extractIndexName(f.RecommendedSQL)
			valid, checkErr := optimizer.CheckIndexValid(ctx, e.pool, idxName)
			if checkErr != nil || !valid {
				e.logFn("executor",
					"new index %s invalid — preserving old index", idxName)
			} else {
				dropErr := ExecConcurrently(ctx, e.pool, dropOld, ddlTimeout)
				if dropErr != nil {
					e.logFn("executor",
						"DROP old index failed (new index valid): %v", dropErr)
				} else {
					e.logFn("executor",
						"dropped old index after INCLUDE upgrade: %s", dropOld)
				}
			}
		}

		if f.RollbackSQL != "" && actionID > 0 {
			go MonitorAndRollback(
				ctx, e.pool, actionID, f.RollbackSQL,
				e.cfg.Trust.RollbackThresholdPct,
				e.cfg.Trust.RollbackWindowMinutes,
				e.logFn,
			)
		} else if actionID > 0 {
			// No rollback possible (VACUUM, ANALYZE, pg_terminate_backend)
			// — mark success immediately.
			updateActionSuccess(ctx, e.pool, actionID)
		}
	}
}

// lookupFindingID retrieves the database ID for an open finding.
func (e *Executor) lookupFindingID(
	ctx context.Context, f analyzer.Finding,
) int64 {
	var id int64
	err := e.pool.QueryRow(ctx,
		`SELECT id FROM sage.findings
		 WHERE category = $1
		   AND object_identifier = $2
		   AND status = 'open'
		 LIMIT 1`,
		f.Category, f.ObjectIdentifier,
	).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// snapshotBeforeState captures current database health metrics
// to serve as a comparison baseline for rollback decisions.
func (e *Executor) snapshotBeforeState(ctx context.Context) map[string]any {
	state := map[string]any{}

	var cacheHit float64
	err := e.pool.QueryRow(ctx,
		`SELECT coalesce(
			sum(blks_hit)::float /
			nullif(sum(blks_hit) + sum(blks_read), 0),
			1.0
		 ) FROM pg_stat_database`,
	).Scan(&cacheHit)
	if err == nil {
		state["cache_hit_ratio"] = cacheHit
	}

	var activeBackends int
	err = e.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		 WHERE state = 'active'`,
	).Scan(&activeBackends)
	if err == nil {
		state["active_backends"] = activeBackends
	}

	var meanExecMs float64
	err = e.pool.QueryRow(ctx,
		`SELECT coalesce(avg(mean_exec_time), 0)
		 FROM pg_stat_statements
		 WHERE query LIKE 'INSERT%' OR query LIKE 'UPDATE%'`,
	).Scan(&meanExecMs)
	if err == nil && meanExecMs > 0 {
		state["mean_exec_time_ms"] = meanExecMs
	}

	return state
}

// logAction records the executed action in sage.action_log.
func (e *Executor) logAction(
	ctx context.Context,
	f analyzer.Finding,
	findingID int64,
	beforeState map[string]any,
	execErr error,
) int64 {
	beforeJSON, _ := json.Marshal(beforeState)

	outcome := "pending"
	if execErr != nil {
		outcome = "failed"
	}

	actionType := categorizeAction(f.RecommendedSQL)

	var actionID int64
	err := e.pool.QueryRow(ctx,
		`INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, rollback_sql,
		  before_state, outcome)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		actionType, findingID, f.RecommendedSQL,
		nilIfEmpty(f.RollbackSQL), beforeJSON, outcome,
	).Scan(&actionID)
	if err != nil {
		e.logFn("executor",
			"failed to log action for %q: %v", f.Title, err,
		)
		return 0
	}

	// Link the finding to this action.
	_, _ = e.pool.Exec(ctx,
		`UPDATE sage.findings
		 SET acted_on_at = now(), action_log_id = $1
		 WHERE id = $2`,
		actionID, findingID,
	)

	return actionID
}

// categorizeAction derives an action_type label from the SQL statement.
func categorizeAction(sql string) string {
	upper := strings.ToUpper(sql)
	switch {
	case strings.Contains(upper, "CREATE INDEX"):
		return "create_index"
	case strings.Contains(upper, "DROP INDEX"):
		return "drop_index"
	case strings.Contains(upper, "REINDEX"):
		return "reindex"
	case strings.Contains(upper, "VACUUM"):
		return "vacuum"
	case strings.Contains(upper, "ANALYZE"):
		return "analyze"
	case strings.Contains(upper, "PG_TERMINATE_BACKEND"):
		return "terminate_backend"
	case strings.Contains(upper, "ALTER"):
		return "alter"
	default:
		return "ddl"
	}
}

// nilIfEmpty returns nil for empty strings, used for nullable SQL params.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// extractIndexName parses the index name from a CREATE INDEX statement.
func extractIndexName(sql string) string {
	upper := strings.ToUpper(sql)
	idx := strings.Index(upper, "INDEX")
	if idx < 0 {
		return ""
	}
	rest := sql[idx+5:]
	// Skip optional "CONCURRENTLY" and "IF NOT EXISTS".
	upper = strings.ToUpper(strings.TrimSpace(rest))
	if strings.HasPrefix(upper, "CONCURRENTLY") {
		rest = strings.TrimSpace(rest[12:])
		upper = strings.ToUpper(rest)
	}
	if strings.HasPrefix(upper, "IF NOT EXISTS") {
		rest = strings.TrimSpace(rest[13:])
	}
	// Next token is the index name.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	name := strings.Trim(fields[0], "\"")
	return name
}

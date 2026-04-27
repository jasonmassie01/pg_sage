package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// EventDispatcher sends notification events. Nil means no
// notifications (backward-compatible default).
type EventDispatcher interface {
	Dispatch(ctx context.Context, event notify.Event) error
}

// ActionProposer defines the subset of ActionStore the executor needs.
// Nil means auto mode (no queueing).
type ActionProposer interface {
	Propose(ctx context.Context, databaseID *int,
		findingID int, sql, rollbackSQL, risk string) (int, error)
}

// PendingActionChecker is implemented by stores that can detect an existing
// unresolved proposal for the same finding.
type PendingActionChecker interface {
	HasPendingForFinding(ctx context.Context, findingID int) (bool, error)
}

// PendingActionSQLChecker is implemented by stores that can detect an
// unresolved proposal with the same SQL text.
type PendingActionSQLChecker interface {
	HasPendingForSQL(ctx context.Context, sql string) (bool, error)
}

// RejectedActionChecker is implemented by stores that can detect a recent
// operator rejection. Approval mode uses this to avoid immediately
// re-proposing an action the user just rejected.
type RejectedActionChecker interface {
	HasRecentlyRejectedForFinding(
		ctx context.Context, findingID int, cooldown time.Duration,
	) (bool, error)
	HasRecentlyRejectedForSQL(
		ctx context.Context, sql string, cooldown time.Duration,
	) (bool, error)
}

// maxConcurrentDDL limits how many DDL operations can run
// in parallel to avoid overwhelming the database.
const maxConcurrentDDL = 3

// Executor runs the autonomous remediation loop after each
// analyzer cycle.
type Executor struct {
	pool               *pgxpool.Pool
	cfg                *config.Config
	analyzer           *analyzer.Analyzer
	rampStart          time.Time
	recentActions      map[string]time.Time
	logFn              func(string, string, ...any)
	actionStore        ActionProposer
	execMode           string // auto, approval, manual
	dispatcher         EventDispatcher
	databaseName       string
	trustLevelOverride string
	ddlSem             chan struct{} // limits concurrent DDL ops
	analyzeSem         chan struct{} // shared fleet-wide for ANALYZE

	// monitors tracks background MonitorAndRollback goroutines so
	// Shutdown can wait for them. shutdownCh is closed to signal
	// monitors to abort their rollback window early.
	monitors     sync.WaitGroup
	shutdownCh   chan struct{}
	shutdownMu   sync.Mutex
	shuttingDown bool
}

// WithAnalyzeSemaphore wires a shared process-wide semaphore
// used to serialize ANALYZE actions across every database
// managed by the sidecar. nil is safe and disables gating.
func (e *Executor) WithAnalyzeSemaphore(sem chan struct{}) {
	e.analyzeSem = sem
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
		pool:          pool,
		cfg:           cfg,
		analyzer:      a,
		rampStart:     rampStart,
		recentActions: make(map[string]time.Time),
		logFn:         logFn,
		execMode:      "auto",
		ddlSem:        make(chan struct{}, maxConcurrentDDL),
		shutdownCh:    make(chan struct{}),
	}
}

// Shutdown signals in-flight rollback monitors to abort their
// wait window and waits until they have returned (or the
// provided context is done, whichever comes first). After
// Shutdown returns, RunCycle must not be called again.
func (e *Executor) Shutdown(ctx context.Context) error {
	e.shutdownMu.Lock()
	if !e.shuttingDown {
		e.shuttingDown = true
		close(e.shutdownCh)
	}
	e.shutdownMu.Unlock()

	done := make(chan struct{})
	go func() {
		e.monitors.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WithActionStore sets the action store and execution mode.
// This enables approval/manual mode queueing.
func (e *Executor) WithActionStore(
	as ActionProposer, mode string,
) {
	e.actionStore = as
	if mode != "" {
		e.execMode = mode
	}
}

// WithDispatcher sets the notification dispatcher. Nil is safe
// and means no notifications are sent (default).
func (e *Executor) WithDispatcher(d EventDispatcher) {
	e.dispatcher = d
}

// WithDatabaseName sets the database name included in events.
func (e *Executor) WithDatabaseName(name string) {
	e.databaseName = name
}

// validTrustLevels enumerates the accepted trust level strings.
// Empty string is allowed to clear an override.
var validTrustLevels = map[string]bool{
	"observation": true,
	"advisory":    true,
	"autonomous":  true,
	"":            true,
}

// SetTrustLevel overrides the global trust level for this
// executor instance. Empty string clears the override.
// Returns an error if the level is not recognized.
func (e *Executor) SetTrustLevel(level string) error {
	if !validTrustLevels[level] {
		return fmt.Errorf("invalid trust level: %q", level)
	}
	e.trustLevelOverride = level
	return nil
}

// TrustLevel returns the effective trust level for this executor.
func (e *Executor) TrustLevel() string {
	if e.trustLevelOverride != "" {
		return e.trustLevelOverride
	}
	return e.cfg.Trust.Level
}

// SetExecutionMode changes the execution mode at runtime.
func (e *Executor) SetExecutionMode(mode string) {
	e.execMode = mode
}

// ExecutionMode returns the current execution mode.
func (e *Executor) ExecutionMode() string {
	return e.execMode
}

// shouldExecute checks whether a finding should be executed,
// respecting any per-instance trust level override.
func (e *Executor) shouldExecute(
	f analyzer.Finding, isReplica, emergencyStop bool,
) bool {
	if e.trustLevelOverride == "" {
		return ShouldExecute(
			f, e.cfg, e.rampStart, isReplica, emergencyStop)
	}
	cfgCopy := *e.cfg
	cfgCopy.Trust.Level = e.trustLevelOverride
	return ShouldExecute(
		f, &cfgCopy, e.rampStart, isReplica, emergencyStop)
}

// RunCycle is called after each analyzer cycle to evaluate and execute
// any actionable findings.
func (e *Executor) RunCycle(ctx context.Context, isReplica bool) {
	// Manual mode: no auto-proposals or executions.
	if e.execMode == "manual" {
		return
	}

	emergencyStop := CheckEmergencyStop(ctx, e.pool)
	if emergencyStop {
		e.logFn("executor", "emergency stop active — skipping cycle")
		return
	}

	e.pruneRecentActions()
	findings := e.analyzer.Findings()

	for _, f := range findings {
		if f.RecommendedSQL == "" {
			continue
		}

		if !e.shouldExecute(f, isReplica, emergencyStop) {
			continue
		}

		if e.isCascadeCooldown(f.ObjectIdentifier) {
			continue
		}

		findingID := e.lookupFindingID(ctx, f)
		if findingID <= 0 {
			continue
		}

		if e.exceedsMaxRetries(ctx, findingID) {
			continue
		}

		// Approval mode: queue for approval instead of executing.
		if e.execMode == "approval" && e.actionStore != nil {
			if checker, ok := e.actionStore.(PendingActionChecker); ok {
				hasPending, err := checker.HasPendingForFinding(
					ctx, int(findingID))
				if err != nil {
					e.logFn("executor",
						"failed to check pending approval for %q: %v",
						f.Title, err)
					continue
				}
				if hasPending {
					continue
				}
			}
			if checker, ok := e.actionStore.(PendingActionSQLChecker); ok {
				hasPending, err := checker.HasPendingForSQL(
					ctx, f.RecommendedSQL)
				if err != nil {
					e.logFn("executor",
						"failed to check duplicate approval SQL for %q: %v",
						f.Title, err)
					continue
				}
				if hasPending {
					continue
				}
			}
			if checker, ok := e.actionStore.(RejectedActionChecker); ok {
				cooldown := e.cascadeCooldown()
				rejected, err := checker.HasRecentlyRejectedForFinding(
					ctx, int(findingID), cooldown)
				if err != nil {
					e.logFn("executor",
						"failed to check rejected approval for %q: %v",
						f.Title, err)
					continue
				}
				if rejected {
					continue
				}
				rejected, err = checker.HasRecentlyRejectedForSQL(
					ctx, f.RecommendedSQL, cooldown)
				if err != nil {
					e.logFn("executor",
						"failed to check rejected approval SQL for %q: %v",
						f.Title, err)
					continue
				}
				if rejected {
					continue
				}
			}

			_, propErr := e.actionStore.Propose(
				ctx, nil, int(findingID),
				f.RecommendedSQL, f.RollbackSQL, f.ActionRisk,
			)
			if propErr != nil {
				e.logFn("executor",
					"failed to queue %q for approval: %v",
					f.Title, propErr)
			} else {
				e.logFn("executor",
					"queued %q for approval", f.Title)
				e.dispatchEvent(ctx,
					notify.ApprovalNeededEvent(
						f.Title, f.RecommendedSQL,
						e.databaseName, f.ActionRisk))
			}
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

		// Limit concurrent DDL to avoid overwhelming the database.
		select {
		case e.ddlSem <- struct{}{}:
			// acquired — will release after execution
		default:
			e.logFn("executor",
				"DDL concurrency limit reached, skipping %s",
				f.RecommendedSQL)
			continue
		}

		e.executeFinding(ctx, f, findingID)
		<-e.ddlSem
	}
}

// executeFinding runs the DDL for a single finding and handles
// post-execution checks, rollback monitoring, and invalid index cleanup.
func (e *Executor) executeFinding(
	ctx context.Context, f analyzer.Finding, findingID int64,
) {
	beforeState := e.snapshotBeforeState(ctx)

	ddlTimeout := e.cfg.Safety.DDLTimeout()
	lockOpt := WithLockTimeout(e.cfg.Safety.LockTimeout())
	var execErr error
	switch {
	case categorizeAction(f.RecommendedSQL) == "analyze":
		execErr = e.executeAnalyze(ctx, f)
	case NeedsConcurrently(f.RecommendedSQL) ||
		NeedsTopLevel(f.RecommendedSQL):
		execErr = ExecConcurrently(
			ctx, e.pool, f.RecommendedSQL,
			ddlTimeout, lockOpt,
		)
	default:
		execErr = ExecInTransaction(
			ctx, e.pool, f.RecommendedSQL,
			ddlTimeout, lockOpt,
		)
	}

	actionID := e.logAction(ctx, f, findingID, beforeState, execErr)
	if execErr != nil {
		if errors.Is(execErr, ErrLockNotAvailable) {
			e.logFn("executor",
				"lock timeout for %q on %s — circuit-breaking table",
				f.Title, f.ObjectIdentifier,
			)
			e.recentActions[f.ObjectIdentifier] = time.Now()
		}
		e.logFn("executor",
			"execution failed for %q: %v", f.Title, execErr,
		)
		e.dispatchEvent(ctx,
			notify.ActionFailedEvent(
				f.Title, f.RecommendedSQL,
				e.databaseName, execErr.Error()))
		return
	}

	e.logFn("executor",
		"executed %q (action %d)", f.Title, actionID,
	)
	e.dispatchEvent(ctx,
		notify.ActionExecutedEvent(
			f.Title, f.RecommendedSQL, e.databaseName))
	e.recentActions[f.ObjectIdentifier] = time.Now()

	// Post-check: verify index validity after CREATE INDEX only. DROP INDEX
	// and other concurrently-required statements do not produce a new index.
	if categorizeAction(f.RecommendedSQL) == "create_index" {
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
				// Auto-cleanup: drop the invalid index.
				dropSQL := fmt.Sprintf(
					"DROP INDEX CONCURRENTLY IF EXISTS %s",
					idxName,
				)
				dropErr := ExecConcurrently(
					ctx, e.pool, dropSQL, ddlTimeout, lockOpt,
				)
				if dropErr != nil {
					// Surface loudly: the invalid index is now
					// occupying disk, blocking future CREATE
					// INDEX with the same name, and slowing
					// writes until it is removed manually.
					e.logFn("executor",
						"CRITICAL: failed to drop invalid index %s: "+
							"%v — manual cleanup required "+
							"(run: %s)",
						idxName, dropErr, dropSQL)
					e.dispatchEvent(ctx,
						notify.ActionFailedEvent(
							"Invalid index cleanup failed: "+idxName,
							dropSQL,
							e.databaseName,
							fmt.Sprintf("manual DROP required: %v",
								dropErr)))
					e.recordInvalidIndexCleanupFailure(
						ctx, findingID, idxName, dropErr)
				} else {
					e.logFn("executor",
						"cleaned up invalid index %s", idxName)
				}
			}
		}
	}

	// Handle INCLUDE upgrade: DROP old index after verifying new one.
	if dropOld, ok := f.Detail["drop_ddl"].(string); ok &&
		dropOld != "" {
		idxName := extractIndexName(f.RecommendedSQL)
		valid, checkErr := optimizer.CheckIndexValid(
			ctx, e.pool, idxName,
		)
		if checkErr != nil || !valid {
			e.logFn("executor",
				"new index %s invalid — preserving old index",
				idxName)
		} else {
			dropErr := ExecConcurrently(
				ctx, e.pool, dropOld, ddlTimeout, lockOpt,
			)
			if dropErr != nil {
				e.logFn("executor",
					"DROP old index failed (new index valid): %v",
					dropErr)
			} else {
				e.logFn("executor",
					"dropped old index after INCLUDE upgrade: %s",
					dropOld)
			}
		}
	}

	if f.RollbackSQL != "" && actionID > 0 {
		// Detach the monitor from the cycle's context so the
		// rollback window can elapse even if RunCycle returns
		// (or is called from an HTTP handler). Shutdown signals
		// the monitor to abort early via e.shutdownCh.
		e.monitors.Add(1)
		go func() {
			defer e.monitors.Done()
			MonitorAndRollback(
				context.WithoutCancel(ctx), e.pool, actionID, f.RollbackSQL,
				e.cfg.Trust.RollbackThresholdPct,
				e.cfg.Trust.RollbackWindowMinutes,
				e.logFn,
				e.shutdownCh,
			)
		}()
	} else if actionID > 0 {
		// No rollback possible (VACUUM, ANALYZE, pg_terminate_backend)
		// — mark success immediately.
		updateActionSuccess(ctx, e.pool, actionID)
	}
}

// dispatchEvent sends a notification event if the dispatcher is set.
// Errors are logged but do not interrupt the executor flow.
func (e *Executor) dispatchEvent(
	ctx context.Context, event notify.Event,
) {
	if e.dispatcher == nil {
		return
	}
	if err := e.dispatcher.Dispatch(ctx, event); err != nil {
		e.logFn("executor",
			"notification dispatch failed: %v", err)
	}
}

// cascadeCooldown returns the cooldown duration for the cascade
// guard, computed from config.
func (e *Executor) cascadeCooldown() time.Duration {
	cycles := e.cfg.Trust.CascadeCooldownCycles
	interval := e.cfg.Collector.IntervalSeconds
	d := time.Duration(cycles) *
		time.Duration(interval) * time.Second
	if d == 0 {
		d = 5 * time.Minute
	}
	return d
}

// isCascadeCooldown returns true if an action was recently
// executed for the given object identifier.
func (e *Executor) isCascadeCooldown(objID string) bool {
	t, ok := e.recentActions[objID]
	if !ok {
		return false
	}
	if time.Since(t) < e.cascadeCooldown() {
		e.logFn("executor",
			"cascade guard: skipping %q (action %v ago)",
			objID, time.Since(t),
		)
		return true
	}
	return false
}

// pruneRecentActions removes entries older than the cascade
// cooldown to prevent unbounded map growth.
func (e *Executor) pruneRecentActions() {
	maxAge := e.cascadeCooldown()
	for k, t := range e.recentActions {
		if time.Since(t) > maxAge {
			delete(e.recentActions, k)
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
		   AND acted_on_at IS NULL
		 LIMIT 1`,
		f.Category, f.ObjectIdentifier,
	).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// maxActionRetries is the maximum number of times the executor
// will retry a failed action before giving up permanently.
const maxActionRetries = 3

// exceedsMaxRetries checks if a finding has already failed more
// than maxActionRetries times, preventing infinite retry loops.
func (e *Executor) exceedsMaxRetries(
	ctx context.Context, findingID int64,
) bool {
	if e.pool == nil {
		return false
	}
	var failCount int
	err := e.pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.action_log
		 WHERE finding_id = $1 AND outcome = 'failed'`,
		findingID,
	).Scan(&failCount)
	if err != nil {
		return false // on error, allow retry
	}
	if failCount >= maxActionRetries {
		// Mark the finding as acted_on so it stops appearing.
		// If the UPDATE fails silently we would retry forever, so
		// log the failure to surface it to operators.
		if _, err := e.pool.Exec(ctx,
			`UPDATE sage.findings
			 SET acted_on_at = now()
			 WHERE id = $1 AND acted_on_at IS NULL`,
			findingID,
		); err != nil {
			e.logFn("executor",
				"failed to mark finding %d acted_on after "+
					"%d retries: %v — will retry this update next cycle",
				findingID, failCount, err)
		}
		return true
	}
	return false
}

// recordInvalidIndexCleanupFailure persists a failed cleanup
// attempt into sage.action_log so the operator sees a durable
// record in the dashboard. Best-effort: a logging failure here
// is itself just logged.
func (e *Executor) recordInvalidIndexCleanupFailure(
	ctx context.Context,
	findingID int64,
	idxName string,
	dropErr error,
) {
	if e.pool == nil {
		return
	}
	reason := fmt.Sprintf(
		"invalid index %s; DROP CONCURRENTLY failed: %v — "+
			"manual cleanup required",
		idxName, dropErr)
	_, logErr := e.pool.Exec(ctx,
		`INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, rollback_sql,
		  before_state, outcome, rollback_reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		"drop_index_failed", findingID,
		fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s", idxName),
		nil, []byte("{}"), "failed", reason,
	)
	if logErr != nil {
		e.logFn("executor",
			"failed to record invalid-index cleanup failure "+
				"for %s: %v", idxName, logErr)
	}
}

func (e *Executor) markFindingActioned(
	ctx context.Context,
	findingID int64,
	actionID int64,
) {
	tag, err := e.pool.Exec(ctx,
		`UPDATE sage.findings
		 SET acted_on_at = now(),
		     action_log_id = $1,
		     status = 'resolved',
		     resolved_at = COALESCE(resolved_at, now())
		 WHERE id = $2`,
		actionID, findingID,
	)
	if err != nil {
		e.logFn("executor",
			"failed to mark finding %d resolved after action %d: %v",
			findingID, actionID, err)
		return
	}
	if tag.RowsAffected() == 0 {
		e.logFn("executor",
			"manual action %d did not resolve finding %d: finding missing or already changed",
			actionID, findingID)
	}
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

	outcome := actionOutcome(execErr)

	actionType := categorizeAction(f.RecommendedSQL)

	var errReason *string
	if execErr != nil {
		s := execErr.Error()
		errReason = &s
	}

	var actionID int64
	err := e.pool.QueryRow(ctx,
		`INSERT INTO sage.action_log
		 (action_type, finding_id, sql_executed, rollback_sql,
		  before_state, outcome, rollback_reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id`,
		actionType, findingID, f.RecommendedSQL,
		nilIfEmpty(f.RollbackSQL), beforeJSON, outcome,
		errReason,
	).Scan(&actionID)
	if err != nil {
		e.logFn("executor",
			"failed to log action for %q: %v", f.Title, err,
		)
		return 0
	}

	// Link the finding to this action.
	// Only mark acted_on_at when the action succeeded so that failed
	// findings remain eligible for retry (lookupFindingID filters on
	// acted_on_at IS NULL).
	if outcome != "failed" {
		e.markFindingActioned(ctx, findingID, actionID)
	}

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

// actionOutcome returns "failed" when execErr is non-nil, "monitoring"
// otherwise. Successful reversible actions stay in monitoring until
// post-action verification marks them success or rolled_back.
// This determines whether acted_on_at is set on the finding — failed actions
// must leave the finding retryable.
func actionOutcome(execErr error) string {
	if execErr != nil {
		return "failed"
	}
	return "monitoring"
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
	rest := strings.TrimSpace(sql[idx+5:])
	// Skip optional "CONCURRENTLY" and "IF NOT EXISTS".
	upper = strings.ToUpper(rest)
	if strings.HasPrefix(upper, "CONCURRENTLY") {
		rest = strings.TrimSpace(rest[len("CONCURRENTLY"):])
		upper = strings.ToUpper(rest)
	}
	if strings.HasPrefix(upper, "IF NOT EXISTS") {
		rest = strings.TrimSpace(rest[len("IF NOT EXISTS"):])
	}
	// Next token is the index name.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	if strings.EqualFold(fields[0], "ON") {
		return ""
	}
	name := strings.Trim(fields[0], "\"")
	return name
}

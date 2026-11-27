package executor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/value"
)

// A crash during a rollback left the action in rolling_back for ever: the
// startup resume picks up only monitoring and interrupted, and the claim
// refuses rolling_back (executor-07). The reconciler settles a row whose
// claim is older than the DDL timeout plus a minute, when no rollback DDL
// can still be running, by probing whether the rollback took effect.
// Index replacements resume their own rollback and are excluded.

// stuckRollbackSlack is added to the DDL timeout: a rollback's statement
// is bounded by it, so a claim older than both is no longer running.
const stuckRollbackSlack = time.Minute

// stuckRollbackRowTimeout bounds one row's lock, probe and write.
const stuckRollbackRowTimeout = 30 * time.Second

const stuckRollbackFilter = `outcome = 'rolling_back' AND action_type <> 'replace_index'
	  AND COALESCE(measured_at, executed_at) < now() - make_interval(secs => $1)`

// stuckDropPattern is one DROP INDEX of one (optionally qualified) index,
// never CASCADE.
var stuckDropPattern = regexp.MustCompile(`(?i)^\s*DROP\s+INDEX\s+` +
	`(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?(` + migrationIdentifier +
	`(?:\s*\.\s*` + migrationIdentifier + `)?)\s*(?:RESTRICT\s*)?;?\s*$`)

// stuckRollbackGrace is how long a claim must be held before the
// reconciler judges it: safety.ddl_timeout_seconds plus a minute.
func (e *Executor) stuckRollbackGrace() time.Duration {
	timeout := time.Duration(config.DefaultDDLTimeoutSeconds) * time.Second
	if cfg, _, _ := e.policySnapshot(); cfg != nil && cfg.Safety.DDLTimeout() > 0 {
		timeout = cfg.Safety.DDLTimeout()
	}
	return timeout + stuckRollbackSlack
}

// settleStuckRollbacks runs the reconciler for RunCycle and logs the result.
func (e *Executor) settleStuckRollbacks(ctx context.Context) {
	n, err := e.reconcileStuckRollbacks(ctx)
	if err != nil {
		e.logFn("executor", "reconcile stuck rollbacks: %v", err)
	}
	if n > 0 {
		e.logFn("executor", "settled %d rollback(s) interrupted in rolling_back", n)
	}
}

// reconcileStuckRollbacks settles every stuck rollback and returns how
// many it changed. While this executor has work in flight (a rollback may
// be waiting for a DDL slot) it judges nothing; the next cycle does.
func (e *Executor) reconcileStuckRollbacks(ctx context.Context) (int, error) {
	if e.pool == nil || e.inflight.count() > 0 {
		return 0, nil
	}
	ids, err := e.stuckRollbackIDs(ctx)
	if err != nil {
		return 0, err
	}
	settled := 0
	var errs []error
	for _, id := range ids {
		ok, err := e.reconcileStuckRollback(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			settled++
		}
	}
	return settled, errors.Join(errs...)
}

func (e *Executor) stuckRollbackIDs(ctx context.Context) ([]int64, error) {
	rows, err := e.pool.Query(ctx, `/* pg_sage executor */ SELECT id FROM sage.action_log
		WHERE `+stuckRollbackFilter+` ORDER BY id`, e.stuckRollbackGrace().Seconds())
	if err != nil {
		return nil, fmt.Errorf("list stuck rollbacks: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read stuck rollbacks: %w", err)
	}
	return ids, nil
}

// reconcileStuckRollback settles one stuck row under a per-action advisory
// lock and a compare-and-set, so concurrent reconcilers make one
// transition. It reports whether this call changed the row.
func (e *Executor) reconcileStuckRollback(ctx context.Context, actionID int64) (bool, error) {
	if e.pool == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, stuckRollbackRowTimeout)
	defer cancel()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin reconcile of action %d: %w", actionID, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rollbackSQL, held, err := lockStuckRollback(ctx, tx, actionID, e.stuckRollbackGrace())
	if err != nil || !held {
		return false, err
	}
	done, detail, err := probeStuckRollback(ctx, e.pool, rollbackSQL)
	if err != nil {
		return false, fmt.Errorf("probe stuck rollback of action %d: %w", actionID, err)
	}
	outcome := "rollback_failed"
	reason := "interrupted during rollback: " + detail
	if rollbackSQL != "" {
		reason += "; the rollback can be retried"
	}
	if done {
		outcome, reason = "rolled_back", "interrupted during rollback; confirmed in effect: "+detail
	}
	changed, err := settleStuckRow(ctx, tx, actionID, outcome, reason)
	if err != nil || !changed {
		return false, err
	}
	countActionOutcome(ctx, e.pool, actionID, outcome)
	e.logFn("executor", "action %d was stuck in rolling_back; now %s: %s",
		actionID, outcome, reason)
	if done {
		_, _ = finalizeActionVerification(ctx, e.pool, actionID, "revert", reason)
		_, _ = value.NewPostgresRepository(e.pool).ZeroCreditOnRevert(ctx, actionID,
			"rolled_back")
	}
	return true, nil
}

// lockStuckRollback takes the action's advisory lock and re-reads the row
// under it. held is false when another reconciler holds the lock or the row
// is no longer stuck.
func lockStuckRollback(
	ctx context.Context, tx pgx.Tx, actionID int64, grace time.Duration,
) (string, bool, error) {
	var locked bool
	err := tx.QueryRow(ctx, `/* pg_sage executor */ SELECT pg_try_advisory_xact_lock(
		hashtextextended('sage.rollback:' || $1::bigint::text, 0))`, actionID).Scan(&locked)
	if err != nil {
		return "", false, fmt.Errorf("lock stuck rollback of action %d: %w", actionID, err)
	}
	if !locked {
		return "", false, nil
	}
	var rollbackSQL string
	err = tx.QueryRow(ctx, `/* pg_sage executor */ SELECT COALESCE(rollback_sql, '')
		FROM sage.action_log WHERE id = $2 AND `+stuckRollbackFilter,
		grace.Seconds(), actionID).Scan(&rollbackSQL)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read stuck rollback of action %d: %w", actionID, err)
	}
	return rollbackSQL, true, nil
}

func settleStuckRow(
	ctx context.Context, tx pgx.Tx, actionID int64, outcome, reason string,
) (bool, error) {
	tag, err := tx.Exec(ctx, `/* pg_sage executor */ UPDATE sage.action_log
		SET outcome = $1, rollback_reason = $2, measured_at = now()
		WHERE id = $3 AND outcome = 'rolling_back'`, outcome, reason, actionID)
	if err != nil {
		return false, fmt.Errorf("record %s for stuck action %d: %w", outcome, actionID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit %s for stuck action %d: %w", outcome, actionID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// probeStuckRollback reports whether an interrupted rollback took effect,
// with why. Only a positive read of the catalog confirms it; a statement
// it cannot probe, or a probe that fails, is not confirmed. The error is
// returned only when the context ended, leaving the row for a later cycle.
func probeStuckRollback(
	ctx context.Context, pool *pgxpool.Pool, rollbackSQL string,
) (bool, string, error) {
	done, detail, err := probeRollbackEffect(ctx, pool, normalizeSQLText(rollbackSQL))
	if err != nil {
		if ctx.Err() != nil {
			return false, "", err
		}
		return false, "whether it took effect could not be confirmed: " + err.Error(), nil
	}
	return done, detail, nil
}

func probeRollbackEffect(
	ctx context.Context, pool *pgxpool.Pool, sql string,
) (bool, string, error) {
	if sql == "" {
		return false, "no rollback SQL is recorded, so whether it took effect " +
			"could not be confirmed", nil
	}
	if stmt, ok := pgconf.ParseAlterSystem(sql); ok {
		return probeSystemSetting(ctx, pool, stmt)
	}
	if stmt, ok := pgconf.ParseAlterDatabase(sql); ok {
		state, note, _ := readDatabaseSetting(ctx, pool, stmt)
		return state == readbackInEffect, note, nil
	}
	if target, ok := droppedIndexTarget(sql); ok {
		return probeDroppedIndex(ctx, pool, target)
	}
	if isCreateIndexSQL(sql) {
		return probeRecreatedIndex(ctx, pool, sql)
	}
	return false, "whether it took effect could not be confirmed for this statement", nil
}

func probeSystemSetting(
	ctx context.Context, pool *pgxpool.Pool, stmt pgconf.SystemStmt,
) (bool, string, error) {
	row, err := readSettingRow(ctx, pool, stmt.Name)
	if err != nil {
		return false, "", err
	}
	state, _ := classifyReadback(row, stmt)
	if state == readbackInEffect {
		return true, fmt.Sprintf("%s is at the rollback's value (%s)", stmt.Name,
			row.Setting), nil
	}
	return false, fmt.Sprintf("%s is not at the rollback's value (%s: %s)", stmt.Name,
		state, row.Setting), nil
}

// droppedIndexTarget returns the index a single DROP INDEX names.
func droppedIndexTarget(sql string) (string, bool) {
	match := stuckDropPattern.FindStringSubmatch(sql)
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

func probeDroppedIndex(
	ctx context.Context, pool *pgxpool.Pool, target string,
) (bool, string, error) {
	var present bool
	// Resolved on the executor's pinned path, like the rollback DDL.
	if err := queryRowPinned(ctx, pool, `/* pg_sage executor */ SELECT
		pg_catalog.to_regclass($1) IS NOT NULL`, []any{target}, &present); err != nil {
		return false, "", err
	}
	if present {
		return false, "index " + target + " still exists", nil
	}
	return true, "index " + target + " no longer exists", nil
}

func probeRecreatedIndex(
	ctx context.Context, pool *pgxpool.Pool, rollbackSQL string,
) (bool, string, error) {
	state, def, err := rollbackIndexTarget(ctx, pool, rollbackSQL)
	if err != nil {
		return false, "", err
	}
	switch state {
	case rollbackTargetSame:
		return true, "the index is back with the rollback's definition", nil
	case rollbackTargetInvalid:
		return false, "an INVALID index of that name remains from the interrupted " +
			"CONCURRENTLY build (" + def + "); drop it with DROP INDEX CONCURRENTLY " +
			"before retrying", nil
	case rollbackTargetDifferent:
		return false, "an index of that name exists with a different definition (" +
			def + ")", nil
	}
	return false, "the index is not back", nil
}

package executor

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// settleConfigChange reads a config change back after it ran and records
// what it found (G-P0-1). It reports whether the action goes on to the
// outcome monitor: only a change verifiably in effect does. A change that
// needs a restart is recorded as applied_pending_restart; one that did
// not take effect is reverted and recorded as failed, so no unverified
// value lingers in postgresql.auto.conf or pg_class.
func (e *Executor) settleConfigChange(ctx context.Context, actionID int64, c *configChange) bool {
	if c == nil {
		return true
	}
	state, note, detail := e.readBackConfig(ctx, c)
	e.recordAfterState(ctx, actionID, "config_readback", detail)
	e.logFn("executor", "config: %s", note)
	switch state {
	case readbackInEffect:
		return true
	case readbackPendingRestart:
		updateActionOutcome(ctx, e.pool, actionID, "applied_pending_restart", note)
		_, _ = finalizeActionVerification(ctx, e.pool, actionID, "unverifiable",
			"pending restart: "+note)
	case readbackUnconfirmed:
		if setMonitoredOutcome(ctx, e.pool, actionID, "unverifiable", note) {
			_, _ = finalizeActionVerification(ctx, e.pool, actionID, "unverifiable", note)
		}
	default:
		e.revertIneffective(ctx, actionID, c, note)
	}
	return false
}

func (e *Executor) readbackWait() time.Duration {
	if e.settingWait > 0 {
		return e.settingWait
	}
	return reloadSettleTimeout
}

// readBackConfig returns the read-back state, a note and the detail kept
// in after_state.config_readback.
func (e *Executor) readBackConfig(
	ctx context.Context, c *configChange,
) (readbackState, string, map[string]any) {
	if c.kind == configKindGUC {
		out := applyConfigChangeWithin(ctx, e.pool, c.sql, e.cfg.CloudEnvironment,
			e.logFn, e.readbackWait())
		return out.State, out.Note, map[string]any{"state": out.State,
			"effective": out.Effective, "source": out.Source,
			"pending_restart": out.PendingRestart}
	}
	main, toast, err := captureReloptions(ctx, e.pool, c.table.Table)
	if err != nil {
		return readbackUnconfirmed, "reloptions read-back failed: " + err.Error(),
			map[string]any{"state": readbackUnconfirmed}
	}
	state, note := readbackInEffect, c.table.Table+" storage parameters in effect"
	for _, opt := range c.table.Options {
		prior := main
		if strings.HasPrefix(opt.Key, "toast.") {
			prior = toast
		}
		v, present := prior[strings.TrimPrefix(opt.Key, "toast.")]
		live := !c.table.Reset && present && reloptionValueMatches(v, opt.Value) ||
			c.table.Reset && !present
		if !live {
			state, note = readbackNotInEffect, opt.Key+" did not take effect on "+
				c.table.Table+" (pg_class.reloptions shows "+v+")"
		}
	}
	return state, note, map[string]any{"state": state, "reloptions": main,
		"toast_reloptions": toast}
}

// reloptionValueMatches compares a stored reloption with the request.
func reloptionValueMatches(stored, want string) bool {
	a, errA := strconv.ParseFloat(stored, 64)
	b, errB := strconv.ParseFloat(want, 64)
	if errA == nil && errB == nil {
		return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(a))
	}
	sa, okA := pgBool(stored)
	sb, okB := pgBool(want)
	if okA && okB {
		return sa == sb
	}
	return strings.EqualFold(stored, want)
}

// revertIneffective restores the prior state of a change that did not take
// effect and records the action as failed. The caller holds the DDL slot.
func (e *Executor) revertIneffective(
	ctx context.Context, actionID int64, c *configChange, note string,
) {
	cfg := e.rollbackMonitorConfig(nil)
	cfg.Acquire = nil
	reason := "did not take effect: " + note
	if rolled, err := executeRollbackSQL(ctx, e.pool, c.rollbackSQL, cfg); err != nil {
		reason += "; revert failed: " + err.Error()
	} else {
		reason += "; reverted (" + rolled + ")"
	}
	e.logFn("executor", "config change %q %s", c.sql, reason)
	updateActionOutcome(ctx, e.pool, actionID, "failed", reason)
	_, _ = finalizeActionVerification(ctx, e.pool, actionID, "failed", reason)
}

// settleConfigOutcome judges a config change's targeted metric at the end
// of its monitor window, after the regression check passed. It returns
// false when the action is not a config change.
func settleConfigOutcome(
	ctx context.Context, pool *pgxpool.Pool, actionID int64,
	logFn func(string, string, ...any),
) bool {
	var raw []byte
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT before_state->'config_change'
		FROM sage.action_log WHERE id = $1`, actionID).Scan(&raw)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var change struct {
		Outcome *outcomeBaseline `json:"outcome"`
	}
	ok, reason := false, "no targeted metric to verify this change"
	if err := json.Unmarshal(raw, &change); err != nil {
		reason = "config change record unreadable: " + err.Error()
	} else if change.Outcome != nil {
		ok, reason = judgeAgainstNow(ctx, pool, *change.Outcome)
	}
	verdict := map[string]any{"verified": ok, "reason": reason}
	if change.Outcome != nil {
		verdict["metric"] = change.Outcome.Metric
	}
	logFn("rollback", "config outcome for action %d: %s", actionID, reason)
	if ok {
		updateActionSuccess(ctx, pool, actionID)
	} else if setMonitoredOutcome(ctx, pool, actionID, "unverifiable", reason) {
		_, _ = finalizeActionVerification(ctx, pool, actionID, "unverifiable", reason)
	}
	if err := mergeAfterState(ctx, pool, actionID, "config_outcome", verdict); err != nil {
		logFn("rollback", "record config outcome for action %d: %v", actionID, err)
	}
	return true
}

func judgeAgainstNow(
	ctx context.Context, pool *pgxpool.Pool, b outcomeBaseline,
) (bool, string) {
	now, err := readOutcomeCounters(ctx, pool, b.Metric, b.Table)
	if err != nil {
		return false, "outcome metric unavailable: " + err.Error()
	}
	return judgeOutcome(b, now, time.Now())
}

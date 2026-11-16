package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// minResumeGrace is the least time after a monitor's window during which
// a restarted sidecar still checks the action once (a config restart).
const minResumeGrace = time.Hour

// verify.on_expired values (lab-01c).
const (
	onExpiredAsk    = "ask"
	onExpiredKeep   = "keep"
	onExpiredRevert = "revert"
)

// expirySource names the expiry's decisions in the ledger.
const expirySource = "verification_expired"

// verificationExpired reports a monitor whose window ended so long ago
// that its before-state no longer describes the workload: the window plus
// a grace of one window (at least minResumeGrace) has passed (dogfood
// lifeos-1: June monitors resumed in October rolled back healthy drops).
func verificationExpired(executedAt time.Time, window time.Duration, now time.Time) bool {
	grace := max(window, minResumeGrace)
	return now.After(executedAt.Add(window + grace))
}

// expireMonitor records an expired monitor: no regression check and no
// automatic judgement, with why. It reports whether this call recorded it
// (false when another resume already did). The action is not credited.
func expireMonitor(ctx context.Context, pool *pgxpool.Pool, actionID int64,
	executedAt time.Time, window time.Duration, logFn func(string, string, ...any)) bool {
	ended := executedAt.Add(window)
	reason := fmt.Sprintf("verification window ended %s (%s ago) while pg_sage was not "+
		"watching; comparing that before-state baseline with today's workload would not "+
		"be evidence, so no regression check and no automatic rollback",
		ended.UTC().Format(time.RFC3339), time.Since(ended).Round(time.Minute))
	logFn("rollback", "action %d: %s", actionID, reason)
	if !setMonitoredOutcome(ctx, pool, actionID, "verification_expired", reason) {
		return false
	}
	_, _ = finalizeActionVerification(ctx, pool, actionID, "unverifiable", reason)
	return true
}

// expireMonitorAndAct records the expiry, then acts as verify.on_expired
// says, so an unjudged change is never left without anyone asked: ask
// queues one "Revert action N" item, keep keeps it (recorded), revert
// rolls it back through the operator rollback and, when that is refused
// or fails, asks instead. An action with no rollback SQL is kept, since
// there is nothing to revert. Each path writes the decision ledger.
func (e *Executor) expireMonitorAndAct(ctx context.Context, actionID int64,
	rollbackSQL string, executedAt time.Time, window time.Duration) {
	if !expireMonitor(ctx, e.pool, actionID, executedAt, window, e.logFn) {
		return
	}
	if e.isPlanStepAction(ctx, actionID) {
		e.recordExpiryDecision(ctx, actionID, rollbackSQL, policy.VerdictObserveOnly,
			"verification expired; its plan step's on_expired decides (no revert item)")
		return
	}
	switch onExpired := e.onExpired(); {
	case strings.TrimSpace(rollbackSQL) == "":
		e.recordExpiryDecision(ctx, actionID, rollbackSQL, policy.VerdictObserveOnly,
			"verification expired; change kept: it has no rollback SQL to revert")
	case onExpired == onExpiredKeep:
		e.recordExpiryDecision(ctx, actionID, rollbackSQL, policy.VerdictObserveOnly,
			"verification expired; change kept (verify.on_expired=keep)")
	case onExpired == onExpiredRevert:
		e.revertExpired(ctx, actionID, rollbackSQL)
	default:
		e.askExpired(ctx, actionID, rollbackSQL,
			"verification expired; revert offered (verify.on_expired=ask)")
	}
}

// planStepActionSQL finds whether action $1 ran a plan step: its queue
// item has a plan_step: identity key, or a plan step records it.
const planStepActionSQL = `/* pg_sage executor */ SELECT
	EXISTS (SELECT 1 FROM sage.action_queue WHERE action_log_id = $1
	         AND identity_key LIKE '` + PlanStepIdentityPrefix + `%')
	OR EXISTS (SELECT 1 FROM sage.plan_step WHERE action_log_id = $1)`

// isPlanStepAction reports whether actionID ran a plan step (plan-02a),
// whose expiry the plan engine handles. A failed read is logged and
// treated as not a plan step, so lab-01c still asks a person.
func (e *Executor) isPlanStepAction(ctx context.Context, actionID int64) bool {
	var plan bool
	if err := e.pool.QueryRow(ctx, planStepActionSQL, actionID).Scan(&plan); err != nil {
		e.logFn("ERROR", "action %d: could not read whether it is a plan step (%v); "+
			"verify.on_expired applies", actionID, err)
		return false
	}
	return plan
}

// revertExpired rolls an expired action back through the operator
// rollback and records it. A rollback refused before it ran (the action
// is still expired) asks a person instead; one that ran and failed is
// recorded with its error (the action says rollback_failed).
func (e *Executor) revertExpired(ctx context.Context, actionID int64, rollbackSQL string) {
	reason := "verification expired; rolled back (verify.on_expired=revert)"
	err := e.RollbackAction(ctx, actionID, reason)
	if err == nil {
		e.recordExpiryDecision(ctx, actionID, rollbackSQL, policy.VerdictExecute, reason)
		return
	}
	if outcome, _, readErr := e.revertState(ctx, actionID); readErr == nil &&
		revertWithheld(outcome) {
		e.logFn("executor", "action %d: verify.on_expired=revert was refused (%v); a "+
			"revert item is queued instead", actionID, err)
		e.askExpired(ctx, actionID, rollbackSQL, fmt.Sprintf("verification expired; the "+
			"rollback was refused (%v), so a revert is offered (verify.on_expired=revert)",
			err))
		return
	}
	e.logFn("ERROR", "action %d: verify.on_expired=revert could not roll it back (%v); "+
		"roll it back by hand", actionID, err)
	e.recordExpiryDecision(ctx, actionID, rollbackSQL, policy.VerdictExecute,
		fmt.Sprintf("verification expired; the rollback failed: %v "+
			"(verify.on_expired=revert)", err))
}

// askExpired queues one "Revert action N" item with its decision.
func (e *Executor) askExpired(ctx context.Context, actionID int64, rollbackSQL,
	reason string) {
	id := e.recordExpiryDecision(ctx, actionID, rollbackSQL,
		policy.VerdictQueueApproval, reason)
	e.queueRevert(ctx, actionID, rollbackSQL, RollbackVerdict{Reason: reason,
		DecisionID: id})
}

func (e *Executor) onExpired() string {
	if e.cfg == nil || e.cfg.Verify.OnExpired == "" {
		return onExpiredAsk
	}
	return e.cfg.Verify.OnExpired
}

// recordExpiryDecision writes the expiry's decision; 0 when it could not
// be written (logged: the action's own record still says why).
func (e *Executor) recordExpiryDecision(ctx context.Context, actionID int64,
	rollbackSQL string, verdict policy.Verdict, reason string) int64 {
	_, risk := e.revertedActionContext(ctx, actionID, rollbackSQL)
	request := policy.ActionRequest{SQL: rollbackSQL, Feature: "rollback", Rollback: true,
		Evidence: map[string]any{"source": expirySource, "rolled_back_action_id": actionID,
			"on_expired": e.onExpired()}}
	decision := policy.Decision{Verdict: verdict, RiskTier: policy.RiskTier(risk),
		Reason: policy.Reason(reason)}
	_, id, err := e.recordStandingDecision(ctx, request, decision)
	if err != nil {
		e.logFn("ERROR", "action %d: record the expired verification's decision: %v",
			actionID, err)
		return 0
	}
	return id
}

// keptExpiredSQL marks expired actions whose revert item an operator
// rejected as kept by operator, once.
const keptExpiredSQL = `/* pg_sage executor */ UPDATE sage.action_log al
	SET rollback_reason = COALESCE(al.rollback_reason || '; ', '') ||
	    'kept by operator (revert item ' || q.id || ' rejected)'
	FROM sage.action_queue q
	WHERE q.action_type = '` + ActionTypeRevertAction + `' AND q.status = 'rejected'
	  AND q.reverts_action_id = al.id AND al.outcome = 'verification_expired'
	  AND strpos(COALESCE(al.rollback_reason, ''), 'kept by operator') = 0
	RETURNING al.id, COALESCE(al.rollback_sql, '')`

// settleKeptExpired records "kept by operator" on each expired action
// whose revert item was rejected, with its decision.
func (e *Executor) settleKeptExpired(ctx context.Context) error {
	if e.pool == nil {
		return nil
	}
	rows, err := e.pool.Query(ctx, keptExpiredSQL)
	if err != nil {
		return fmt.Errorf("record expired verifications kept by operator: %w", err)
	}
	type kept struct {
		id  int64
		sql string
	}
	settled, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (kept, error) {
		var k kept
		return k, r.Scan(&k.id, &k.sql)
	})
	if err != nil {
		return fmt.Errorf("record expired verifications kept by operator: %w", err)
	}
	for _, k := range settled {
		e.recordExpiryDecision(ctx, k.id, k.sql, policy.VerdictObserveOnly,
			"verification expired; kept by operator (revert item rejected)")
	}
	return nil
}

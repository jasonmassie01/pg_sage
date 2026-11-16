package changeplan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// legalTransitions is every step transition the engine (and its runner)
// may write; anything else is ErrIllegalTransition and changes nothing. A
// kept step is reverted only by a revert_to_checkpoint walk (plan-02c).
var legalTransitions = map[StepState][]StepState{
	StatePending: {StateStarting, StateSkipped, StateKept, StateAwaitingOperator,
		StateAwaitingApproval},
	StateStarting: {StateQueued, StateFailed},
	StateQueued:   {StateRunning, StateFailed},
	StateRunning:  {StateExecuted, StateFailed},
	StateExecuted: {StateJudging},
	// An operator step lands (judging) or expires unlanded (skipped).
	StateAwaitingOperator: {StateJudging, StateSkipped},
	StateJudging:          {StateKept, StateReverting, StateReverted, StateHalted},
	StateKept:             {StateReverting},
	StateReverting:        {StateReverted, StateFailed},
}

func legal(from, to StepState) bool {
	for _, s := range legalTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// eventKindFor is the plan event a transition to state to records (the
// plan-01b vocabulary): the start path is step_started, the run's end
// step_executed, a verdict or skip verdict, a revert step_reverted.
func eventKindFor(to StepState) EventKind {
	switch to {
	case StateStarting, StateQueued, StateRunning, StateAwaitingOperator:
		return EventStepStarted
	case StateExecuted, StateJudging, StateFailed:
		return EventStepExecuted
	case StateReverted:
		return EventStepReverted
	}
	return EventVerdict
}

// stepWrite is what a transition records besides the state, and what it
// requires of the row.
type stepWrite struct {
	reason      string
	claim       bool // take (or renew) this engine's claim
	attempt     bool // count a start
	queueID     *int64
	actionLogID *int64
	executedAt  *time.Time
	verdict     Outcome
	verdictAt   *time.Time
	landedAt    *time.Time
	// waitFromNow restarts waiting_since at now: an operator step's
	// landing deadline counts from when the operator was asked.
	waitFromNow bool
	// requireQueue and requireClaim guard the runner's queued -> running.
	requireQueue *int64
	requireClaim bool
	requireSQL   *string
	detail       map[string]any
	// kind overrides the event kind eventKindFor gives (a walk's revert
	// starting is step_reverted, not a verdict).
	kind EventKind
}

const transitionSQL = `/* pg_sage */ UPDATE sage.plan_step SET state = $4,
	state_reason = $5,
	queue_id = COALESCE($6, queue_id), action_log_id = COALESCE($7, action_log_id),
	executed_at = COALESCE($8, executed_at), verdict = COALESCE(NULLIF($9, ''), verdict),
	verdict_at = COALESCE($10, verdict_at), attempt = attempt + $11,
	claimed_by = CASE WHEN $12 THEN $13 ELSE claimed_by END,
	claimed_until = CASE WHEN $12 THEN $14::timestamptz ELSE claimed_until END,
	landed_at = COALESCE($19, landed_at),
	waiting_since = CASE WHEN $20 THEN $17::timestamptz ELSE waiting_since END
	WHERE id = $1 AND plan_id = $2 AND state = $3
	  AND ($15::bigint IS NULL OR queue_id = $15)
	  AND (NOT $16 OR (claimed_by = $13 AND claimed_until > $17::timestamptz))
	  AND ($18::text IS NULL OR proposed_sql = $18)
	RETURNING ordinal`

// transition moves step stepID of plan planID from one state to another
// in its own transaction (the plan row locked), with its event.
func (e *Engine) transition(ctx context.Context, planID, stepID int64, from,
	to StepState, w stepWrite) error {
	return e.inTx(ctx, func(tx pgx.Tx) error {
		var hash string
		err := tx.QueryRow(ctx, `/* pg_sage */ SELECT plan_hash FROM sage.plan
			WHERE id = $1 FOR UPDATE`, planID).Scan(&hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: plan %d", ErrNotFound, planID)
		}
		if err != nil {
			return err
		}
		return e.transitionTx(ctx, tx, planID, hash, stepID, from, to, w)
	})
}

// transitionTx is transition inside tx, whose caller holds the plan row.
func (e *Engine) transitionTx(ctx context.Context, tx pgx.Tx, planID int64, hash string,
	stepID int64, from, to StepState, w stepWrite) error {
	if !legal(from, to) {
		return fmt.Errorf("%w: step %d of plan %d from %s to %s", ErrIllegalTransition,
			stepID, planID, from, to)
	}
	now := e.cfg.Now()
	attempt := 0
	if w.attempt {
		attempt = 1
	}
	var ordinal int
	err := tx.QueryRow(ctx, transitionSQL, stepID, planID, string(from), string(to),
		truncate(w.reason, 1000), w.queueID, w.actionLogID, w.executedAt, string(w.verdict),
		w.verdictAt, attempt, w.claim, e.cfg.Owner, now.Add(e.cfg.Lease), w.requireQueue,
		w.requireClaim, now, w.requireSQL, w.landedAt, w.waitFromNow).Scan(&ordinal)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: step %d of plan %d is not %s", ErrStepMoved, stepID, planID,
			from)
	}
	if err != nil {
		return fmt.Errorf("move step %d of plan %d to %s: %w", stepID, planID, to, err)
	}
	detail := map[string]any{"from": string(from), "to": string(to), "ordinal": ordinal}
	for k, v := range w.detail {
		detail[k] = v
	}
	if w.reason != "" {
		detail["reason"] = truncate(w.reason, 1000)
	}
	if w.verdict != "" {
		detail["verdict"] = string(w.verdict)
	}
	kind := eventKindFor(to)
	if w.kind != "" {
		kind = w.kind
	}
	_, err = AppendEvent(ctx, tx, Event{PlanID: planID, StepID: &stepID,
		Kind: kind, PlanHash: hash, Actor: e.actor(), Detail: detail})
	return err
}

// note records why a step has not moved (a queue failure) on the step,
// without changing its state.
func (e *Engine) note(ctx context.Context, planID, stepID int64, state StepState,
	reason string) error {
	_, err := e.cfg.Pool.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step SET
		state_reason = $4 WHERE id = $1 AND plan_id = $2 AND state = $3`, stepID, planID,
		string(state), truncate(reason, 1000))
	if err != nil {
		return fmt.Errorf("record why step %d of plan %d waits: %w", stepID, planID, err)
	}
	return nil
}

// setStatus moves plan p to status with its event (kind) in tx.
func (e *Engine) setStatus(ctx context.Context, tx pgx.Tx, p Plan, status Status,
	kind EventKind, detail map[string]any) error {
	if err := setPlan(ctx, tx, p.ID, status, p.PlanHash); err != nil {
		return err
	}
	if kind == "" {
		return nil
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["from_status"] = string(p.Status)
	_, err := AppendEvent(ctx, tx, Event{PlanID: p.ID, Kind: kind, PlanHash: p.PlanHash,
		Actor: e.actor(), Detail: detail})
	return err
}

func (e *Engine) actor() string { return "plan_engine:" + truncate(e.cfg.Owner, 150) }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

package changeplan

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/notify"
)

// The engine's holds before a step starts (plan-02b): a step the plan's
// approval does not cover (a never-automatic step, one past an up_to, any
// step of a step-by-step plan; plan-03a) and that has no approval of its
// own stops with awaiting_approval; a step that would cross a plan
// cap, or start inside a peak window, waits, and past its deadline it ends
// skipped with verdict expired and its on_expired runs.

// uncountedStates are the step states whose action has not run (or was
// undone): the caps count every other state of a running plan's steps.
var uncountedStates = []string{string(StatePending), string(StateSkipped),
	string(StateReverted), string(StateAwaitingOperator), string(StateAwaitingApproval)}

// holdBeforeStart reports whether s must not start this tick (held), with
// the follow-up a hold leaves (a notification).
func (e *Engine) holdBeforeStart(ctx context.Context, tx pgx.Tx, p Plan, s Step) (followUp,
	bool, error) {
	settings, err := readSettingContexts(ctx, tx, []Step{s})
	if err != nil {
		return nil, true, err
	}
	cov, err := coverageNow(ctx, tx, p, s, settings)
	if err != nil {
		return nil, true, err
	}
	if !cov.Covered {
		next, err := e.awaitApproval(ctx, tx, p, s, cov.Reason)
		return next, true, err
	}
	wait, err := e.mustWait(ctx, tx, p, s)
	if err != nil {
		return nil, true, err
	}
	if wait != "" {
		e.cfg.LogFn("plan", "plan %d step %d waits: %s", p.ID, s.Ordinal, wait)
		return nil, true, nil
	}
	why, err := e.capWait(ctx, tx, s)
	if err != nil || why == "" {
		return nil, err != nil, err
	}
	e.cfg.LogFn("plan", "plan %d step %d waits: %s", p.ID, s.Ordinal, why)
	return nil, true, e.waitOnCap(ctx, tx, p, s, why)
}

// awaitApproval stops plan p before step s, which its approval does not
// cover: the step is awaiting_approval with why, and one notification asks
// a person.
func (e *Engine) awaitApproval(ctx context.Context, tx pgx.Tx, p Plan, s Step,
	why string) (followUp, error) {
	if p.Status == StatusApproved {
		if err := e.setStatus(ctx, tx, p, StatusRunning, "", nil); err != nil {
			return nil, err
		}
	}
	err := e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StatePending,
		StateAwaitingApproval, stepWrite{reason: why})
	if err != nil {
		return nil, err
	}
	evt := notify.Event{Type: "approval_needed", Severity: "warning",
		Subject: fmt.Sprintf("Plan %d step %d needs its own approval", p.ID, s.Ordinal),
		Body: fmt.Sprintf("Plan %d (%s) on %s stopped before step %d: approving the plan "+
			"does not cover it. Why: %s. Approve the step to go on.", p.ID,
			truncate(p.Goal, 120), e.cfg.Database, s.Ordinal, truncate(why, 500)),
		Data: map[string]any{"plan_id": p.ID, "step": s.Ordinal,
			"database": e.cfg.Database},
		DedupKey: fmt.Sprintf("plan_step_approval:%s:%d:%d", e.cfg.Database, p.ID, s.ID)}
	return func(ctx context.Context) error {
		if e.cfg.Notify == nil {
			return nil
		}
		if err := e.cfg.Notify.Dispatch(ctx, evt); err != nil {
			e.cfg.LogFn("plan", "plan %d step %d awaits approval; the notification "+
				"failed: %v", p.ID, s.Ordinal, err)
		}
		return nil
	}, nil
}

// capWait is why s must wait for a cap or a peak window ("" when it may
// start). A caps or size read that fails makes it wait (fail closed).
func (e *Engine) capWait(ctx context.Context, tx pgx.Tx, s Step) (string, error) {
	caps, err := e.cfg.Caps(ctx)
	if err != nil {
		return ReasonCapsUnavailable + ": " + err.Error(), nil
	}
	if window, open := caps.PeakWindowAt(e.cfg.Now()); open {
		return fmt.Sprintf("%s: %q is open", ReasonPeakWindow, window), nil
	}
	if _, _, ok := newIndexOf(s); !ok {
		return "", nil
	}
	others, err := countedSteps(ctx, tx, s.ID)
	if err != nil {
		return "", err
	}
	if why := indexCountWait(s, others, caps); why != "" {
		return why, nil
	}
	var names []string
	for _, o := range others {
		index, _, ok := newIndexOf(Step{Kind: KindAction, ProposedSQL: o.sql})
		if ok && index == "" {
			return fmt.Sprintf("%s: step %d of plan %d built an unnamed index",
				ReasonSizeUnavailable, o.stepID, o.planID), nil
		}
		if ok {
			names = append(names, index)
		}
	}
	built, err := e.cfg.Sizes.IndexBytes(ctx, names)
	if err != nil {
		return ReasonSizeUnavailable + ": " + err.Error(), nil
	}
	db, err := e.cfg.Sizes.DatabaseBytes(ctx)
	if err != nil {
		return ReasonSizeUnavailable + ": " + err.Error(), nil
	}
	return indexBytesWait(built, db, caps), nil
}

// countedSteps are the started action steps of every running plan (any
// plan not terminal) except step stepID, as the caps count them.
func countedSteps(ctx context.Context, tx pgx.Tx, stepID int64) ([]capStep, error) {
	rows, err := tx.Query(ctx, `/* pg_sage */ SELECT st.plan_id, st.id, st.proposed_sql
		FROM sage.plan_step st JOIN sage.plan p ON p.id = st.plan_id
		WHERE p.status IN ('approved', 'running', 'halted') AND st.kind = 'action'
		  AND st.id <> $1 AND st.state <> ALL($2) ORDER BY st.id`, stepID, uncountedStates)
	if err != nil {
		return nil, fmt.Errorf("read the running plans' steps: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (capStep, error) {
		var c capStep
		return c, r.Scan(&c.planID, &c.stepID, &c.sql)
	})
	if err != nil {
		return nil, fmt.Errorf("read the running plans' steps: %w", err)
	}
	return out, nil
}

// waitOnCap records why s waits (and since when, the first time). Past
// its deadline, counted from when it first waited, the step ends skipped
// with verdict expired; its on_expired then decides (keep and revert_step
// go on, since nothing ran; halt_ask halts the plan).
func (e *Engine) waitOnCap(ctx context.Context, tx pgx.Tx, p Plan, s Step,
	why string) error {
	now := e.cfg.Now()
	since := s.Runtime.WaitingSince
	if since != nil &&
		now.After(since.Add(time.Duration(s.DeadlineMinutes)*time.Minute)) {
		return e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StatePending, StateSkipped,
			stepWrite{reason: ReasonCapWaitExpired + ": " + why, verdict: OutcomeExpired,
				verdictAt: &now})
	}
	_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan_step SET state_reason = $3,
		waiting_since = COALESCE(waiting_since, $4) WHERE id = $1 AND plan_id = $2
		AND state = 'pending'`, s.ID, p.ID, truncate(why, 1000), now)
	if err != nil {
		return fmt.Errorf("record why step %d of plan %d waits: %w", s.Ordinal, p.ID, err)
	}
	return nil
}

// coverageNow is the approval coverage of step s of plan p (its rows
// locked by tx): the plan's live approval and the step's own.
func coverageNow(ctx context.Context, tx pgx.Tx, p Plan, s Step,
	settings SettingContexts) (StepCoverage, error) {
	live, err := liveApproval(ctx, tx, p.ID)
	if err != nil {
		return StepCoverage{}, err
	}
	own, err := liveStepApproval(ctx, tx, s.ID)
	if err != nil {
		return StepCoverage{}, err
	}
	var steps []StepApproval
	if own != nil {
		steps = []StepApproval{*own}
	}
	return Coverage(Plan{PlanHash: p.PlanHash, Steps: []Step{s}}, live, steps, settings)[0], nil
}

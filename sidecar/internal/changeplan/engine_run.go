package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// PlanStepKey is the queue identity key of step stepID of plan planID.
func PlanStepKey(planID, stepID int64) string {
	return fmt.Sprintf("%s%d:%d", executor.PlanStepIdentityPrefix, planID, stepID)
}

// parsePlanStepKey reads a PlanStepKey.
func parsePlanStepKey(key string) (planID, stepID int64, ok bool) {
	rest, found := strings.CutPrefix(key, executor.PlanStepIdentityPrefix)
	if !found {
		return 0, 0, false
	}
	planText, stepText, found := strings.Cut(rest, ":")
	if !found {
		return 0, 0, false
	}
	planID, err1 := strconv.ParseInt(planText, 10, 64)
	stepID, err2 := strconv.ParseInt(stepText, 10, 64)
	return planID, stepID, err1 == nil && err2 == nil && planID > 0 && stepID > 0
}

// runStep walks the approval path of step s (state starting): the queue
// item, its approval as approver with exactly the step's SQL, and the
// executor's RunApprovedAction, which hands it to this engine's runner. A
// queue failure leaves the step starting with the error recorded; any
// later failure fails the step with the error's text (the plan halts on
// its next tick).
func (e *Engine) runStep(ctx context.Context, p Plan, s Step, approver int) error {
	queueID, err := e.queue(ctx, p, s)
	if err != nil {
		e.cfg.LogFn("plan", "plan %d step %d: queue item: %v", p.ID, s.Ordinal, err)
		return e.note(ctx, p.ID, s.ID, StateStarting, "queue item: "+err.Error())
	}
	qid := int64(queueID)
	if err := e.transition(ctx, p.ID, s.ID, StateStarting, StateQueued,
		stepWrite{queueID: &qid, claim: true}); err != nil {
		return err
	}
	item, err := e.actions.ApproveExpecting(ctx, queueID, approver, s.ProposedSQL)
	if err != nil {
		return e.failStep(ctx, p.ID, s.ID, queueID, "approve the queue item: "+err.Error())
	}
	runCtx := context.WithValue(ctx, engineRunKey{}, engineRun{plan: p.ID, step: s.ID,
		owner: e.cfg.Owner})
	run, err := e.cfg.Executor.RunApprovedAction(runCtx, *item, approver)
	if err != nil {
		return e.failStep(ctx, p.ID, s.ID, queueID, err.Error())
	}
	at := e.cfg.Now()
	if err := e.transition(ctx, p.ID, s.ID, StateRunning, StateExecuted,
		stepWrite{actionLogID: &run.ActionLogID, executedAt: &at, claim: true,
			detail: map[string]any{"action_log_id": run.ActionLogID}}); err != nil {
		return err
	}
	verification := run.VerificationStatus
	if verification == "" {
		verification = "monitoring"
	}
	if err := e.actions.MarkExecuted(ctx, queueID, run.ActionLogID, verification); err != nil {
		e.cfg.LogFn("plan", "plan %d step %d: %v", p.ID, s.Ordinal, err)
	}
	return nil
}

// queue proposes step s's queue item: identity plan_step:<plan>:<step>,
// proposed by the plan, with the step's finding, SQL and rollback.
func (e *Engine) queue(ctx context.Context, p Plan, s Step) (int, error) {
	risk := "moderate"
	if c, ok := executor.ContractForActionType(s.ActionType); ok {
		risk = c.BaseRiskTier
	}
	finding := 0
	if s.FindingID != nil {
		finding = int(*s.FindingID)
	}
	return e.actions.ProposeWithMetadata(ctx, p.DatabaseID, finding, s.ProposedSQL,
		s.RollbackSQL, risk, store.ActionProposalMetadata{ActionType: s.ActionType,
			IdentityKey: PlanStepKey(p.ID, s.ID), ProposedBy: fmt.Sprintf("plan:%d", p.ID),
			PolicyDecision:     executor.PolicyDecisionQueueApproval,
			VerificationStatus: "not_started"})
}

// failStep fails the step from wherever the run left it (queued, or
// running when the runner took it) with why, and the queue item too.
func (e *Engine) failStep(ctx context.Context, planID, stepID int64, queueID int,
	why string) error {
	if err := e.actions.MarkAttemptFailed(ctx, queueID, why, 0); err != nil {
		e.cfg.LogFn("plan", "plan %d: record queue item %d failed: %v", planID, queueID, err)
	}
	err := e.transition(ctx, planID, stepID, StateRunning, StateFailed,
		stepWrite{reason: why})
	if errors.Is(err, ErrStepMoved) {
		err = e.transition(ctx, planID, stepID, StateQueued, StateFailed,
			stepWrite{reason: why})
	}
	return err
}

// engineRun marks the context of the engine's own RunApprovedAction call.
type engineRun struct {
	plan, step int64
	owner      string
}

type engineRunKey struct{}

// planRunner runs plan step queue items for its engine
// (executor.ApprovedActionRunner).
type planRunner struct {
	engine *Engine
	manual ManualExecutor
}

// Runner is this engine's approved-action runner: register it with the
// executor for executor.PlanStepIdentityPrefix. It runs a plan step's item
// only when this engine handed it over for that step, the step is queued
// with that item, the engine's claim is live and the item's SQL is the
// step's: approving the item from the Actions page, a card or the
// scheduled runner runs nothing (ErrNotEngineRun).
func (e *Engine) Runner(manual ManualExecutor) executor.ApprovedActionRunner {
	return &planRunner{engine: e, manual: manual}
}

func (r *planRunner) Owns(action store.QueuedAction) bool {
	_, _, ok := parsePlanStepKey(action.IdentityKey)
	return ok
}

func (r *planRunner) RunApproved(ctx context.Context, action store.QueuedAction,
	approvedBy int) (executor.ApprovedRun, error) {
	planID, stepID, _ := parsePlanStepKey(action.IdentityKey)
	run, ok := ctx.Value(engineRunKey{}).(engineRun)
	if !ok || run.plan != planID || run.step != stepID || run.owner != r.engine.cfg.Owner {
		return executor.ApprovedRun{}, fmt.Errorf("queue item %d (%s): %w", action.ID,
			action.IdentityKey, ErrNotEngineRun)
	}
	queueID := int64(action.ID)
	sql := action.ProposedSQL
	err := r.engine.transition(ctx, planID, stepID, StateQueued, StateRunning,
		stepWrite{requireQueue: &queueID, requireClaim: true, requireSQL: &sql, claim: true})
	if err != nil {
		return executor.ApprovedRun{}, fmt.Errorf("queue item %d: %w: %v", action.ID,
			ErrNotEngineRun, err)
	}
	s, err := r.engine.loadStep(ctx, planID, stepID)
	if err != nil {
		return executor.ApprovedRun{}, err
	}
	finding := 0
	if s.FindingID != nil {
		finding = int(*s.FindingID)
	}
	id, err := r.manual.ExecuteManual(ctx, finding, s.ProposedSQL, s.RollbackSQL, &approvedBy)
	return executor.ApprovedRun{ActionLogID: id}, err
}

func (e *Engine) loadStep(ctx context.Context, planID, stepID int64) (Step, error) {
	rows, err := e.cfg.Pool.Query(ctx, `/* pg_sage */ SELECT `+stepColumns+`
		FROM sage.plan_step WHERE plan_id = $1 AND id = $2`, planID, stepID)
	if err != nil {
		return Step{}, fmt.Errorf("read step %d of plan %d: %w", stepID, planID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Step{}, fmt.Errorf("read step %d of plan %d: %w", stepID, planID, err)
		}
		return Step{}, fmt.Errorf("%w: step %d of plan %d", ErrNotFound, stepID, planID)
	}
	s, err := scanStep(rows)
	if err != nil {
		return Step{}, fmt.Errorf("read step %d of plan %d: %w", stepID, planID, err)
	}
	return s, nil
}

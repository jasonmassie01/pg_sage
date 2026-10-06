package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// A replacement is judged twice over. Until its first verdict (the
// verification window, extended to the cap while evidence is thin) the
// targeted queries must improve and the old index's users must not
// regress; a regression, no gain or no verdict at the cap rolls it back
// (re-create the old index, drop the new one), as for an index create.
// After a kept verdict, the soft drop watches the old index's users over
// the drop window and re-creates only the old index if they regress.

type replaceAction string

const (
	actWait       replaceAction = "wait"
	actKeep       replaceAction = "keep"
	actRollback   replaceAction = "rollback"
	actRestoreOld replaceAction = "restore_old"
	actDone       replaceAction = "done"
)

// replaceJudgeInput is one check of a replacement.
type replaceJudgeInput struct {
	Phase                          string
	Targets, Guarded               *verify.Comparison
	HintNamesOld                   bool
	PastMin, AtCap, PastDropWindow bool
}

// replaceDecision is what a check decided and why.
type replaceDecision struct {
	Action  replaceAction
	Verdict string
	Reason  string
}

func regressed(c *verify.Comparison) bool {
	return c != nil && c.Verdict == verify.OutcomeRegressed
}

// decideReplace is the verdict rule of a replacement check.
func decideReplace(in replaceJudgeInput) replaceDecision {
	switch in.Phase {
	case replacePhaseJudging:
		return decideJudging(in)
	case replacePhaseWatch:
		return decideWatch(in)
	}
	return replaceDecision{Action: actDone}
}

func decideJudging(in replaceJudgeInput) replaceDecision {
	if regressed(in.Targets) {
		return replaceDecision{actRollback, verify.OutcomeRegressed,
			"targeted queries regressed: " + in.Targets.Reason}
	}
	if why := oldIndexMiss(in); why != "" {
		return replaceDecision{actRollback, verify.OutcomeRegressed, why}
	}
	if !in.PastMin {
		return replaceDecision{Action: actWait}
	}
	if in.Targets != nil {
		switch in.Targets.Verdict {
		case verify.OutcomeImproved:
			return replaceDecision{actKeep, verify.OutcomeImproved,
				"targeted queries improved: " + in.Targets.Reason}
		case verify.OutcomeNeutral:
			return replaceDecision{actRollback, verify.OutcomeNeutral,
				"no gain on the targeted queries: " + in.Targets.Reason}
		}
	}
	if in.AtCap {
		why := "no targeted query was measured"
		if in.Targets != nil {
			why = in.Targets.Reason
		}
		return replaceDecision{actRollback, verify.OutcomeUnverifiable,
			"no verdict by the verification cap: " + why}
	}
	return replaceDecision{Action: actWait}
}

func decideWatch(in replaceJudgeInput) replaceDecision {
	if why := oldIndexMiss(in); why != "" {
		return replaceDecision{actRestoreOld, verify.OutcomeRegressed, "soft drop missed: " + why}
	}
	if in.PastDropWindow {
		return replaceDecision{Action: actDone}
	}
	return replaceDecision{Action: actWait}
}

// oldIndexMiss is why the old index is needed ("" when it is not): its
// users regressed, or an active pg_sage hint names it.
func oldIndexMiss(in replaceJudgeInput) string {
	switch {
	case regressed(in.Guarded):
		return "queries that used the old index regressed: " + in.Guarded.Reason
	case in.HintNamesOld:
		return "an active pg_sage hint names the old index"
	}
	return ""
}

// replaceWatchSet is the replacements this process watches.
type replaceWatchSet struct {
	mu  sync.Mutex
	ids map[int64]bool
}

func (s *replaceWatchSet) claim(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[int64]bool{}
	}
	if s.ids[id] {
		return false
	}
	s.ids[id] = true
	return true
}

func (s *replaceWatchSet) release(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, id)
}

// watchIndexReplace starts the verification watch of a replacement once
// per process; the watch's phase is durable, so a restart resumes it.
func (e *Executor) watchIndexReplace(ctx context.Context, actionID int64) {
	if actionID <= 0 || !e.replaceWatch.claim(actionID) {
		return
	}
	cfg := e.rollbackMonitorConfig(e.manualRollbackAuthorizer())
	if !e.startRollbackMonitor(func() {
		defer e.replaceWatch.release(actionID)
		e.runReplaceWatch(context.WithoutCancel(ctx), actionID, cfg)
	}) {
		e.replaceWatch.release(actionID)
	}
}

func (e *Executor) runReplaceWatch(ctx context.Context, actionID int64,
	cfg RollbackMonitorConfig) {
	minW, _, _ := monitorWindows(verify.ClassIndexReplace, cfg)
	every := clampDuration(minW/8, 5*time.Minute, time.Hour)
	for {
		timer := time.NewTimer(every)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-e.shutdownCh:
			timer.Stop()
			return
		case <-timer.C:
		}
		d, err := e.checkIndexReplace(ctx, actionID, cfg, time.Now())
		if err != nil {
			e.logFn("executor", "check replacement of action %d: %v", actionID, err)
		}
		if d.Action != actWait && d.Action != actKeep {
			return
		}
	}
}

// replaceWatchState is what a check reads of the action.
type replaceWatchState struct {
	Guarded []int64 `json:"guarded_queryids"`
}

// checkIndexReplace judges a replacement at now and acts on the decision.
func (e *Executor) checkIndexReplace(ctx context.Context, actionID int64,
	cfg RollbackMonitorConfig, now time.Time) (replaceDecision, error) {
	rec, err := e.loadReplaceByAction(ctx, actionID)
	if err != nil {
		return replaceDecision{Action: actDone}, fmt.Errorf("load replacement: %w", err)
	}
	if rec.State != replaceCompleted ||
		(rec.Phase != replacePhaseJudging && rec.Phase != replacePhaseWatch) {
		return replaceDecision{Action: actDone}, nil
	}
	plan, err := loadMonitorPlan(ctx, e.pool, actionID, cfg)
	if err != nil {
		return replaceDecision{Action: actWait}, err
	}
	in, evidence, err := e.replaceInput(ctx, rec, plan, cfg, now)
	if err != nil {
		return replaceDecision{Action: actWait}, err
	}
	d := decideReplace(in)
	return d, e.actOnReplace(ctx, &rec, plan, d, in, evidence, cfg, now)
}

func (e *Executor) replaceInput(ctx context.Context, rec indexReplaceRecord,
	plan monitorPlan, cfg RollbackMonitorConfig, now time.Time) (replaceJudgeInput,
	map[string]any, error) {
	var raw []byte
	if err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(before_state,
		'{}'::jsonb) FROM sage.action_log WHERE id = $1`, rec.ActionLogID).
		Scan(&raw); err != nil {
		return replaceJudgeInput{}, nil, fmt.Errorf("read action %d: %w", rec.ActionLogID, err)
	}
	var st replaceWatchState
	if err := json.Unmarshal(raw, &st); err != nil {
		return replaceJudgeInput{}, nil, fmt.Errorf("decode action %d: %w",
			rec.ActionLogID, err)
	}
	evidence, guardedEvidence := map[string]any{}, map[string]any{}
	in := replaceJudgeInput{Phase: rec.Phase,
		Targets:        plan.judgeQueries(ctx, e.pool, cfg, now, evidence),
		PastMin:        !now.Before(plan.executedAt.Add(plan.minWindow)),
		AtCap:          !now.Before(plan.executedAt.Add(plan.capWindow)),
		PastDropWindow: !now.Before(plan.executedAt.Add(dropWindowOf(cfg)))}
	guarded := plan
	guarded.targets = st.Guarded
	in.Guarded = guarded.judgeQueries(ctx, e.pool, cfg, now, guardedEvidence)
	evidence["guarded"] = guardedEvidence
	soft := plan
	soft.softDrop, soft.index = true, bareIndexName(rec.Plan.OldIndex)
	in.HintNamesOld = soft.softDropMiss(ctx, e.pool, nil) == "hint_reference"
	return in, evidence, nil
}

func dropWindowOf(cfg RollbackMonitorConfig) time.Duration {
	w, _, _ := monitorWindows(verify.ClassIndexDrop, cfg)
	return w
}

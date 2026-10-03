package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// monitorPlan is what a post-action monitor verifies, read from the
// action's row: its class, prediction, targeted queries with their frozen
// baseline, the config change's metric, and its windows. The windows are
// sized to traffic: the first verdict needs minWindow and enough
// evidence; insufficient evidence extends the watch up to capWindow.
type monitorPlan struct {
	actionID    int64
	class       string
	executedAt  time.Time
	monitorable bool
	rollbackSQL string
	prediction  verify.Prediction
	targets     []int64
	baseline    map[int64]verify.Measurement
	config      *outcomeBaseline
	isConfig    bool
	index       string // the dropped index, for the soft-drop hint check
	softDrop    bool

	minWindow, capWindow, checkEvery time.Duration
}

// monitorWindows sizes a class's windows. A drop is watched for its
// business cycle (verify.drop_window_hours) and checked from the start
// (soft drop: re-create on the first miss); other classes give their
// first verdict after trust.rollback_window_minutes and may extend to
// verify.window_max_minutes.
func monitorWindows(class string, cfg RollbackMonitorConfig) (minW, capW, every time.Duration) {
	if class == verify.ClassIndexDrop {
		minW = cfg.DropWindow
		if minW <= 0 {
			minW = time.Duration(config.DefaultVerifyDropWindowHours) * time.Hour
		}
		return minW, minW, clampDuration(minW/8, 5*time.Minute, time.Hour)
	}
	minW = time.Duration(cfg.WindowMinutes) * time.Minute
	capW = max(time.Duration(cfg.CapMinutes)*time.Minute, minW)
	return minW, capW, clampDuration(minW/8, 5*time.Minute, time.Hour)
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	return min(max(d, lo), hi)
}

// firstCheck is when the monitor first judges the action.
func (p monitorPlan) firstCheck() time.Time {
	if p.softDrop {
		return p.executedAt.Add(p.checkEvery)
	}
	return p.executedAt.Add(p.minWindow)
}

// nextCheck is the check after one at last, never past the cap.
func (p monitorPlan) nextCheck(last time.Time) time.Time {
	end := p.executedAt.Add(p.capWindow)
	next := last.Add(p.checkEvery)
	if next.After(end) {
		return end
	}
	return next
}

// final reports whether a verdict reached at now ends the watch: a
// regression or an unverifiable action at once, a verdict with evidence
// once the minimum window passed, anything at the cap.
func (p monitorPlan) final(now time.Time, verdict string) bool {
	switch {
	case verdict == verify.OutcomeRegressed || verdict == verify.OutcomeUnverifiable:
		return true
	case !now.Before(p.executedAt.Add(p.capWindow)):
		return true
	}
	return !now.Before(p.executedAt.Add(p.minWindow)) && verdict != verify.OutcomeInsufficient
}

// monitorState is the part of before_state the monitor reads.
type monitorState struct {
	Predicted *verify.Prediction            `json:"predicted_effect"`
	Targets   []int64                       `json:"target_queryids"`
	Baseline  map[string]verify.Measurement `json:"verify_baseline"`
	Config    *struct {
		Outcome *outcomeBaseline `json:"outcome"`
	} `json:"config_change"`
	DroppedIndex string `json:"dropped_index"`
}

// loadMonitorPlan reads an action's plan.
func loadMonitorPlan(
	ctx context.Context, pool *pgxpool.Pool, actionID int64, cfg RollbackMonitorConfig,
) (monitorPlan, error) {
	p := monitorPlan{actionID: actionID}
	var raw []byte
	var sql, outcome string
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT sql_executed, executed_at,
		COALESCE(before_state, '{}'::jsonb), outcome, COALESCE(rollback_sql, '')
		FROM sage.action_log WHERE id = $1`, actionID).
		Scan(&sql, &p.executedAt, &raw, &outcome, &p.rollbackSQL)
	if err != nil {
		return p, fmt.Errorf("load monitor plan for action %d: %w", actionID, err)
	}
	var state monitorState
	if err := json.Unmarshal(raw, &state); err != nil {
		return p, fmt.Errorf("decode before_state of action %d: %w", actionID, err)
	}
	p.class = verificationClass(sql)
	p.monitorable = outcome == "monitoring" || outcome == "pending" ||
		outcome == "interrupted"
	p.minWindow, p.capWindow, p.checkEvery = monitorWindows(p.class, cfg)
	p.softDrop = p.class == verify.ClassIndexDrop
	p.applyState(state, sql)
	return p, nil
}

func (p *monitorPlan) applyState(state monitorState, sql string) {
	p.isConfig = state.Config != nil
	if p.isConfig {
		p.config = state.Config.Outcome
	}
	switch {
	case state.Predicted != nil:
		p.prediction = *state.Predicted
	case p.config != nil:
		p.prediction = configPrediction(p.config.Metric)
	case p.isConfig:
		p.prediction = verify.NoPrediction(p.class, "no targeted metric to verify this change")
	default:
		p.prediction = verify.NoPrediction(p.class,
			"the action was recorded without a predicted effect")
	}
	p.prediction.Class = p.class
	p.targets = state.Targets
	if len(p.targets) == 0 {
		p.targets = p.prediction.TargetQueryIDs
	}
	if len(state.Baseline) > 0 {
		p.baseline = make(map[int64]verify.Measurement, len(state.Baseline))
		for key, m := range state.Baseline {
			if id, err := strconv.ParseInt(key, 10, 64); err == nil {
				p.baseline[id] = m
			}
		}
	}
	p.index = state.DroppedIndex
	if p.index == "" && p.softDrop {
		p.index = bareIndexName(firstObjectAfter(sql, "DROP INDEX", "CONCURRENTLY", "IF",
			"EXISTS"))
	}
}

package rca

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// selfActionLookback and rollbackLookback bound the action_log reads
// used for self-action correlation.
const (
	selfActionLookback = 30 * time.Minute
	rollbackLookback   = 30 * 24 * time.Hour
)

// cyclePlan carries the lock-free phase of one analysis cycle: what was
// detected under e.mu, and what must happen before results are merged.
type cyclePlan struct {
	firedIDs    map[string]bool
	incidents   []Incident
	tier2       *tier2Request
	actionStore ActionQuerier
	correlator  *SelfActionCorrelator
}

// sageActions is the self-action evidence fetched outside the lock.
type sageActions struct {
	recent    []SageAction
	rollbacks []SageAction
	ok        bool
}

// prepareCycle runs detection and Tier 1 under e.mu. It performs no I/O
// except draining the in-memory log source.
func (e *Engine) prepareCycle(
	current, previous *collector.Snapshot,
	cfg *config.Config,
	lockChainFindings []analyzer.Finding,
) cyclePlan {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.cycleCount++
	if e.gracePeriodLeft > 0 {
		e.gracePeriodLeft--
	}

	signals := e.detectSignals(current, previous, cfg, lockChainFindings)
	if e.logSource != nil {
		signals = append(signals, e.filterLogSignals(e.logSource.Drain())...)
	}

	firedIDs := make(map[string]bool, len(signals))
	for _, s := range signals {
		firedIDs[s.ID] = true
	}

	incidents := e.runDecisionTrees(signals, current, previous, cfg)
	incidents = append(incidents, e.runLogDecisionTrees(signals)...)
	for i := range incidents {
		incidents[i].DatabaseName = e.databaseName
	}

	plan := cyclePlan{
		firedIDs:    firedIDs,
		incidents:   incidents,
		actionStore: e.actionStore,
		correlator:  e.correlator,
	}
	req, reobserved := e.planTier2(signals, incidents)
	plan.tier2 = req
	plan.incidents = append(plan.incidents, reobserved...)
	return plan
}

// fetchSageActions reads recent pg_sage actions and rollback history
// outside the engine lock, bounded by the caller context.
func (e *Engine) fetchSageActions(
	ctx context.Context, plan cyclePlan,
) sageActions {
	if plan.actionStore == nil || plan.correlator == nil {
		return sageActions{}
	}
	recent, err := plan.actionStore.RecentSageActions(ctx, selfActionLookback)
	if err != nil {
		e.logFn("warn", "rca: failed to query recent actions: %v", err)
		return sageActions{}
	}
	rollbacks, err := plan.actionStore.RollbackHistory(ctx, rollbackLookback)
	if err != nil {
		e.logFn("warn", "rca: failed to query rollback history: %v", err)
		return sageActions{}
	}
	return sageActions{recent: recent, rollbacks: rollbacks, ok: true}
}

// commitCycle merges the cycle's incidents into engine state under e.mu.
func (e *Engine) commitCycle(plan cyclePlan, actions sageActions) []Incident {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i := range plan.incidents {
		e.dedup(&plan.incidents[i])
	}
	if actions.ok && len(actions.recent) > 0 {
		e.applySelfActionCorrelation(plan, actions)
	}
	e.autoResolve(e.consumeFastFired(plan.firedIDs))
	e.escalate()
	e.trimResolvedOverflow()
	return e.activeIncidents()
}

// applySelfActionCorrelation correlates this cycle's incidents with
// recent pg_sage actions. The action store is scoped to this engine's
// database, so actions without a database are attributed to it; an
// engine without a database identity never matches (R05).
func (e *Engine) applySelfActionCorrelation(
	plan cyclePlan, actions sageActions,
) {
	recent := stampActionDatabase(actions.recent, e.databaseName)
	_, selfCaused, manualReview := plan.correlator.Correlate(
		plan.incidents, recent, actions.rollbacks)

	for i := range selfCaused {
		e.logFn("warn",
			"rca: self-caused incident: %s", selfCaused[i].RootCause)
		e.dedup(&selfCaused[i])
	}
	for i := range manualReview {
		e.logFn("warn",
			"rca: manual review required: %s", manualReview[i].RootCause)
		e.dedup(&manualReview[i])
	}
}

func stampActionDatabase(actions []SageAction, db string) []SageAction {
	out := make([]SageAction, len(actions))
	copy(out, actions)
	for i := range out {
		if out[i].Database == "" {
			out[i].Database = db
		}
	}
	return out
}

// consumeFastFired adds the signals the fast path saw since the last
// analyzer cycle to this cycle's fired set, then clears them. Caller
// holds e.mu.
func (e *Engine) consumeFastFired(fired map[string]bool) map[string]bool {
	if len(e.fastFired) == 0 {
		return fired
	}
	merged := make(map[string]bool, len(fired)+len(e.fastFired))
	for id := range fired {
		merged[id] = true
	}
	for id := range e.fastFired {
		merged[id] = true
	}
	e.fastFired = make(map[string]bool)
	return merged
}

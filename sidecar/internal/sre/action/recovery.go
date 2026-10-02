package action

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Recovery verification (Codex §9, CHECK-22): the incident's predicate,
// re-probed over fresh samples after the action, independent of the
// action's own success. A sample observed before the action, truncated
// or without usable telemetry never counts; a database where nothing
// makes progress any more (traffic disappeared) is never certified.

// backlogMinWaiting matches the causal graph's blocked-backlog threshold:
// the connection family recovers only below it.
const backlogMinWaiting = 3

// summarizeSample reads one recovery_sample result against the target
// and the baseline waiters.
func summarizeSample(res probes.Result, rows []probes.RecoveryRow, target BackendTarget,
	base Baseline, executedAt time.Time) RecoverySample {
	s := RecoverySample{ObservedAt: res.ObservedAt, Status: string(res.Status),
		Reason: res.Reason}
	switch {
	case !res.Status.Usable():
		return s
	case res.Truncated:
		s.Reason = "truncated: more sessions than a sample holds"
		return s
	case !res.ObservedAt.After(executedAt):
		s.Reason = "observed before the action"
		return s
	}
	s.Usable = true
	waiters := map[int32]time.Time{}
	for _, w := range base.Waiters {
		waiters[w.PID] = w.BackendStart
	}
	for _, r := range rows {
		countRow(&s, r, target, waiters)
	}
	if !s.TargetPresent {
		s.TargetBlocking = 0
	}
	return s
}

func countRow(s *RecoverySample, r probes.RecoveryRow, target BackendTarget,
	waiters map[int32]time.Time) {
	isTarget := r.IsTarget || (r.PID == target.PID && r.BackendStart.Equal(target.BackendStart))
	if isTarget {
		s.TargetPresent = true
	}
	if r.BlockedByTarget {
		s.TargetBlocking++
	}
	if r.Waiting {
		s.Waiting++
	}
	if r.State == "active" && !r.Waiting && !isTarget {
		s.ActiveNotWaiting++
	}
	if start, ok := waiters[r.PID]; ok && start.Equal(r.BackendStart) {
		s.WaitersPresent++
		if !r.Waiting {
			s.WaitersProgressed++
		}
	}
}

// evaluateRecovery decides a recovery record: observing until enough
// fresh samples (or the deadline), then recovered, not_recovered or
// inconclusive with the reason.
func evaluateRecovery(family string, base Baseline, rec RecoveryRecord, cfg ActionConfig,
	now time.Time) (RecoveryState, string) {
	var fresh []RecoverySample
	for _, s := range rec.Samples {
		if s.Usable && s.ObservedAt.After(rec.StartedAt) {
			fresh = append(fresh, s)
		}
	}
	if len(fresh) < cfg.RecoverySamples {
		if !rec.Deadline.IsZero() && !now.Before(rec.Deadline) {
			return RecoveryInconclusive, fmt.Sprintf("only %d of %d fresh samples were "+
				"usable before the deadline (stale or unavailable telemetry)", len(fresh),
				cfg.RecoverySamples)
		}
		return RecoveryObserving, ""
	}
	return judgeRecovery(family, base, fresh)
}

func judgeRecovery(family string, base Baseline, fresh []RecoverySample) (RecoveryState,
	string) {
	last := fresh[len(fresh)-1]
	if last.TargetBlocking > 0 {
		return RecoveryNotRecovered, fmt.Sprintf("the target still blocks %d sessions "+
			"in the latest sample", last.TargetBlocking)
	}
	for _, s := range fresh {
		if s.TargetBlocking > 0 {
			return RecoveryNotRecovered, "the target's wait edges did not stay clear"
		}
	}
	improved := last.Waiting < base.Waiting
	if family == "connection_pressure" {
		improved = improved && last.Waiting < backlogMinWaiting
	}
	if !improved {
		return RecoveryNotRecovered, fmt.Sprintf("lock waits did not fall (%d before, "+
			"%d in the latest sample)", base.Waiting, last.Waiting)
	}
	for _, s := range fresh {
		if s.WaitersProgressed > 0 || s.ActiveNotWaiting > 0 {
			return RecoveryRecovered, fmt.Sprintf("the target's wait edges cleared and "+
				"lock waits fell from %d to %d over %d fresh samples, with sessions "+
				"still making progress", base.Waiting, last.Waiting, len(fresh))
		}
	}
	return RecoveryInconclusive, "the wait edges cleared but no session made progress: " +
		"traffic disappeared, so recovery cannot be certified"
}

// queueVerification maps a recovery verdict to the approval item's
// verification status and the executor's verification verdict.
func queueVerification(state RecoveryState) (queue, executor string) {
	switch state {
	case RecoveryRecovered:
		return "verified", "success"
	case RecoveryNotRecovered:
		return "failed", "failed"
	default:
		return "inconclusive", "unverifiable"
	}
}

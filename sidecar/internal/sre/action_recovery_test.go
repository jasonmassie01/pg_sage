package sre

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Recovery verification (Codex §9, CHECK-22): the incident's predicate,
// re-probed over fresh samples after the action. The blocking family
// recovers when the target's wait edges clear, lock waits fall and some
// session still makes progress; a sample observed before the action or
// without usable telemetry never counts, and an empty database (traffic
// disappeared) is never certified as recovered.

var recoveryExecuted = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

func recoveryConfig() ActionConfig {
	c := DefaultActionConfig()
	c.RecoveryInterval, c.RecoverySamples = 40*time.Second, 3
	c.RecoveryDeadline = 30 * time.Minute
	return c
}

func goodSample(k int) RecoverySample {
	return RecoverySample{ObservedAt: recoveryExecuted.Add(time.Duration(k) * 40 *
		time.Second), Usable: true, TargetPresent: true, TargetBlocking: 0, Waiting: 0,
		ActiveNotWaiting: 3, WaitersPresent: 2, WaitersProgressed: 2}
}

func recordOf(samples ...RecoverySample) RecoveryRecord {
	return RecoveryRecord{State: RecoveryObserving, Attribution: AttributionSage,
		StartedAt: recoveryExecuted, Deadline: recoveryExecuted.Add(30 * time.Minute),
		Samples: samples}
}

var lockBaseline = Baseline{Waiting: 2, Waiters: []Waiter{{PID: 20}, {PID: 30}}}

func evalLock(rec RecoveryRecord, now time.Time) (RecoveryState, string) {
	return evaluateRecovery("lock_blocking", lockBaseline, rec, recoveryConfig(), now)
}

func TestRecoveryRecoversOnThreeFreshSamples(t *testing.T) {
	state, why := evalLock(recordOf(goodSample(1), goodSample(2), goodSample(3)),
		recoveryExecuted.Add(2*time.Minute))
	if state != RecoveryRecovered || why == "" {
		t.Fatalf("three good samples = %s (%s), want recovered", state, why)
	}
}

func TestRecoveryWaitsForEnoughSamples(t *testing.T) {
	two := recordOf(goodSample(1), goodSample(2))
	if state, _ := evalLock(two, recoveryExecuted.Add(90*time.Second)); state !=
		RecoveryObserving {
		t.Fatalf("two samples before the deadline = %s, want observing", state)
	}
	state, why := evalLock(two, recoveryExecuted.Add(31*time.Minute))
	if state != RecoveryInconclusive || !strings.Contains(why, "2 of 3") {
		t.Fatalf("two samples at the deadline = %s (%s), want inconclusive", state, why)
	}
}

func TestRecoveryIgnoresStaleAndUnusableSamples(t *testing.T) {
	stale := goodSample(1)
	stale.ObservedAt = recoveryExecuted.Add(-time.Second)
	failed := RecoverySample{ObservedAt: recoveryExecuted.Add(time.Minute), Usable: false,
		Status: "no_privilege", Reason: "permission_denied"}
	rec := recordOf(stale, failed, goodSample(2), goodSample(3))
	if state, _ := evalLock(rec, recoveryExecuted.Add(3*time.Minute)); state !=
		RecoveryObserving {
		t.Fatalf("stale + failed + 2 good = %s, want observing", state)
	}
	state, why := evalLock(rec, recoveryExecuted.Add(31*time.Minute))
	if state != RecoveryInconclusive || !strings.Contains(why, "telemetry") {
		t.Fatalf("at the deadline = %s (%s), want inconclusive on telemetry", state, why)
	}
}

func TestRecoveryNeverCertifiesWhenTrafficDisappeared(t *testing.T) {
	quiet := func(k int) RecoverySample {
		s := goodSample(k)
		s.ActiveNotWaiting, s.WaitersPresent, s.WaitersProgressed = 0, 0, 0
		return s
	}
	state, why := evalLock(recordOf(quiet(1), quiet(2), quiet(3)),
		recoveryExecuted.Add(2*time.Minute))
	if state != RecoveryInconclusive || !strings.Contains(why, "traffic") {
		t.Fatalf("no demand = %s (%s), want inconclusive (traffic disappeared)", state, why)
	}
	oneProgressed := quiet(3)
	oneProgressed.WaitersPresent, oneProgressed.WaitersProgressed = 1, 1
	if state, _ := evalLock(recordOf(quiet(1), quiet(2), oneProgressed),
		recoveryExecuted.Add(2*time.Minute)); state != RecoveryRecovered {
		t.Fatalf("an original waiter progressed = %s, want recovered", state)
	}
}

func TestRecoveryNotRecoveredWhileTheTargetStillBlocks(t *testing.T) {
	blocking := goodSample(3)
	blocking.TargetBlocking, blocking.Waiting = 2, 2
	state, why := evalLock(recordOf(goodSample(1), goodSample(2), blocking),
		recoveryExecuted.Add(2*time.Minute))
	if state != RecoveryNotRecovered || !strings.Contains(why, "still blocks") {
		t.Fatalf("target blocks in the last sample = %s (%s)", state, why)
	}
}

func TestRecoveryPressureMustFall(t *testing.T) {
	same := func(k, waiting int) RecoverySample {
		s := goodSample(k)
		s.Waiting = waiting
		return s
	}
	state, why := evalLock(recordOf(same(1, 2), same(2, 2), same(3, 2)),
		recoveryExecuted.Add(2*time.Minute))
	if state != RecoveryNotRecovered || !strings.Contains(why, "lock waits") {
		t.Fatalf("waits equal to the baseline = %s (%s), want not_recovered", state, why)
	}
	if state, _ := evalLock(recordOf(same(1, 1), same(2, 1), same(3, 1)),
		recoveryExecuted.Add(2*time.Minute)); state != RecoveryRecovered {
		t.Fatalf("waits one below the baseline = %s, want recovered", state)
	}
}

func TestRecoveryConnectionBacklogMustDrain(t *testing.T) {
	base := Baseline{Waiting: 12}
	waiting := func(k, n int) RecoverySample {
		s := goodSample(k)
		s.Waiting = n
		return s
	}
	cfg := recoveryConfig()
	now := recoveryExecuted.Add(2 * time.Minute)
	state, _ := evaluateRecovery("connection_pressure", base,
		recordOf(waiting(1, 3), waiting(2, 3), waiting(3, 3)), cfg, now)
	if state != RecoveryNotRecovered {
		t.Fatalf("3 sessions still waiting = %s, want not_recovered (backlog)", state)
	}
	state, _ = evaluateRecovery("connection_pressure", base,
		recordOf(waiting(1, 2), waiting(2, 2), waiting(3, 2)), cfg, now)
	if state != RecoveryRecovered {
		t.Fatalf("2 sessions waiting = %s, want recovered", state)
	}
}

func TestSummarizeSampleCountsTheIncidentPredicate(t *testing.T) {
	start := recoveryExecuted.Add(-time.Hour)
	target := BackendTarget{PID: 5151, BackendStart: start}
	base := Baseline{Waiting: 3, Waiters: []Waiter{{PID: 20, BackendStart: start},
		{PID: 30, BackendStart: start}, {PID: 40, BackendStart: start}}}
	res := probes.Result{ProbeID: probes.RecoverySample, Status: probes.StatusOK,
		ObservedAt: recoveryExecuted.Add(time.Minute)}
	rs := []probes.RecoveryRow{
		{PID: 5151, BackendStart: start, State: "idle", IsTarget: true},
		{PID: 20, BackendStart: start, State: "active"},
		{PID: 30, BackendStart: start, State: "active", Waiting: true},
		// pid 40 was reused by a new session: not the original waiter.
		{PID: 40, BackendStart: start.Add(time.Minute), State: "active"},
		{PID: 50, BackendStart: start, State: "active", Waiting: true, BlockedByTarget: true},
	}
	s := summarizeSample(res, rs, target, base, recoveryExecuted)
	if !s.Usable || !s.TargetPresent || s.TargetBlocking != 1 || s.Waiting != 2 ||
		s.ActiveNotWaiting != 2 || s.WaitersPresent != 2 || s.WaitersProgressed != 1 {
		t.Fatalf("sample = %+v", s)
	}
	gone := summarizeSample(res, []probes.RecoveryRow{{PID: 50, BlockedByTarget: true}},
		target, base, recoveryExecuted)
	if gone.TargetPresent || gone.TargetBlocking != 0 {
		t.Fatalf("absent target counted as blocking: %+v", gone)
	}
	res.Truncated = true
	if s := summarizeSample(res, rs, target, base, recoveryExecuted); s.Usable {
		t.Fatal("a truncated sample is usable")
	}
	res.Truncated, res.Status = false, probes.StatusError
	if s := summarizeSample(res, nil, target, base, recoveryExecuted); s.Usable ||
		s.Status != "error" {
		t.Fatalf("an error sample = %+v", s)
	}
	res.Status, res.ObservedAt = probes.StatusOK, recoveryExecuted.Add(-time.Second)
	if s := summarizeSample(res, rs, target, base, recoveryExecuted); s.Usable {
		t.Fatal("a sample observed before the action is usable")
	}
}

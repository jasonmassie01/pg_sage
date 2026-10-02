package policy

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Coordinator decision 2026-10-02: an action the gate authorizes as a
// mandatory deadline override (a critical XID or disk deadline the
// standing policy lets override) is not restricted by the ledger level or
// by its downgrade signals. It stays subject to the emergency stop, the
// rest of the gate and the deadline-override rules, and the ledger
// records it.

type recordingLimiter struct {
	fakeLimiter
	mu       sync.Mutex
	recorded []Decision
}

func (r *recordingLimiter) RecordDeadlineOverride(_ context.Context, _ ActionRequest,
	d Decision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, d)
}

var _ AutonomyDeadlineRecorder = (*recordingLimiter)(nil)

func redWraparound(f *autonomyFixture, tier RiskTier) ActionRequest {
	req := familyRequest(tier, RollbackNoRollbackNeeded)
	req.IncidentFamily = "wraparound_runway"
	req.Deadline = &DeadlineContext{Kind: DeadlineXID, Urgency: UrgencyCritical,
		HardAt: f.now.Add(6 * time.Hour)}
	return req
}

func deadlineGate(f *autonomyFixture, lim AutonomyLimiter) Gate {
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return f.runtime, nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy:      func(context.Context, ActionRequest) (Document, error) { return f.doc, nil },
		Usage: func(context.Context, ActionRequest) (LimitUsage, error) {
			return LimitUsage{}, nil
		},
		RecordDecision: func(_ context.Context, _ ActionRequest, d Decision) (string, error) {
			f.records = append(f.records, d)
			return "decision-1", nil
		},
		Now:      func() time.Time { return f.now },
		Autonomy: lim,
	})
}

func TestMandatoryDeadlineRunsAtL1AndWhileDowngraded(t *testing.T) {
	for name, limit := range map[string]AutonomyLimit{
		"L1": {Level: 1, Granted: 1},
		"L0": {Level: 0, Granted: 0},
		"budget fast burn": {Level: 1, Granted: 3, Downgraded: true,
			Reasons: []string{"error_budget_fast_burn"}},
		"failover": {Level: 1, Granted: 3, Downgraded: true,
			Reasons: []string{"ha_role_not_primary"}},
	} {
		f := newAutonomyFixture(0)
		lim := &recordingLimiter{fakeLimiter: fakeLimiter{limit: limit}}
		d := deadlineGate(f, lim).Authorize(context.Background(), redWraparound(f, RiskSafe))
		if d.Verdict != VerdictExecute {
			t.Fatalf("%s: red wraparound freeze = %s/%s (%s)", name, d.Verdict, d.Reason,
				d.Detail)
		}
		if len(lim.recorded) != 1 || lim.recorded[0].Verdict != VerdictExecute {
			t.Fatalf("%s: ledger records = %+v", name, lim.recorded)
		}
	}
}

func TestMandatoryDeadlineOutsideTheWindowKeepsTheOverride(t *testing.T) {
	f := newAutonomyFixture(1)
	f.doc.MaintenanceWindows = []string{"weekdays 01:00-05:00"} // fixture: Sunday 02:00
	lim := &recordingLimiter{fakeLimiter: fakeLimiter{limit: AutonomyLimit{Level: 1,
		Granted: 1}}}
	d := deadlineGate(f, lim).Authorize(context.Background(), redWraparound(f, RiskModerate))
	assertDecision(t, d, VerdictExecute, ReasonDeadlineOverride)
	if len(lim.recorded) != 1 {
		t.Fatalf("ledger records = %d", len(lim.recorded))
	}
}

func TestMandatoryDeadlineIsStoppedByTheEmergencyStop(t *testing.T) {
	f := newAutonomyFixture(3)
	f.runtime.EmergencyStop = true
	lim := &recordingLimiter{fakeLimiter: fakeLimiter{limit: AutonomyLimit{Level: 3,
		Granted: 3}}}
	d := deadlineGate(f, lim).Authorize(context.Background(), redWraparound(f, RiskSafe))
	assertDecision(t, d, VerdictBlocked, ReasonEmergencyStop)
	if len(lim.recorded) != 0 {
		t.Fatalf("a stopped action was recorded as an override: %+v", lim.recorded)
	}
}

// The rest of the gate still applies: approval mode queues the deadline
// action for approval (the ledger does not turn it into observe-only).
func TestMandatoryDeadlineKeepsTheOperatorBound(t *testing.T) {
	f := newAutonomyFixture(1)
	f.runtime.ExecutionMode = ExecutionApproval
	lim := &recordingLimiter{fakeLimiter: fakeLimiter{limit: AutonomyLimit{Level: 1,
		Granted: 1}}}
	d := deadlineGate(f, lim).Authorize(context.Background(), redWraparound(f, RiskSafe))
	assertDecision(t, d, VerdictQueueApproval, ReasonApprovalRequired)
	if len(lim.recorded) != 0 {
		t.Fatalf("an unexecuted action was recorded: %+v", lim.recorded)
	}
}

// Only a mandatory deadline bypasses the ledger: a non-critical deadline,
// or one the standing policy does not let override, is restricted.
func TestNonMandatoryDeadlinesAreRestricted(t *testing.T) {
	cases := map[string]func(*autonomyFixture, *ActionRequest){
		"not critical": func(_ *autonomyFixture, r *ActionRequest) {
			r.Deadline.Urgency = Urgency("high")
		},
		"override disabled": func(f *autonomyFixture, _ *ActionRequest) {
			f.doc.DeadlineOverrides = map[DeadlineKind]bool{DeadlineXID: false}
		},
		"deadline passed": func(f *autonomyFixture, r *ActionRequest) {
			r.Deadline.HardAt = f.now.Add(-time.Minute)
		},
		"no deadline": func(_ *autonomyFixture, r *ActionRequest) { r.Deadline = nil },
	}
	for name, mutate := range cases {
		f := newAutonomyFixture(1)
		req := redWraparound(f, RiskSafe)
		mutate(f, &req)
		lim := &recordingLimiter{fakeLimiter: fakeLimiter{limit: AutonomyLimit{Level: 1,
			Granted: 1}}}
		d := deadlineGate(f, lim).Authorize(context.Background(), req)
		if d.Verdict != VerdictObserveOnly || d.Reason != ReasonAutonomyLevel {
			t.Errorf("%s: %s/%s, want observe_only autonomy_level", name, d.Verdict, d.Reason)
		}
		if len(lim.recorded) != 0 {
			t.Errorf("%s: recorded %+v", name, lim.recorded)
		}
	}
}

// A limiter that cannot record still lets the mandatory action through.
func TestMandatoryDeadlineWithoutARecorder(t *testing.T) {
	f := newAutonomyFixture(1)
	lim := &fakeLimiter{limit: AutonomyLimit{Level: 1, Granted: 1}}
	d := deadlineGate(f, lim).Authorize(context.Background(), redWraparound(f, RiskSafe))
	if d.Verdict != VerdictExecute {
		t.Fatalf("decision = %s/%s", d.Verdict, d.Reason)
	}
}

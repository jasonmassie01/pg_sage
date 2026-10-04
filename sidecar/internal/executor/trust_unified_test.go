package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Roadmap 1.2: the executor's gating reads the unified ledger for every
// self-initiated class (not only custodian incident families), hands the
// ledger the trust ramp as its promotion floor, records the trust family
// and class of each decision, and never lets the ledger withhold a
// rollback of pg_sage's own change.

// scopedCountingLimiter governs every request and answers one level.
type scopedCountingLimiter struct {
	mu    sync.Mutex
	limit policy.AutonomyLimit
	calls int
}

func (l *scopedCountingLimiter) Limit(context.Context, policy.ActionRequest) (
	policy.AutonomyLimit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.limit, nil
}

func (l *scopedCountingLimiter) Governs(policy.ActionRequest) bool { return true }

func (l *scopedCountingLimiter) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func windowedAutonomousConfig() *config.Config {
	cfg := autonomousConfig()
	cfg.Trust.MaintenanceWindow = "always"
	return cfg
}

func scopedExecutor(level int) (*Executor, *scopedCountingLimiter) {
	limiter := &scopedCountingLimiter{limit: policy.AutonomyLimit{Level: level,
		Granted: level}}
	exec := New(nil, windowedAutonomousConfig(), time.Now().Add(-60*24*time.Hour),
		noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	exec.SetExecutionMode("auto")
	exec.WithAutonomy(limiter)
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	return exec, limiter
}

func gucFinding() analyzer.Finding {
	return analyzer.Finding{Category: "guc_tuning", ObjectIdentifier: "work_mem",
		Title: "work_mem too small", RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"}
}

// A self-initiated finding at L1 is a manual script even with autonomous
// trust and the ramp long elapsed: the ledger decides, not the clock.
func TestFindingPolicyReadsTheLedger(t *testing.T) {
	exec, limiter := scopedExecutor(1)
	got := exec.evaluateFindingPolicy(context.Background(), gucFinding(), false)
	if got.Decision != PolicyDecisionObserveOnly ||
		got.BlockedReason != string(policy.ReasonAutonomyLevel) || limiter.count() != 1 {
		t.Fatalf("decision = %+v after %d ledger calls", got, limiter.count())
	}
	limiter.limit = policy.AutonomyLimit{Level: 2, Granted: 2}
	got = exec.evaluateFindingPolicy(context.Background(), gucFinding(), false)
	if got.Decision != PolicyDecisionQueueApproval ||
		got.BlockedReason != string(policy.ReasonAutonomyHandoff) {
		t.Fatalf("L2 decision = %+v", got)
	}
}

// The rollback of a governed action is authorized without the ledger,
// even when the class of the rollback statement sits at L0.
func TestStandingRollbackAuthorizerBypassesTheLedger(t *testing.T) {
	exec, limiter := scopedExecutor(0)
	authorize := exec.standingRollbackAuthorizer(gucFinding())
	if !authorize(context.Background(), "ALTER SYSTEM SET work_mem = '4MB'") {
		t.Fatal("the ledger withheld a rollback")
	}
	if limiter.count() != 0 {
		t.Fatalf("the ledger was consulted %d times for a rollback", limiter.count())
	}
}

func TestCreatedIndexRevertBypassesTheLedger(t *testing.T) {
	exec, limiter := scopedExecutor(0)
	if !exec.authorizeCreatedIndexRevert(context.Background(),
		"DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_a", "public.idx_orders_a") {
		t.Fatal("the ledger withheld the revert of a created index")
	}
	if limiter.count() != 0 {
		t.Fatalf("the ledger was consulted %d times for a revert", limiter.count())
	}
}

func TestDecisionEvidenceNamesTheTrustFamily(t *testing.T) {
	req := findingRequest(gucFinding(), false)
	in := ledgerInput(nil, 1, req, policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonAutonomyL3, RiskTier: policy.RiskModerate})
	if in.Evidence["trust_family"] != "tuning" || in.Evidence["autonomy_class"] != "config_guc" {
		t.Fatalf("decision evidence = %#v", in.Evidence)
	}
	if _, ok := in.Evidence["incident_family"]; ok {
		t.Fatal("a self-initiated decision names an incident family")
	}
	plain := ledgerInput(nil, 1, policy.ActionRequest{Feature: "schema_change",
		Contract: &policy.ActionContract{ActionType: "alter_table"}},
		policy.Decision{Verdict: policy.VerdictBlocked})
	if _, ok := plain.Evidence["trust_family"]; ok {
		t.Fatalf("an ungoverned class names a trust family: %#v", plain.Evidence)
	}
}

func TestOperatorBoundCarriesTheRamp(t *testing.T) {
	cfg := windowedAutonomousConfig()
	cfg.Trust.RampSafeHours, cfg.Trust.RampModerateHours = 2, 5
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	bound := New(nil, cfg, start, noopExecLog).OperatorBound()
	if !bound.RampStart.Equal(start) || bound.SafeRampAge != 2*time.Hour ||
		bound.ModerateRampAge != 5*time.Hour || bound.TrustLevel != "autonomous" {
		t.Fatalf("bound = %+v", bound)
	}
	var nilExec *Executor
	if got := nilExec.OperatorBound(); !got.RampStart.IsZero() {
		t.Fatalf("nil executor bound = %+v", got)
	}
}

func TestRampFloorFollowsTheConfig(t *testing.T) {
	cfg := windowedAutonomousConfig()
	cfg.Trust.RampSafeHours, cfg.Trust.RampModerateHours = 3, 7
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	exec := New(nil, cfg, start, noopExecLog)
	want := earned.RampFloor{Start: start, Safe: 3 * time.Hour, Moderate: 7 * time.Hour}
	if got := exec.RampFloor(); got != want {
		t.Fatalf("floor = %+v, want %+v", got, want)
	}
	if got := New(nil, nil, start, noopExecLog).RampFloor(); got !=
		(earned.RampFloor{Start: start}) {
		t.Fatalf("floor without a config = %+v", got)
	}
}

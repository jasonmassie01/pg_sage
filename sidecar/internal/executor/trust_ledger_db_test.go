package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Integration (real Postgres, real ledger): the executor's gate reads
// the unified ledger. A grandfathered L3 class executes a finding; the
// same finding becomes a one-click handoff after a demotion to L2 and a
// manual script at L1. Only the HA role and the concurrency signal are
// fixed inputs.

type primaryHA struct{}

func (primaryHA) HAStatus(context.Context) (earned.HAState, error) {
	return earned.HAState{Role: earned.RolePrimary}, nil
}

// noConcurrentActions is the concurrency signal of a quiet database (the
// package's other tests act on their own tables at the same time).
type noConcurrentActions struct{}

func (noConcurrentActions) ConcurrentActions(context.Context, []string, bool,
	time.Duration) (int, error) {
	return 0, nil
}

func analyzeFinding() analyzer.Finding {
	return analyzer.Finding{Category: "stale_statistics",
		ObjectIdentifier: "public.trust_probe", Title: "stale statistics",
		RecommendedSQL: "ANALYZE public.trust_probe"}
}

func TestExecutorGateReadsTheRealLedger(t *testing.T) {
	pool, ctx := requireDB(t)
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	database := "trust-" + time.Now().UTC().Format("150405.000000000")
	store, err := earned.NewPostgresStore(pool, deployment, database)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := earned.NewService(store, earned.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// The executor runs without its own pool so the shared database's
	// usage limits (other tests' actions) cannot park the probe; the
	// ledger is the real one.
	exec := New(nil, windowedAutonomousConfig(), time.Now().Add(-60*24*time.Hour),
		noopExecLog)
	exec.SetExecutionMode("auto")
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	rep, err := svc.SeedGrandfathered(ctx, database, exec.OperatorBound())
	if err != nil || !rep.Migrated {
		t.Fatalf("grandfather = %+v (%v)", rep, err)
	}
	exec.WithAutonomy(svc.Limiter(earned.Binding{Database: database, HA: primaryHA{},
		Concurrency: noConcurrentActions{}}))
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)

	got := exec.evaluateFindingPolicy(ctx, analyzeFinding(), false)
	if got.Decision != PolicyDecisionExecute ||
		got.BlockedReason != string(policy.ReasonAutonomyL3) {
		t.Fatalf("grandfathered L3 analyze = %+v", got)
	}
	for _, step := range []struct {
		to       earned.Level
		decision string
		reason   policy.Reason
	}{
		{earned.L2, PolicyDecisionQueueApproval, policy.ReasonAutonomyHandoff},
		{earned.L1, PolicyDecisionObserveOnly, policy.ReasonAutonomyLevel},
	} {
		if _, err := svc.Downgrade(ctx, earned.DowngradeRequest{Family: earned.FamilyHygiene,
			Class: earned.ClassAnalyze, To: step.to, Actor: "user:1:ops@example.com",
			Reason: "integration step"}); err != nil {
			t.Fatal(err)
		}
		got = exec.evaluateFindingPolicy(ctx, analyzeFinding(), false)
		if got.Decision != step.decision || got.BlockedReason != string(step.reason) {
			t.Fatalf("at %v: %+v, want %s/%s", step.to, got, step.decision, step.reason)
		}
	}
}

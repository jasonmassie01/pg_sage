package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Regression tests for G4-B17 (gate usage never wired) and G4-B19
// (recentActions written from two goroutines without a lock).

func TestStandingUsageCountsSelfInitiatedChangesInWindow(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{},
		func(string, string, ...any) {})
	before, err := exec.standingUsage(ctx, policy.ActionRequest{})
	if err != nil {
		t.Fatalf("standingUsage: %v", err)
	}
	decision, err := ledger.NewService(ledger.NewPostgresRepository(pool)).RecordDecision(
		ctx, ledger.DecisionInput{
			Feature: "index", Intent: "index", Verdict: ledger.VerdictExecute,
			Reason: "authorized", RiskTier: "safe", PolicyVersion: 1,
			TargetObjects: []string{"public.usage_probe_a"},
		})
	if err != nil {
		t.Fatalf("record decision: %v", err)
	}
	const inserted = 3
	for i := 0; i < inserted; i++ {
		sql := fmt.Sprintf("ANALYZE public.usage_probe_%d", i)
		if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log
			(action_type, sql_executed, outcome, decision_id)
			VALUES ('analyze', $1, 'success', $2)`, sql, decision.ID); err != nil {
			t.Fatalf("insert action: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.action_log WHERE decision_id=$1`, decision.ID)
	})

	after, err := exec.standingUsage(ctx, policy.ActionRequest{})
	if err != nil {
		t.Fatalf("standingUsage: %v", err)
	}
	if got := after.SelfInitiatedChangesInWindow - before.SelfInitiatedChangesInWindow; got !=
		inserted {
		t.Fatalf("self-initiated delta = %d, want %d", got, inserted)
	}
	if after.TablesInWindow < 1 {
		t.Fatalf("tables in window = %d, want >= 1", after.TablesInWindow)
	}
}

func TestRecentActionsSafeForConcurrentUse(t *testing.T) {
	cfg := config.DefaultConfig()
	exec := New(nil, cfg, time.Time{}, func(string, string, ...any) {})
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				object := fmt.Sprintf("public.t%d_%d", worker, i%50)
				exec.noteRecentAction(object)
				_ = exec.isCascadeCooldown(object)
				exec.pruneRecentActions()
			}
		}(worker)
	}
	wg.Wait()
	if !exec.isCascadeCooldown("public.t0_1") {
		t.Fatal("recent action was not recorded")
	}
}

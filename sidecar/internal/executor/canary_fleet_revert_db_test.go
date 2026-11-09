package executor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rollout"
	"github.com/pg-sage/sidecar/internal/verify"
)

// executor-04 review, integration: when the target's own monitor rolls the
// canary's change back (a regression), the canary halts and records that
// database rolled_back. It must not record rollback_failed because the
// real executor refuses a second rollback of a rolled-back action.
func TestFleetCanaryRevertByTheTargetMonitorRecordsRolledBack(t *testing.T) {
	controlPool, _ := requireDB(t)
	source := fleetDatabase(t, "canary_rv_src")
	target := fleetDatabase(t, "canary_rv_tgt")
	ctx := context.Background()
	srcID, err := source.e.ExecuteManual(ctx, firstFinding(t, source), fleetCanarySQL,
		fleetCanaryRollbackSQL, nil)
	if err != nil {
		t.Fatalf("source execute: %v", err)
	}
	settleVerdict(t, source, srcID, verify.OutcomeImproved)
	svc := fleetCanaryService(t, controlPool, map[string]rollout.CanaryTarget{
		"canary_rv_src": source, "canary_rv_tgt": target})
	started, err := svc.Start(ctx, rollout.StartRequest{SourceDatabase: "canary_rv_src",
		ActionLogID: srcID, Targets: []string{"canary_rv_tgt"},
		StartedBy: "user:1:a@example.com"})
	if err != nil {
		t.Fatalf("start canary: %v", err)
	}
	actionID := waitForMonitoring(t, target)
	// The target's monitor reverts the change, as on a regression.
	if err := target.e.RollbackAction(ctx, actionID, "regression"); err != nil {
		t.Fatalf("target monitor rollback: %v", err)
	}
	run := waitForRunEnd(t, svc, started.EvidenceID)
	if run.State != "halted" || run.HaltReason != "verification_failed" {
		t.Fatalf("run = %+v, want halted on verification_failed", run)
	}
	_, instances, err := svc.Get(ctx, started.EvidenceID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %+v (%v), want one", instances, err)
	}
	if got := instances[0]; got.Status != rollout.StatusRolledBack || got.ActionLogID != actionID {
		t.Fatalf("instance = %+v, want rolled_back of action %d", got, actionID)
	}
	if _, outcome := latestAction(t, target); outcome != "rolled_back" {
		t.Fatalf("target action outcome %q, want rolled_back", outcome)
	}
	var indexes int
	if err := target.e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes
		WHERE indexname = 'idx_canary_orders'`).Scan(&indexes); err != nil || indexes != 0 {
		t.Fatalf("index count = %d (%v), want 0 after the revert", indexes, err)
	}
}

// fleetCanaryService is a canary over targets that polls every 20 ms.
func fleetCanaryService(t *testing.T, control *pgxpool.Pool,
	targets map[string]rollout.CanaryTarget) *rollout.CanaryService {
	t.Helper()
	svc, err := rollout.NewCanaryService(func(name string) (rollout.CanaryTarget, error) {
		if tg, ok := targets[name]; ok {
			return tg, nil
		}
		return nil, fmt.Errorf("database %q is not in the fleet", name)
	}, rollout.NewPostgresRunStore(control), rollout.CanaryOptions{CanaryInstances: 1,
		RegressionLimitPct: 10, PollEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

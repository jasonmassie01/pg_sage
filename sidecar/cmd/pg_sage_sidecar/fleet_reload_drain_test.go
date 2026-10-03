package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func executeDecision(context.Context) (executor.ActionPolicyDecision, error) {
	return executor.ActionPolicyDecision{Decision: executor.PolicyDecisionExecute}, nil
}

// sleepingAction runs pg_sleep on the database as an executor action.
func sleepingAction(
	inst *fleet.DatabaseInstance, seconds float64, started chan<- struct{},
) executor.ActionIntent {
	return executor.ActionIntent{
		Authorize: executeDecision,
		Execute: func(ctx context.Context, _ executor.ActionPolicyDecision) (int64, error) {
			close(started)
			_, err := inst.Pool.Exec(ctx, "SELECT pg_sleep($1)", seconds)
			return 1, err
		},
	}
}

// gatedAction is an executor action that stays in flight until proceed is
// closed, then runs one statement on the database's pool and records that
// its work finished. The test decides when it ends, so its ordering
// against a drain is never left to timing.
func gatedAction(
	inst *fleet.DatabaseInstance, started chan<- struct{}, proceed <-chan struct{},
	finished *atomic.Bool,
) executor.ActionIntent {
	return executor.ActionIntent{
		Authorize: executeDecision,
		Execute: func(ctx context.Context, _ executor.ActionPolicyDecision) (int64, error) {
			close(started)
			select {
			case <-proceed:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			if _, err := inst.Pool.Exec(ctx, "SELECT pg_sleep(0.1)"); err != nil {
				return 0, err
			}
			finished.Store(true)
			return 1, nil
		},
	}
}

func noopIntent(executed *atomic.Int32) executor.ActionIntent {
	return executor.ActionIntent{
		Authorize: executeDecision,
		Execute: func(context.Context, executor.ActionPolicyDecision) (int64, error) {
			executed.Add(1)
			return 0, nil
		},
	}
}

// runtimeBoundContext ties an action to the runtime's lifecycle the way the
// orchestrator's actions are: cancelling the runtime cancels the action.
func runtimeBoundContext(t *testing.T, inst *fleet.DatabaseInstance) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runtimeCancel := inst.Cancel
	inst.Cancel = func() {
		runtimeCancel()
		cancel()
	}
	return ctx
}

// The removal must wait for the in-flight action. The action is held in
// flight until the drain is seen parking new actions, so the drain always
// starts while it runs; whether the action's work had finished is read the
// moment the reload returns (comparing two goroutines' clock readings
// raced: the action's goroutine could be scheduled after the reload's).
func TestFleetReloadRemovalLetsInFlightActionFinish(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	inst := fleetMgr.GetInstance("b")
	actionCtx := runtimeBoundContext(t, inst)
	started, proceed := make(chan struct{}), make(chan struct{})
	var workFinished, finishedAtReturn atomic.Bool
	actionDone := make(chan error, 1)
	go func() {
		_, err := inst.Executor.Apply(actionCtx,
			gatedAction(inst, started, proceed, &workFinished))
		actionDone <- err
	}()
	<-started
	reloadDone := make(chan error, 1)
	go func() {
		err := env.reload(func(c *config.Config) {
			c.Databases = withoutDatabase(c.Databases, "b")
		})
		finishedAtReturn.Store(workFinished.Load())
		reloadDone <- err
	}()
	// Once the runtime drains, a new action parks instead of starting.
	var executed atomic.Int32
	eventually(t, 30*time.Second, "draining runtime to park new actions", func() bool {
		_, err := inst.Executor.Apply(context.Background(), noopIntent(&executed))
		return errors.Is(err, executor.ErrDDLSlotUnavailable)
	})
	close(proceed)
	if err := <-actionDone; err != nil {
		t.Fatalf("in-flight action was interrupted: %v", err)
	}
	if err := <-reloadDone; err != nil {
		t.Fatalf("removal reload: %v", err)
	}
	if !finishedAtReturn.Load() || !poolClosed(inst.Pool) {
		t.Fatal("removal completed before the in-flight action finished")
	}
	if fleetMgr.GetInstance("b") != nil {
		t.Fatal("removed database still registered")
	}
	if err := fleet.ShutdownInstance(context.Background(), inst); err != nil {
		t.Fatalf("drained teardown reported %v", err)
	}
}

func TestFleetReloadRemovalBoundsAStuckAction(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	inst := fleetMgr.GetInstance("b")
	inst.DrainTimeout = 300 * time.Millisecond
	actionCtx := runtimeBoundContext(t, inst)
	started := make(chan struct{})
	actionDone := make(chan error, 1)
	go func() {
		_, err := inst.Executor.Apply(actionCtx, sleepingAction(inst, 30, started))
		actionDone <- err
	}()
	<-started
	began := time.Now()
	if err := env.reload(func(c *config.Config) {
		c.Databases = withoutDatabase(c.Databases, "b")
	}); err != nil {
		t.Fatalf("removal reload: %v", err)
	}
	if elapsed := time.Since(began); elapsed > 15*time.Second {
		t.Fatalf("removal of a stuck action took %v", elapsed)
	}
	if err := <-actionDone; err == nil {
		t.Fatal("a stuck action survived the bounded drain")
	}
	err := fleet.ShutdownInstance(context.Background(), inst)
	if !errors.Is(err, executor.ErrQuiesceTimeout) {
		t.Fatalf("teardown error = %v, want the drain timeout surfaced", err)
	}
	if !poolClosed(inst.Pool) {
		t.Fatal("stuck runtime's pool stayed open")
	}
}

func TestFleetReloadRebuildDrainsTheRetiredGeneration(t *testing.T) {
	newFleetReloadEnv(t, "ctl", "b")
	old := fleetMgr.GetInstance("b")
	actionCtx := runtimeBoundContext(t, old)
	started := make(chan struct{})
	actionDone := make(chan error, 1)
	go func() {
		_, err := old.Executor.Apply(actionCtx, sleepingAction(old, 1, started))
		actionDone <- err
	}()
	<-started
	if err := reloadDatabase(t, "b", func(db *config.DatabaseConfig) {
		db.MaxConnections = 5
	}); err != nil {
		t.Fatalf("rebuild reload: %v", err)
	}
	if err := <-actionDone; err != nil {
		t.Fatalf("old generation's in-flight action was interrupted: %v", err)
	}
	replacement := fleetMgr.GetInstance("b")
	if replacement == old || !poolClosed(old.Pool) || poolClosed(replacement.Pool) {
		t.Fatal("rebuild did not retire the old generation after its action")
	}
}

// failingCommitOwner fails its commit after the fleet owner committed, so
// the controller rolls the fleet owner back.
type failingCommitOwner struct{ name string }

func (o failingCommitOwner) Name() string { return o.name }

func (o failingCommitOwner) Prepare(
	context.Context, config.ConfigSnapshot, config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	return failingCommit{}, nil
}

type failingCommit struct{}

func (failingCommit) Commit(context.Context) error   { return errors.New("collector refused") }
func (failingCommit) Rollback(context.Context) error { return nil }
func (failingCommit) Drain(context.Context) error    { return nil }

func TestFleetReloadRollsBackWhenALaterOwnerFails(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	if err := configController.RegisterOwner(failingCommitOwner{name: "collector"}); err != nil {
		t.Fatal(err)
	}
	extra := testdbExtra(t, env, "c")
	oldB := fleetMgr.GetInstance("b")
	err := env.reload(func(c *config.Config) {
		c.Databases = append(withoutDatabase(c.Databases, "b"), extra)
		c.Collector.IntervalSeconds = 1800
	})
	if err != nil {
		t.Fatalf("reload with a failing owner: %v", err)
	}
	if fleetMgr.GetInstance("b") != oldB || poolClosed(oldB.Pool) {
		t.Fatal("rollback did not restore the removed database's running runtime")
	}
	if fleetMgr.GetInstance("c") != nil || fleetMgr.InstanceCount() != 2 {
		t.Fatal("rollback left the added database published")
	}
	if _, ok := fleetLLMBudget.Snapshot()["c"]; ok {
		t.Fatal("rolled-back addition kept a budget share")
	}
	if databaseIndex(cfg.Databases, "b") < 0 || databaseIndex(cfg.Databases, "c") >= 0 {
		t.Fatal("process config does not show the restored database list")
	}
	eventually(t, 5*time.Second, "rolled-back candidate backends to close", func() bool {
		return pgSageConnections(t, env.admin, extra.Database) == 0
	})
}

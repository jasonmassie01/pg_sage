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

func TestFleetReloadRemovalLetsInFlightActionFinish(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	inst := fleetMgr.GetInstance("b")
	actionCtx := runtimeBoundContext(t, inst)
	started := make(chan struct{})
	actionDone := make(chan time.Time, 1)
	var actionErr error
	go func() {
		_, actionErr = inst.Executor.Apply(actionCtx, sleepingAction(inst, 1.5, started))
		actionDone <- time.Now()
	}()
	<-started
	reloadDone := make(chan time.Time, 1)
	var reloadErr error
	go func() {
		reloadErr = env.reload(func(c *config.Config) {
			c.Databases = withoutDatabase(c.Databases, "b")
		})
		reloadDone <- time.Now()
	}()
	// Once the runtime drains, a new action parks instead of starting.
	var executed atomic.Int32
	eventually(t, 5*time.Second, "draining runtime to park new actions", func() bool {
		_, err := inst.Executor.Apply(context.Background(), noopIntent(&executed))
		return errors.Is(err, executor.ErrDDLSlotUnavailable)
	})
	finished := <-actionDone
	if actionErr != nil {
		t.Fatalf("in-flight action was interrupted: %v", actionErr)
	}
	returned := <-reloadDone
	if reloadErr != nil {
		t.Fatalf("removal reload: %v", reloadErr)
	}
	if returned.Before(finished) || !poolClosed(inst.Pool) {
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

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// plan-02a: the plan engine ticks in the executor cycle, after the
// executor, only on a writable primary; a tick's error is logged and the
// cycle carries on.

type countingPlans struct {
	ticks atomic.Int32
	err   error
}

func (c *countingPlans) Tick(context.Context) error {
	c.ticks.Add(1)
	return c.err
}

func TestExecutorCycleTicksThePlanEngineOnAPrimary(t *testing.T) {
	m, _ := scriptedHAMonitor(false, false)
	plans := &countingPlans{err: errors.New("plan 3: read failed")}
	exec := &recordingExecutor{}
	runFleetDBCycle(context.Background(), fleetCycleDeps{name: "orders", ha: m, exec: exec,
		plans: plans})
	if exec.last(t) || plans.ticks.Load() != 1 {
		t.Fatalf("primary: replica=%v, plan ticks %d, want a writable cycle and one tick",
			exec.last(t), plans.ticks.Load())
	}
}

func TestExecutorCycleDoesNotTickThePlanEngineOnAReplica(t *testing.T) {
	m, _ := scriptedHAMonitor(true, true)
	plans := &countingPlans{}
	exec := &recordingExecutor{}
	runFleetDBCycle(context.Background(), fleetCycleDeps{name: "orders", ha: m, exec: exec,
		plans: plans})
	if !exec.last(t) || plans.ticks.Load() != 0 {
		t.Fatalf("replica: replica=%v, plan ticks %d, want none", exec.last(t),
			plans.ticks.Load())
	}
}

// No plan engine (none built): the cycle runs as before.
func TestExecutorCycleWithoutAPlanEngine(t *testing.T) {
	m, _ := scriptedHAMonitor(false, false)
	exec := &recordingExecutor{}
	runFleetDBCycle(context.Background(), fleetCycleDeps{name: "orders", ha: m, exec: exec})
	if exec.last(t) {
		t.Fatal("a primary's cycle was told replica")
	}
}

package main

import (
	"fmt"
	"os"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/executor"
)

// planRunnerName names the plan engine's runner among the executor's
// approved-action runners.
const planRunnerName = "plans"

// buildPlanEngine builds the database's plan engine (plan-02a) and
// registers its runner for plan step queue items. It runs only plans a
// person approved, so it is on with no configuration; only the fleet
// leader ticks it. Without it (an error, logged) plan steps cannot run:
// the executor refuses their items.
func (rt *databaseRuntime) buildPlanEngine() planTicker {
	var dispatch changeplan.Dispatcher
	if rt.dispatcher != nil {
		dispatch = rt.dispatcher
	}
	engine, err := changeplan.NewEngine(changeplan.EngineConfig{Pool: rt.spec.Pool,
		Executor: rt.executor, Waits: rt.executor.VerificationWaits(),
		Verdicts: changeplan.NewDBVerdicts(rt.spec.Pool), Notify: dispatch,
		Leader: func() bool { return fleetLeader == nil || fleetLeader.IsLeader() },
		Owner:  planEngineOwner(), Database: rt.spec.Name, LogFn: logStructuredWrapper})
	if err != nil {
		logError(rt.spec.Scope, "db %q: plan engine unavailable: %v", rt.spec.Name, err)
		return nil
	}
	if err := rt.executor.AddApprovedActionRunner(planRunnerName,
		[]string{executor.PlanStepIdentityPrefix}, engine.Runner(rt.executor)); err != nil {
		logError(rt.spec.Scope, "db %q: plan engine unavailable: %v", rt.spec.Name, err)
		return nil
	}
	rt.note("plans")
	return engine
}

// planEngineOwner names this sidecar's claims on plan steps.
func planEngineOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "sidecar"
	}
	if len(host) > 100 {
		host = host[:100]
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

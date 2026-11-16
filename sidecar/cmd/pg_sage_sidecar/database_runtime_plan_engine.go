package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/policy"
)

// planRunnerName names the plan engine's runner among the executor's
// approved-action runners.
const planRunnerName = "plans"

// buildPlanEngine builds the database's plan engine (plan-02a) and
// registers its runner for plan step queue items. It runs only plans a
// person approved, so it is on with no configuration; only the fleet
// leader ticks it. Without it (an error, logged) plan steps cannot run:
// the executor refuses their items. Its plan caps come from the plans
// block of the standing policy the executor enforces (plan-02b); operator
// steps are judged with the executor's verification v2 readers after its
// verification window (plan-02d). Its step claims are the fleet leader's
// holder id with the elector's lease (plan-02e): a new leader resumes a
// step only once the old leader's claim expired.
func (rt *databaseRuntime) buildPlanEngine() planTicker {
	var dispatch changeplan.Dispatcher
	if rt.dispatcher != nil {
		dispatch = rt.dispatcher
	}
	caps := rt.planCaps()
	v2, window := rt.executor.OperatorVerification()
	owner, lease := planClaim(fleetLeader)
	engine, err := changeplan.NewEngine(changeplan.EngineConfig{Pool: rt.spec.Pool,
		Executor: rt.executor, Waits: rt.executor.VerificationWaits(),
		Verdicts: changeplan.NewDBVerdicts(rt.spec.Pool), Notify: dispatch,
		Leader: func() bool { return fleetLeader == nil || fleetLeader.IsLeader() },
		Owner:  owner, Lease: lease, Database: rt.spec.Name, LogFn: logStructuredWrapper,
		Caps: caps, Operator: changeplan.NewOperatorJudge(&v2, window)})
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

// planCaps reads the plan caps from the plans block of the standing policy
// the executor enforces (plan-02b): the engine checks steps against it and
// the plan API checks approvals against it (plan-03a).
func (rt *databaseRuntime) planCaps() changeplan.CapsSource {
	store, scope := rt.policyStoreScope()
	return planCapsFrom(func(ctx context.Context) (policy.Policy, error) {
		return store.Current(ctx, scope)
	})
}

// planClaim is the owner and lease of the plan engine's step claims: the
// elector's holder id and lease once it runs; before it does (database
// runtimes start first) or without one, this process's name (unique per
// process, as the holder id is) and the configured leader lease.
func planClaim(e *leader.Elector) (string, time.Duration) {
	if e != nil && e.Holder() != "" {
		return truncateOwner(e.Holder()), e.TTL()
	}
	lease := leader.DefaultTTL
	if cfg != nil && cfg.FleetLearning.LeaderLeaseSeconds > 0 {
		lease = cfg.FleetLearning.LeaderLease()
	}
	return planEngineOwner(), lease
}

// truncateOwner keeps a claim owner within the step's claimed_by column.
func truncateOwner(s string) string {
	if len(s) > 100 {
		return s[:100]
	}
	return s
}

// planEngineOwner names this sidecar's claims on plan steps.
func planEngineOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "sidecar"
	}
	return truncateOwner(fmt.Sprintf("%s:%d", truncateOwner(host), os.Getpid()))
}

// planCapsFrom is the plan caps source over a standing-policy read: the
// document's plans block (its defaults when it has none). A read that
// fails, or a stored document that does not parse, is an error, so plan
// steps wait (fail closed).
func planCapsFrom(read func(context.Context) (policy.Policy, error)) changeplan.CapsSource {
	return func(ctx context.Context) (policy.PlanCaps, error) {
		current, err := read(ctx)
		if err != nil {
			return policy.PlanCaps{}, fmt.Errorf("read the standing policy: %w", err)
		}
		doc, err := policy.DocumentFromPolicy(current)
		if err != nil {
			return policy.PlanCaps{}, err
		}
		return doc.Plans, nil
	}
}

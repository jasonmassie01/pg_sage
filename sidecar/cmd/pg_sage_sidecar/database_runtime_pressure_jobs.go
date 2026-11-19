package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/pressure"
)

// pressureEnforceInterval is how often the enforcer checks the enforced
// jobs: well past the checker's 2 s cache, so two consecutive stops are
// two read sets at least 10 s apart.
const pressureEnforceInterval = 10 * time.Second

// startPressure builds the database's check_pressure (guard-01a) and its
// backfill jobs (guard-01b): the job store, the approved-cancel runner on
// the executor and the enforcer loop. The enforcer only proposes, into
// the approval queue; only the fleet leader on a writable primary runs
// it. Without a pool there is neither; without an executor, jobs register
// but nothing is enforced.
func (rt *databaseRuntime) startPressure() {
	rt.pressure = rt.pressureChecker()
	if rt.pressure == nil {
		return
	}
	jobs := pressure.NewJobStore(rt.spec.Pool)
	rt.pressureJobs = jobs
	if rt.executor == nil {
		return
	}
	backends := pressure.NewSQLBackends(rt.spec.Pool)
	roles, apps := protectedBackends(rt.cfg)
	runner, err := pressure.NewRunner(pressure.RunnerOptions{Jobs: jobs,
		Pressure: rt.pressure, Backends: backends, Exec: rt.executor,
		ProtectedRoles: roles, ProtectedApplications: apps})
	if err == nil {
		err = rt.executor.AddApprovedActionRunner(pressure.RunnerName,
			[]string{pressure.JobIdentityPrefix}, runner)
	}
	var enforcer *pressure.Enforcer
	if err == nil {
		enforcer, err = pressure.NewEnforcer(pressure.EnforcerOptions{Database: rt.spec.Name,
			Jobs: jobs, Pressure: rt.pressure, Backends: backends,
			Proposals: pressure.NewPGProposer(rt.spec.Pool, rt.recordID())})
	}
	if err != nil {
		logError(rt.spec.Scope, "db %q: pressure job enforcement unavailable: %v",
			rt.spec.Name, err)
		return
	}
	rt.start(func() { rt.enforcePressure(enforcer) })
	rt.note("pressure_jobs")
}

// protectedBackends are the operator's never-signalled roles and
// applications (sre.actions), which a pressure cancel honours too.
func protectedBackends(cfg *config.Config) (roles, apps []string) {
	if cfg == nil {
		return nil, nil
	}
	config.RLockForHotReload()
	defer config.RUnlockForHotReload()
	a := cfg.SRE.Actions
	return append([]string(nil), a.ProtectedRoles...),
		append([]string(nil), a.ProtectedApplications...)
}

// enforcePressure ticks the enforcer until the runtime stops.
func (rt *databaseRuntime) enforcePressure(e pressureTicker) {
	ticker := time.NewTicker(pressureEnforceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rt.ctx.Done():
			return
		case <-ticker.C:
			runPressureTick(rt.ctx, rt.spec.Name, e, rt.mayPropose)
		}
	}
}

// pressureTicker is the enforcer (pressure.Enforcer).
type pressureTicker interface {
	Tick(ctx context.Context) (pressure.TickResult, error)
}

// mayPropose: only the fleet leader on a writable primary proposes.
func (rt *databaseRuntime) mayPropose() bool {
	if !currentFleetLeader().IsLeader() {
		return false
	}
	return rt.ha == nil || !haNotWritable(rt.ha)
}

// runPressureTick runs one tick when allowed and logs what it proposed or
// why it failed; a failure leaves the next tick to retry.
func runPressureTick(ctx context.Context, name string, e pressureTicker,
	allowed func() bool) {
	if !allowed() {
		return
	}
	res, err := e.Tick(ctx)
	if err != nil {
		logWarn("pressure", "[%s] backfill job enforcement: %v", name, err)
	}
	if res.Proposed > 0 {
		logInfo("pressure", "[%s] proposed %d backfill cancel(s) for approval after "+
			"consecutive stop verdicts", name, res.Proposed)
	}
}

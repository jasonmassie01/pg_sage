package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// managedChangeInterval is how often managed-change intents become
// proposals, applied values are observed and drift is checked.
const managedChangeInterval = 5 * time.Minute

// newCloudTelemetry is a variable so tests can stub the credential chains.
var newCloudTelemetry = func(ctx context.Context, rt *databaseRuntime) (*cloudtel.Runtime,
	managedparam.Resolver) {
	host := rt.spec.Pool.Config().ConnConfig.Host
	return cloudTelemetryFor(ctx, rt.cfg, rt.provider, host, rt.spec.Name, rt.spec.Shared,
		defaultCloudDeps())
}

// initCloudTelemetry resolves the database's managed-cloud telemetry
// before monitoring starts, so the memory gates can read it.
func (rt *databaseRuntime) initCloudTelemetry() {
	rt.cloud, rt.cloudResolver = newCloudTelemetry(rt.ctx, rt)
	if rt.cloud == nil {
		return
	}
	cloudtel.Register(rt.cloud)
	rt.start(func() {
		<-rt.ctx.Done()
		cloudtel.Unregister(rt.cloud)
	})
	if st := rt.cloud.Status(); !st.Available && rt.cloudResolver == nil {
		logWarn(rt.spec.Scope, "db %q: managed-cloud telemetry %s", rt.spec.Name, st.Reason)
	}
}

// hostMemory is live host memory from telemetry (zero when unknown).
func (rt *databaseRuntime) hostMemory() advisor.HostMemory {
	if rt.cloud == nil {
		return advisor.HostMemory{}
	}
	total, available := rt.cloud.HostMemory()
	return advisor.HostMemory{TotalBytes: total, AvailableBytes: available}
}

// startCloudTelemetry starts polling, gives the executor host CPU and the
// host guards, and runs the managed-change worker. Telemetry supplies
// evidence only: trust levels and the policy gate are untouched.
func (rt *databaseRuntime) startCloudTelemetry() {
	if rt.cloud != nil && rt.cloudResolver != nil {
		if rt.executor != nil {
			rt.executor.WithHostCPUReader(rt.cloud)
		}
		rt.start(func() {
			rt.cloud.Run(rt.ctx, func(err error) {
				logWarn(rt.spec.Scope, "db %q: cloud telemetry: %v", rt.spec.Name, err)
			})
		})
		rt.note("cloud_telemetry")
	}
	if !managedparam.Supported(rt.provider) {
		return
	}
	worker, err := managedparam.NewWorker(managedparam.WorkerOptions{Database: rt.spec.Name,
		Provider: rt.provider, Pool: rt.spec.Pool, Store: managedparam.NewStore(rt.spec.Pool),
		Resolver: rt.cloudResolver})
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: managed changes unavailable: %v", rt.spec.Name, err)
		return
	}
	rt.start(func() { worker.Run(rt.ctx, managedChangeInterval, rt.afterManagedCycle) })
	rt.note("managed_changes")
}

// afterManagedCycle publishes drift and refreshes the managed findings.
func (rt *databaseRuntime) afterManagedCycle(res managedparam.CycleResult, err error) {
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: managed changes: %v", rt.spec.Name, err)
		return
	}
	for _, e := range res.BuildErrors {
		logWarn(rt.spec.Scope, "db %q: managed change not proposed: %s", rt.spec.Name, e)
	}
	if res.Proposed > 0 || res.Applied > 0 {
		logInfo(rt.spec.Scope, "db %q: managed changes: %d proposed, %d observed applied",
			rt.spec.Name, res.Proposed, res.Applied)
	}
	if rt.cloud != nil {
		rt.cloud.SetDrift(res.Drift)
	}
	ctx, cancel := context.WithTimeout(rt.ctx, 30*time.Second)
	defer cancel()
	if err := upsertManagedFindings(ctx, rt.spec.Pool, res.TargetError == "", res.Drift,
		rt.cloud); err != nil {
		logWarn(rt.spec.Scope, "db %q: managed-cloud findings: %v", rt.spec.Name, err)
	}
}

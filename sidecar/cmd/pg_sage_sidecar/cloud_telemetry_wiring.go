package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/managedparam"
	prov "github.com/pg-sage/sidecar/internal/provider"
)

// managedChangeInterval is how often managed-change intents become
// proposals, applied values are observed and drift is checked.
const managedChangeInterval = 5 * time.Minute

// newCloudTelemetry is a variable so tests can stub the credential chains.
var newCloudTelemetry = func(ctx context.Context, rt *databaseRuntime) (*cloudtel.Runtime,
	managedparam.Resolver) {
	return runtimeCloudTelemetry(ctx, rt, defaultCloudDeps())
}

// runtimeCloudTelemetry resolves the telemetry of rt's database from its
// host and its own cloud_resource.
func runtimeCloudTelemetry(ctx context.Context, rt *databaseRuntime,
	deps cloudDeps) (*cloudtel.Runtime, managedparam.Resolver) {
	host := rt.spec.Pool.Config().ConnConfig.Host
	return cloudTelemetryFor(ctx, rt.cfg, rt.provider, host, rt.spec.Name, rt.spec.Shared,
		rt.spec.Config.CloudResource, deps)
}

// initCloudTelemetry acquires the managed-cloud telemetry of the
// database's provider instance before monitoring starts, so the memory
// gates can read it. Databases on one instance share it (clouds-08): it
// polls from the first acquire until the last database stops.
func (rt *databaseRuntime) initCloudTelemetry() {
	var release func()
	rt.cloud, rt.cloudResolver, release = processCloudTelemetry.acquire(rt.cloudShareKey(),
		rt.spec.Name, func(ctx context.Context) (*cloudtel.Runtime, managedparam.Resolver) {
			return newCloudTelemetry(ctx, rt)
		})
	if rt.cloud == nil {
		return
	}
	rt.start(func() {
		<-rt.ctx.Done()
		release()
	})
	rt.attachHostUsage()
	if st := rt.cloud.Status(); !st.Available && !rt.cloud.HasSource() {
		logWarn(rt.spec.Scope, "db %q: managed-cloud telemetry %s", rt.spec.Name, st.Reason)
	}
}

// cloudShareKey names the database's provider instance (cloudShareKey).
func (rt *databaseRuntime) cloudShareKey() string {
	var ct config.CloudTelemetryConfig
	if rt.cfg != nil {
		ct = rt.cfg.CloudTelemetry
	}
	if rt.provider == prov.CNPG && rt.spec.Config.CNPG.Enabled() {
		return cnpgShareKey(rt.spec.Config.CNPG)
	}
	cc := rt.spec.Pool.Config().ConnConfig
	return cloudShareKey(rt.provider, cc.Host, cc.Port, rt.spec.Config.CloudResource, ct)
}

// hostMemory is live host memory from managed-cloud telemetry or, without
// it, from Supabase observability (zero when unknown or stale).
func (rt *databaseRuntime) hostMemory() advisor.HostMemory {
	var total, available int64
	if rt.cloud != nil {
		total, available = rt.cloud.HostMemory()
	} else if obs := rt.providerObs.Load(); obs != nil {
		total, available = obs.HostMemory()
	}
	return advisor.HostMemory{TotalBytes: total, AvailableBytes: available}
}

// startCloudTelemetry gives the executor host CPU and the host guards,
// and runs the managed-change worker (the shared runtime already polls).
// Telemetry supplies evidence only: trust levels and the policy gate are
// untouched. The worker joins its instance's peers, so a change another
// database of the instance holds is not proposed twice.
func (rt *databaseRuntime) startCloudTelemetry() {
	if rt.cloud.HasSource() {
		if rt.executor != nil {
			rt.executor.WithHostCPUReader(rt.cloud)
		}
		rt.note("cloud_telemetry")
		rt.startRightsize()
	}
	rt.start(rt.checkCNPGHint)
	if !managedparam.Supported(rt.provider) {
		return
	}
	resolver := managedChangeResolver(rt.cloudResolver, rt.azureParams)
	if rt.provider == prov.CNPG {
		resolver = rt.cnpgChangeResolver()
	}
	resource, store := rt.cloudShareKey(), managedparam.NewStore(rt.spec.Pool)
	worker, err := managedparam.NewWorker(managedparam.WorkerOptions{Database: rt.spec.Name,
		Provider: rt.provider, Pool: rt.spec.Pool, Store: store, Resolver: resolver,
		Resource: resource})
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: managed changes unavailable: %v", rt.spec.Name, err)
		return
	}
	leave := managedparam.JoinPeers(resource, rt.spec.Name, store)
	rt.start(func() {
		defer leave()
		worker.Run(rt.ctx, managedChangeInterval, rt.afterManagedCycle)
	})
	rt.note("managed_changes")
}

// afterManagedCycle publishes drift and refreshes the managed findings.
func (rt *databaseRuntime) afterManagedCycle(res managedparam.CycleResult, err error) {
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: managed changes: %v", rt.spec.Name, err)
		return
	}
	if res.PeerError != "" {
		logWarn(rt.spec.Scope, "db %q: managed changes: %s", rt.spec.Name, res.PeerError)
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
	if err := rt.upsertBackupFindings(ctx); err != nil {
		logWarn(rt.spec.Scope, "db %q: backup findings: %v", rt.spec.Name, err)
	}
}

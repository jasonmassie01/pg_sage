package main

import "github.com/pg-sage/sidecar/internal/fleet"

// capabilityRefreshInterval is how often each runtime recollects its
// provider capabilities (clouds-04).
var capabilityRefreshInterval = fleet.DefaultCapabilityRefreshInterval

// startCapabilityRefresh runs this runtime's capability refresher as an
// instance worker, so grants and extensions added after start reach fleet
// readiness. It registers the reload hook before the instance is published.
func (rt *databaseRuntime) startCapabilityRefresh() {
	refresher := fleet.NewCapabilityRefresher(rt.inst, rt.provider,
		fleet.ExecutorFamilyExplainer(rt.executor), capabilityRefreshInterval)
	rt.inst.CapabilityRefresh = refresher.Request
	rt.start(func() { refresher.Run(rt.ctx) })
}

// requestCapabilityRefresh asks every running database to recollect its
// capabilities after a config reload.
func requestCapabilityRefresh() {
	fleetMgr.RequestCapabilityRefresh()
}

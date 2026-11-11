package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/runway"
)

// Fleet cluster ownership of runways and forecasts (forecast-09). The
// cluster's series (xid and mxid age, disk use, replication slots) and
// the cluster-wide forecasts (connection saturation, checkpoint
// pressure) are reported by one owner database per cluster instead of
// once per database. Databases watched by separate sidecar processes are
// not coordinated, as for the instance owner (advisors-08).

// runwayClusterOwners elects, per cluster (system identifier and
// postmaster start, read by the runway monitor itself), the fleet runtime
// whose runway monitor reports the cluster's series.
var runwayClusterOwners = runway.NewClusterOwners()

// forecastClusterOwners elects, per instance key, the fleet runtime whose
// forecaster raises the cluster-wide forecasts.
var forecastClusterOwners = runway.NewClusterOwners()

// runwayOwners is the runway owner set of a fleet runtime; nil outside
// fleet mode (no instance registry), where the database reports all of
// its runways itself, as before.
func (rt *databaseRuntime) runwayOwners() *runway.ClusterOwners {
	if rt.instanceOwner == nil {
		return nil
	}
	return runwayClusterOwners
}

// forecastClusterOwner is the forecaster's membership of its instance,
// left when the runtime ends. Only a system-identifier key is used: an
// address can front several clusters, and grouping them would hide one
// cluster's forecasts behind another's owner. Without one (or outside
// fleet mode) it is nil and the database raises every forecast.
func (rt *databaseRuntime) forecastClusterOwner() forecaster.ClusterOwner {
	m := rt.instanceOwner
	if m == nil {
		return nil
	}
	if m.key.source != instanceKeySystemID {
		logInfo(rt.spec.Scope, "db %q: no system identifier; its connection and "+
			"checkpoint forecasts are raised for it alone", rt.spec.Name)
		return nil
	}
	member := forecastClusterOwners.Member(m.key.key, rt.spec.Name,
		2*rt.cfg.Analyzer.Interval())
	context.AfterFunc(rt.ctx, member.Leave)
	return member
}

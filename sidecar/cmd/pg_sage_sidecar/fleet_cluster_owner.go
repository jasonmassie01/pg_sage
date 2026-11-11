package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

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

// forecastClusterOwners elects, per instance key and address, the runtime whose
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
// cluster's forecasts behind another's owner. The key also names the
// address: a restored clone or a promoted standby shares the identifier
// with its source, and grouping the two would hide the clone's forecasts
// too (two addresses of one cluster only duplicate them). Without both
// (or outside fleet mode) it is nil and the database raises every
// forecast.
func (rt *databaseRuntime) forecastClusterOwner() forecaster.ClusterOwner {
	m := rt.instanceOwner
	if m == nil {
		return nil
	}
	if m.key.source != instanceKeySystemID || rt.spec.Pool == nil {
		logInfo(rt.spec.Scope, "db %q: no system identifier; its connection and "+
			"checkpoint forecasts are raised for it alone", rt.spec.Name)
		return nil
	}
	member := forecastClusterOwners.Member(forecastOwnerKey(m.key, rt.spec.Pool),
		rt.spec.Name, 2*rt.cfg.Analyzer.Interval())
	context.AfterFunc(rt.ctx, member.Leave)
	return member
}

// forecastOwnerKey is a runtime's forecast owner key: its instance key
// and the address its pool connects to.
func forecastOwnerKey(k instanceKey, pool *pgxpool.Pool) string {
	cc := pool.Config().ConnConfig
	return k.key + "@" + factClusterAddressKey(cc.Host, cc.Port)
}

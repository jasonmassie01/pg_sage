package forecaster

import (
	"slices"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Fleet cluster ownership (forecast-09). Connection saturation counts
// every backend of the cluster and checkpoint pressure reads the
// cluster's checkpointer counters, so N fleet databases of one cluster
// raised N copies of each. With a ClusterOwner only the owner's
// forecaster raises them. Disk growth, cache pressure and query volume
// stay per database.

// ClusterOwner tells a forecaster whether its database raises the
// cluster-wide forecasts (runway.ClusterMember implements it).
type ClusterOwner interface {
	// IsClusterOwner heartbeats and returns the owner database; ok
	// reports that this database is it.
	IsClusterOwner() (owner string, ok bool)
	// Evaluated records that this owner judged the cluster forecasts.
	Evaluated()
	// OwnerHealthy reports that owner judged them recently.
	OwnerHealthy(owner string) bool
	// Databases lists the cluster's member databases.
	Databases() []string
}

// clusterForecastCategories are the system forecasts over cluster-wide
// counters.
var clusterForecastCategories = []string{
	"forecast_connection_saturation", "forecast_checkpoint_pressure",
}

func isClusterForecast(category string) bool {
	return slices.Contains(clusterForecastCategories, category)
}

// WithClusterOwner makes the cluster-wide forecasts the owner's alone
// (nil: this database raises them, as a single database does). Call it
// before the first Forecast.
func (f *Forecaster) WithClusterOwner(o ClusterOwner) *Forecaster {
	f.owner = o
	return f
}

// scopeCluster narrows one run's findings and evaluated categories to
// the forecaster's cluster scope. The owner's cluster findings name it in
// their detail, and a complete owner run is recorded. A non-owner raises
// none; it lists their categories as evaluated, so its own open copies
// resolve, only while the owner reports them, and otherwise not at all,
// so a failing owner never hides a live forecast.
func (f *Forecaster) scopeCluster(all []analyzer.Finding,
	evaluated []string) ([]analyzer.Finding, []string) {
	if f.owner == nil {
		return all, evaluated
	}
	owner, ok := f.owner.IsClusterOwner()
	if ok {
		members := f.owner.Databases()
		for i := range all {
			if isClusterForecast(all[i].Category) {
				all[i].Detail = clusterDetail(all[i].Detail, owner, members)
			}
		}
		if containsAll(evaluated, clusterForecastCategories) {
			f.owner.Evaluated()
		}
		return all, evaluated
	}
	kept := make([]analyzer.Finding, 0, len(all))
	for _, fd := range all {
		if !isClusterForecast(fd.Category) {
			kept = append(kept, fd)
		}
	}
	cats := make([]string, 0, len(evaluated)+len(clusterForecastCategories))
	for _, c := range evaluated {
		if !isClusterForecast(c) {
			cats = append(cats, c)
		}
	}
	if f.owner.OwnerHealthy(owner) {
		cats = append(cats, clusterForecastCategories...)
	}
	return kept, cats
}

// clusterDetail adds the owner's scope to a cluster forecast's detail.
func clusterDetail(detail map[string]any, owner string, members []string) map[string]any {
	out := make(map[string]any, len(detail)+3)
	for k, v := range detail {
		out[k] = v
	}
	out["scope"], out["owner_database"] = "cluster", owner
	out["cluster_databases"] = append([]string(nil), members...)
	return out
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

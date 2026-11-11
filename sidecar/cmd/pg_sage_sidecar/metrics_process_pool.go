package main

import (
	"sort"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// withUnrefreshedInstances adds every fleet instance without a refresher
// as a database whose value read failed. Such an instance failed to
// connect or prepare, so no runtime reads it; /metrics keeps reporting it
// as pg_sage_value_metrics_up 0 instead of silently dropping it.
func withUnrefreshedInstances(views []metricsView, mgr *fleet.DatabaseManager) []metricsView {
	if mgr == nil {
		return views
	}
	known := make(map[string]bool, len(views))
	for _, v := range views {
		known[v.name] = true
	}
	out := append([]metricsView(nil), views...)
	for name := range mgr.Instances() {
		if known[name] {
			continue
		}
		out = append(out, metricsView{name: name, sources: []string{metricsSourceValue},
			snap: &metricsSnapshot{ok: map[string]bool{metricsSourceValue: false}}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

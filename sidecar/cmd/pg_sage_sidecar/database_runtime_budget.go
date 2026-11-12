package main

import (
	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

// newBudgetMonitor builds the workload budget detector (market-08): it
// reads the collector's workload_sessions category, keeps the shadow
// ledger in this database's sage schema and reads roles through bounded
// catalog statements.
func (rt *databaseRuntime) newBudgetMonitor() *budget.Monitor {
	config.RLockForHotReload()
	timeouts := catalogread.FromSafety(rt.cfg.Safety)
	config.RUnlockForHotReload()
	return budget.NewMonitor(rt.cfg, rt.collector,
		budget.NewStore(rt.spec.Pool, rt.spec.Name),
		budget.NewCatalog(rt.spec.Pool, timeouts), rt.spec.Name, logStructuredWrapper)
}

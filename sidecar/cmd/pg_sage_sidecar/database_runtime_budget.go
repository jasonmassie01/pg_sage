package main

import (
	"context"
	"time"

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
	rt.budgetMonitor = budget.NewMonitor(rt.cfg, rt.collector,
		budget.NewStore(rt.spec.Pool, rt.spec.Name),
		budget.NewCatalog(rt.spec.Pool, timeouts), rt.spec.Name, logStructuredWrapper)
	return rt.budgetMonitor
}

// startBudgetCancels counts this database's statement-timeout cancels from
// the cluster's server log for the budget monitor (market-08b). Without a
// log fanout nothing counts them: an enforced timeout then cannot be
// auto-reverted and needs approval.
func (rt *databaseRuntime) startBudgetCancels() {
	if rt.budgetMonitor == nil || rt.logFanout == nil {
		return
	}
	db := rt.spec.Config.Database
	if db == "" {
		ctx, cancel := context.WithTimeout(rt.ctx, 5*time.Second)
		err := rt.spec.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&db)
		cancel()
		if err != nil {
			logWarn(rt.spec.Scope, "db %q: budget cancel counting skipped: %v",
				rt.spec.Name, err)
			return
		}
	}
	sub := rt.logFanout.SubscribeEntries("budget:"+rt.spec.Name, db)
	counter := budget.NewCancelCounter(db, sub, rt.logFanout.Active)
	interval := logwatchPollInterval()
	rt.start(func() {
		defer sub.Stop()
		counter.Run(rt.ctx, interval)
	})
	rt.budgetMonitor.WithCancelCounter(counter)
	rt.note("budget_cancels")
}

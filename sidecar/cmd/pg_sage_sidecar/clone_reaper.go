package main

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The rehearsal clone reaper (migration-07): a crash between a rehearsal's
// create and destroy leaves a ready receipt, and once it expires the
// reaper destroys its clone. It runs at startup and every 10 minutes, on
// the fleet leader only, and acts only on rehearsal receipts of its own
// adapter, scope and deployment.

const cloneReapInterval = 10 * time.Minute

// cloneReapUnresolved is the expired receipts without a provider ref
// already reported, so each is logged once per process.
var cloneReapUnresolved sync.Map // receipt key -> struct{}

// startCloneReaper starts the reaper when a clone adapter is configured.
func startCloneReaper(ctx context.Context, c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) {
	if c == nil {
		return
	}
	adapter, err := configuredMCPCloneProvider(c.Clone)
	if err != nil {
		logError("clone", "rehearsal clone reaper is off: clone.provider %q: %v",
			c.Clone.Provider, err)
		return
	}
	if adapter == nil {
		return
	}
	go every(ctx, cloneReapInterval, func(ctx context.Context) {
		cloneReapPass(ctx, c, mgr, meta, adapter)
	})
	logInfo("clone", "rehearsal clone reaper started (every %s)", cloneReapInterval)
}

// cloneReapPass reaps every receipt database once, on the leader only; it
// returns how many databases it reaped.
func cloneReapPass(ctx context.Context, c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState, adapter clone.Provider) int {
	if !fleetLeaderAllows("rehearsal clone reaper") {
		return 0
	}
	pools := cloneReceiptPools(c, mgr, meta)
	for _, pool := range pools {
		reapRehearsalClones(ctx, c.Clone, adapter, pool)
	}
	return len(pools)
}

// cloneReceiptPools are the databases rehearsal receipts are written to:
// the governance control database, else every connected monitored
// database (each keeps its own receipts).
func cloneReceiptPools(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) []*pgxpool.Pool {
	if control := governanceControlPool(c, mgr, meta); control != nil {
		return []*pgxpool.Pool{control}
	}
	if mgr == nil {
		return nil
	}
	instances := mgr.Instances()
	names := make([]string, 0, len(instances))
	for name, inst := range instances {
		if inst != nil && inst.Pool != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	pools := make([]*pgxpool.Pool, 0, len(names))
	for _, name := range names {
		pools = append(pools, instances[name].Pool)
	}
	return pools
}

// reapRehearsalClones runs one reaper pass on the receipts in pool.
func reapRehearsalClones(ctx context.Context, cloneCfg config.CloneProviderConfig,
	adapter clone.Provider, pool *pgxpool.Pool) clone.ReapReport {
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		logWarn("clone", "rehearsal clone reaper: deployment id: %v", err)
		return clone.ReapReport{}
	}
	report, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: adapter,
		Scope: clone.ReceiptScope{DeploymentID: deployment, Adapter: cloneCfg.Provider,
			Scope: cloneReceiptScope(cloneCfg)},
		Purpose: clone.PurposeRehearsal}.Reap(ctx)
	if err != nil {
		logWarn("clone", "rehearsal clone reaper: %v", err)
		return report
	}
	for _, r := range report.Destroyed {
		logInfo("clone", "destroyed expired rehearsal clone %s (receipt %d, %s)",
			r.ProviderRef(), r.ID, r.Name)
	}
	for id, ferr := range report.Failed {
		logWarn("clone", "expired rehearsal clone of receipt %d not destroyed, retried "+
			"in %s: %v", id, cloneReapInterval, ferr)
	}
	for _, r := range report.Unresolved {
		key := deployment + "/" + r.Name
		if _, seen := cloneReapUnresolved.LoadOrStore(key, struct{}{}); !seen {
			logWarn("clone", "expired rehearsal receipt %d (%s) is %s with no provider "+
				"id: not destroyed; check %s for a clone created at %s", r.ID, r.Name,
				r.Status, r.Scope, r.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
	return report
}

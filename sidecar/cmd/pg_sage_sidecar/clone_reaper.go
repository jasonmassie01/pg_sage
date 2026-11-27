package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The clone reaper (migration-07, lab-05a): a crash between a rehearsal's
// create and destroy leaves a ready receipt, and once it expires the
// reaper destroys its clone; a clone whose holder process stopped
// heartbeating (a killed sidecar) is destroyed before its TTL, whatever
// its purpose. It runs at startup and every 10 minutes, on the fleet
// leader only, and acts only on receipts of its own adapter, scope and
// deployment.

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
	for _, w := range c.Clone.Warnings() {
		logWarn("clone", "%s", w)
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
	// The reaper keeps the clone config it started with, as the MCP
	// executor does (mcpDependencies): its adapter and its receipt scope
	// must name the same endpoint as the receipts the rehearsals write, and
	// a hot reload of clone.* must not race the pass or split the two.
	cloneCfg := c.Clone
	go every(ctx, cloneReapInterval, func(ctx context.Context) {
		cloneReapPass(ctx, c, cloneCfg, mgr, meta, adapter)
	})
	logInfo("clone", "rehearsal clone reaper started (every %s)", cloneReapInterval)
}

// cloneReapPass reaps every receipt database once, on the leader only, with
// adapter and the clone config it was built from; it returns how many
// databases it reaped.
func cloneReapPass(ctx context.Context, c *config.Config, cloneCfg config.CloneProviderConfig,
	mgr *fleet.DatabaseManager, meta *metaDBState, adapter clone.Provider) int {
	if !fleetLeaderAllows("rehearsal clone reaper") {
		return 0
	}
	pools := cloneReceiptPools(c, mgr, meta)
	for _, pool := range pools {
		reapRehearsalClones(ctx, cloneCfg, adapter, pool)
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

// cloneReapPurposes are the receipt purposes whose expired receipts the
// reaper destroys: index rehearsals and restore drills (drill-01a). Both
// use clone.provider, so one adapter reaps both.
var cloneReapPurposes = []string{clone.PurposeRehearsal, clone.PurposeDrill}

// reapRehearsalClones runs one reaper pass per purpose on the receipts in
// pool and returns the merged report.
func reapRehearsalClones(ctx context.Context, cloneCfg config.CloneProviderConfig,
	adapter clone.Provider, pool *pgxpool.Pool) clone.ReapReport {
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		logWarn("clone", "clone reaper: deployment id: %v", err)
		return clone.ReapReport{}
	}
	merged := clone.ReapReport{Failed: map[int64]error{}, FindFailed: map[int64]error{}}
	for _, purpose := range cloneReapPurposes {
		report, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: adapter,
			Scope: clone.ReceiptScope{DeploymentID: deployment, Adapter: cloneCfg.Provider,
				Scope: cloneReceiptScope(cloneCfg)},
			Purpose: purpose, Holder: clone.ProcessHolder()}.Reap(ctx)
		if err != nil {
			logWarn("clone", "%s clone reaper: %v", purpose, err)
		}
		mergeReapReport(&merged, report)
	}
	logReapReport(deployment, merged)
	return merged
}

func mergeReapReport(into *clone.ReapReport, r clone.ReapReport) {
	into.Destroyed = append(into.Destroyed, r.Destroyed...)
	into.Unresolved = append(into.Unresolved, r.Unresolved...)
	for id, err := range r.Failed {
		into.Failed[id] = err
	}
	for id, err := range r.FindFailed {
		into.FindFailed[id] = err
	}
}

// logReapReport logs one pass; unresolved receipts once per process.
func logReapReport(deployment string, report clone.ReapReport) {
	for _, r := range report.Destroyed {
		logInfo("clone", "destroyed left-behind %s clone %s (receipt %d, %s, holder %q)",
			r.Purpose, r.ProviderRef(), r.ID, r.Name, r.HolderID)
	}
	for id, ferr := range report.Failed {
		logWarn("clone", "left-behind clone of receipt %d not destroyed, retried in %s: %v",
			id, cloneReapInterval, ferr)
	}
	for id, ferr := range report.FindFailed {
		if _, seen := cloneReapUnresolved.LoadOrStore(fmt.Sprintf("%s/find/%d", deployment,
			id), struct{}{}); !seen {
			logWarn("clone", "clone receipt %d not resolved by name, retried in %s: %v", id,
				cloneReapInterval, ferr)
		}
	}
	for _, r := range report.Unresolved {
		key := deployment + "/" + r.Name
		if _, seen := cloneReapUnresolved.LoadOrStore(key, struct{}{}); !seen {
			logWarn("clone", "expired %s receipt %d (%s) is %s with no provider "+
				"id: not destroyed; check %s for a clone created at %s", r.Purpose, r.ID,
				r.Name, r.Status, r.Scope, r.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
}

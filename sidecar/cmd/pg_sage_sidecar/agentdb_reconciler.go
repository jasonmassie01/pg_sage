package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentdb"
)

// startAgentDBReconciler launches the periodic agent-DB lifecycle
// reconciler. interval <= 0 disables it.
func startAgentDBReconciler(ctx context.Context, pool *pgxpool.Pool) {
	interval := cfg.AgentDB.ReconcileIntervalSeconds
	if interval <= 0 {
		logInfo("agentdb", "lifecycle reconciler disabled (interval<=0)")
		return
	}
	store := agentdb.NewStore(pool)
	store.SetRequireBackupBeforeDestroy(cfg.AgentDB.RequireBackupBeforeDrop)
	store.SetMutationGate(func(c context.Context) error {
		return agentDBMutationGate(fleetMgr)(c) // read fleetMgr at call time
	})
	registry := agentdb.RuntimeRunnerRegistryFromEnv(ctx)
	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcileAgentDBsOnce(ctx, store, registry)
			}
		}
	}()
	logInfo("agentdb",
		"lifecycle reconciler started, interval=%ds", interval)
}

// reconcileAgentDBsOnce runs one reconcile pass (extracted for testing).
func reconcileAgentDBsOnce(
	ctx context.Context,
	store *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := store.ReconcileAbandonedDeployments(
		rctx, time.Now(), registry)
	if err != nil {
		logWarn("agentdb", "reconcile abandoned: %v", err)
	} else if len(res.Archived) > 0 || len(res.DestroyLive) > 0 ||
		len(res.DestroyDryRun) > 0 || len(res.Blocked) > 0 {
		logInfo("agentdb",
			"reconcile: archived=%d destroyed=%d dry_run=%d blocked=%d",
			len(res.Archived), len(res.DestroyLive),
			len(res.DestroyDryRun), len(res.Blocked))
	}
	if _, err := store.ReconcileLiveProvisioning(rctx, registry); err != nil {
		logWarn("agentdb", "reconcile live provisioning: %v", err)
	}
	// Bring newly-provisioned agent databases into the fleet so the
	// collector monitors them (B1).
	if fleetMgr != nil {
		syncAgentDBsToFleet(rctx, store, fleetMgr)
	}
}

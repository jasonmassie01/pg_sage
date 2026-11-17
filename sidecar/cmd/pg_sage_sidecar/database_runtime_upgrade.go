package main

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// newUpgradeMonitor is the analyzer's upgrade readiness detector
// (market-05): once a day, for a member whose major is where AP-10
// reports end of life, it checks an upgrade to the newest major pg_sage
// knows and opens advisory upgrade_readiness findings.
func (rt *databaseRuntime) newUpgradeMonitor() *upgradecheck.Monitor {
	return upgradecheck.NewMonitor(rt.spec.Pool, upgradecheck.MonitorOptions{
		Provider: func() string {
			config.RLockForHotReload()
			defer config.RUnlockForHotReload()
			if rt.cfg == nil {
				return ""
			}
			return rt.cfg.CloudEnvironment
		},
	})
}

// upgradeReadinessBudget bounds one MCP upgrade_readiness run.
const upgradeReadinessBudget = 30 * time.Second

// upgradeReadinessMCP serves MCP upgrade_readiness on the database the
// server resolved, read-only; target_database must be a fleet member.
type upgradeReadinessMCP struct {
	manager *fleet.DatabaseManager
}

var _ mcp.UpgradeReadinessBackend = upgradeReadinessMCP{}

func (u upgradeReadinessMCP) UpgradeReadiness(ctx context.Context,
	req mcp.UpgradeReadinessRequest) (any, error) {
	if u.manager == nil {
		return nil, upgradecheck.ErrNoPool
	}
	name, _ := mcp.DatabaseFromContext(ctx)
	pool := u.manager.PoolForDatabase(name)
	if pool == nil {
		return nil, fmt.Errorf("database %q has no connection pool", name)
	}
	opts := upgradecheck.Options{TargetDatabase: req.TargetDatabase}
	if req.TargetDatabase != "" {
		if u.manager.GetInstance(req.TargetDatabase) == nil {
			return nil, fmt.Errorf("%w: target_database %q is not a monitored database",
				upgradecheck.ErrInvalidTarget, req.TargetDatabase)
		}
		if opts.TargetDB = u.manager.PoolForDatabase(req.TargetDatabase); opts.TargetDB == nil {
			return nil, fmt.Errorf("target_database %q has no connection pool",
				req.TargetDatabase)
		}
	}
	target := upgradecheck.Target{Major: req.TargetMajor, Method: req.Method,
		Provider: u.provider(name)}
	ctx, cancel := context.WithTimeout(ctx, upgradeReadinessBudget)
	defer cancel()
	if req.Brief {
		return api.UpgradeBriefMCP(ctx, cfg, name, pool, target, opts) // drill-02a
	}
	return upgradecheck.Run(ctx, pool, target, opts)
}

// provider is the member's detected provider (each fleet member has its
// own); the process's cloud environment when the member reports none.
func (u upgradeReadinessMCP) provider(name string) string {
	if inst := u.manager.GetInstance(name); inst != nil {
		if p := inst.SnapshotStatus().Platform; p != "" {
			return p
		}
	}
	if cfg == nil {
		return ""
	}
	config.RLockForHotReload()
	defer config.RUnlockForHotReload()
	return cfg.CloudEnvironment
}

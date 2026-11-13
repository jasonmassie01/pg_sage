package main

import (
	"github.com/pg-sage/sidecar/internal/config"
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

package main

import (
	"slices"

	"github.com/pg-sage/sidecar/internal/agentposture"
	"github.com/pg-sage/sidecar/internal/config"
)

// Agent posture (G0, spec §6.15) wiring: the first look runs the "Agent
// posture" section on its own (firstLookOptions), and the analyzer runs
// the posture monitor, which checks again when the catalog posture reads
// changes and daily. Posture only reports: every fix is a manual script.

// postureConfig maps the agents.* keys; nil is the shipped defaults. The
// slices are copied, so a hot reload never races a running check.
func postureConfig(c *config.Config) agentposture.Config {
	if c == nil {
		return agentposture.DefaultConfig()
	}
	a := c.Agents
	return agentposture.Config{ExposedRoles: slices.Clone(a.ExposedRoles),
		ClientPatterns:    slices.Clone(a.ClientPatterns),
		MemoryGrowthGBDay: a.Posture.MemoryGrowthGBDay,
		DailyAt:           a.Posture.DailyAt,
		Platform:          agentposture.Platform{Provider: c.CloudEnvironment}}
}

// newPostureMonitor is the analyzer's posture detector; it reads the live
// agents.* keys on every cycle.
func (rt *databaseRuntime) newPostureMonitor() *agentposture.Monitor {
	return agentposture.NewMonitor(rt.spec.Pool, agentposture.MonitorOptions{
		Config: func() agentposture.Config {
			config.RLockForHotReload()
			defer config.RUnlockForHotReload()
			cfg := postureConfig(rt.cfg)
			cfg.Platform.Backup = rt.postureBackup()
			return cfg
		},
		Logf: logStructuredWrapper,
	})
}

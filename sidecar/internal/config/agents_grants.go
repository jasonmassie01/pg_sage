package config

import (
	"fmt"
	"time"
)

// AgentsCapabilitiesConfig bounds agent grants (AGENTDB-SPEC §6.6, §9).
type AgentsCapabilitiesConfig struct {
	MaxDurationMinutes int `yaml:"max_duration_minutes" doc:"Longest agent grant, in minutes; every grant expires and the reconciler revokes it. 1-10080. Default: 240."`
}

// Agent grant defaults (spec §9).
const (
	DefaultAgentGrantMaxMinutes       = 240
	DefaultAgentReconcileIntervalSecs = 60
	maxAgentGrantMinutes              = 7 * 24 * 60
)

// MaxDuration is the longest grant.
func (c AgentsCapabilitiesConfig) MaxDuration() time.Duration {
	return time.Duration(c.MaxDurationMinutes) * time.Minute
}

// ReconcileInterval is agents.reconcile_interval_seconds as a duration.
func (a AgentsConfig) ReconcileInterval() time.Duration {
	return time.Duration(a.ReconcileIntervalSeconds) * time.Second
}

// validateGrants checks the grant bounds and the reconcile interval.
func (a AgentsConfig) validateGrants() error {
	if m := a.Capabilities.MaxDurationMinutes; m < 1 || m > maxAgentGrantMinutes {
		return fmt.Errorf("agents.capabilities.max_duration_minutes must be 1-%d, got %d",
			maxAgentGrantMinutes, m)
	}
	if s := a.ReconcileIntervalSeconds; s < 10 || s > 3600 {
		return fmt.Errorf("agents.reconcile_interval_seconds must be 10-3600, got %d", s)
	}
	return nil
}

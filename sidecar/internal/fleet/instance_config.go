package fleet

import (
	"github.com/pg-sage/sidecar/internal/config"
)

// ConfigSnapshot returns a copy of the instance's per-database config, read
// under configMu. A SIGHUP reload or a dashboard edit swaps Config in place,
// so code that does not hold the manager lock must read it here. The slices
// are copied so the caller never shares a backing array with the instance;
// nil and empty stay distinct (an empty DisabledRules re-enables every
// rule, nil falls back to the global list).
func (d *DatabaseInstance) ConfigSnapshot() config.DatabaseConfig {
	if d == nil {
		return config.DatabaseConfig{}
	}
	d.configMu.RLock()
	cfg := d.Config
	d.configMu.RUnlock()
	cfg.Tags = cloneTags(cfg.Tags)
	cfg.DisabledRules = cloneTags(cfg.DisabledRules)
	if cfg.Replicas != nil {
		cfg.Replicas = append([]config.DatabaseReplica{}, cfg.Replicas...)
	}
	return cfg
}

// setConfig swaps the per-database config. Callers hold the manager lock,
// so readers under m.mu stay consistent without configMu.
func (d *DatabaseInstance) setConfig(cfg config.DatabaseConfig) {
	d.configMu.Lock()
	d.Config = cfg
	d.configMu.Unlock()
}

package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// collection-08: the host-CPU admission ceiling is applied like its
// sibling, and never rewrites the session-load ceiling.
func TestHotReloadHostCPUCeiling(t *testing.T) {
	cfg := &config.Config{Safety: config.SafetyConfig{CPUCeilingPct: 70}}
	hotReload(cfg, "safety.host_cpu_ceiling_pct", "75")
	if cfg.Safety.HostCPUCeilingPct != 75 || cfg.Safety.CPUCeilingPct != 70 {
		t.Errorf("host/session = %d/%d, want 75/70", cfg.Safety.HostCPUCeilingPct,
			cfg.Safety.CPUCeilingPct)
	}
	if got := cfg.Safety.EffectiveHostCPUCeilingPct(); got != 75 {
		t.Errorf("effective host ceiling = %d, want 75", got)
	}
}

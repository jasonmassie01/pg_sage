package config

import "fmt"

// EffectiveHostCPUCeilingPct is the database-host CPU ceiling index
// verification admits changes under: safety.host_cpu_ceiling_pct, or
// cpu_ceiling_pct while it is unset (the one key used to tune both, so no
// verify guardrail loosens on upgrade). 0 means neither is configured.
func (s *SafetyConfig) EffectiveHostCPUCeilingPct() int {
	if s.HostCPUCeilingPct > 0 {
		return s.HostCPUCeilingPct
	}
	return s.CPUCeilingPct
}

// validateHostCPUCeiling accepts unset (0) or a percent 1-100.
func (s *SafetyConfig) validateHostCPUCeiling() error {
	if s.HostCPUCeilingPct < 0 || s.HostCPUCeilingPct > 100 {
		return fmt.Errorf("safety.host_cpu_ceiling_pct must be 1-100 "+
			"(or unset to inherit cpu_ceiling_pct), got %d", s.HostCPUCeilingPct)
	}
	return nil
}

// warnInheritedHostCPUCeiling says once, at load, when verify's host CPU
// ceiling comes from a non-default cpu_ceiling_pct, the collector's
// session-load ceiling.
func (s *SafetyConfig) warnInheritedHostCPUCeiling() {
	if s.HostCPUCeilingPct != 0 || s.CPUCeilingPct == DefaultCPUCeilingPct {
		return
	}
	_, _ = fmt.Fprintf(configWarningOutput, "WARN: safety.host_cpu_ceiling_pct is unset, "+
		"so index verification inherits safety.cpu_ceiling_pct=%d as its host CPU "+
		"ceiling; cpu_ceiling_pct is the collector's active sessions / "+
		"max_connections ceiling. Set safety.host_cpu_ceiling_pct to tune them "+
		"separately.\n", s.CPUCeilingPct)
}

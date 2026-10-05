package config

import (
	"fmt"
	"time"
)

// SelfConfigConfig is the operator's switch for derived settings (roadmap
// phase 3): derivable keys the operator leaves unset are derived per
// database from evidence, recorded in shadow first, and promoted after
// the soak only when the comparison is not worse. Both keys are
// safety-critical: derivation never tunes its own switch or soak.
type SelfConfigConfig struct {
	Enabled   bool `yaml:"enabled" doc:"Derive unset derivable settings (intervals, timeouts, thresholds) per database from evidence, within bounds that never widen authority or spend. New values soak in shadow first. Default: true."`
	SoakHours int  `yaml:"soak_hours" doc:"Hours a newly derived value stays in shadow, compared against the active value's measured outcomes, before it may be promoted. 1-720. Default: 24."`
}

const (
	// DefaultSelfConfigEnabled: shadow-first derivation is on by default.
	DefaultSelfConfigEnabled = true
	// DefaultSelfConfigSoakHours is the shadow soak of a derived value.
	DefaultSelfConfigSoakHours = 24
	maxSelfConfigSoakHours     = 720
)

func defaultSelfConfigConfig() SelfConfigConfig {
	return SelfConfigConfig{Enabled: DefaultSelfConfigEnabled,
		SoakHours: DefaultSelfConfigSoakHours}
}

// Soak is how long a derived value stays in shadow before promotion.
func (s SelfConfigConfig) Soak() time.Duration {
	return time.Duration(s.SoakHours) * time.Hour
}

func (s SelfConfigConfig) validate() error {
	if s.SoakHours < 1 || s.SoakHours > maxSelfConfigSoakHours {
		return fmt.Errorf("self_config.soak_hours must be 1-%d, got %d",
			maxSelfConfigSoakHours, s.SoakHours)
	}
	return nil
}

// Validate runs the load-time validation on a complete configuration, for
// callers that build one (a derived-settings candidate) outside Load.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	return c.validate()
}

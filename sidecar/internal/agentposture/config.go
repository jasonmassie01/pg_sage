package agentposture

import (
	"fmt"
	"regexp"

	"github.com/pg-sage/sidecar/internal/config"
)

// Config is what posture checks against: the agents.* keys (spec §9).
type Config struct {
	// ExposedRoles are roles untrusted clients reach, besides PUBLIC.
	ExposedRoles []string
	// ClientPatterns are anchored, case-insensitive regular expressions
	// over application_name that hint a session is an agent.
	ClientPatterns []string
	// MemoryGrowthGBDay is the agent memory-store growth posture reports.
	MemoryGrowthGBDay float64
	// DailyAt is the local HH:MM of the daily posture run.
	DailyAt string
}

// DefaultConfig is the shipped configuration.
func DefaultConfig() Config {
	return Config{ClientPatterns: config.DefaultClientPatterns(),
		MemoryGrowthGBDay: config.DefaultPostureMemoryGrowthGBDay,
		DailyAt:           config.DefaultPostureDailyAt}
}

// Validate applies the agents.* load-time rules; errors are ErrInvalidConfig.
func (c Config) Validate() error {
	for _, r := range c.ExposedRoles {
		if err := config.ValidExposedRole(r); err != nil {
			return fmt.Errorf("%w: agents.exposed_roles: %v", ErrInvalidConfig, err)
		}
	}
	for _, p := range c.ClientPatterns {
		if err := config.ValidClientPattern(p); err != nil {
			return fmt.Errorf("%w: agents.client_patterns: %v", ErrInvalidConfig, err)
		}
	}
	if !(c.MemoryGrowthGBDay > 0) {
		return fmt.Errorf("%w: agents.posture.memory_growth_gb_day must be greater than 0",
			ErrInvalidConfig)
	}
	if _, _, err := config.ParseDailyAt(c.DailyAt); err != nil {
		return fmt.Errorf("%w: agents.posture.daily_at: %v", ErrInvalidConfig, err)
	}
	return nil
}

// clientMatcher compiles the client patterns (case-insensitive); an
// invalid pattern is skipped (Validate reports it).
func (c Config) clientMatcher() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(c.ClientPatterns))
	for _, p := range c.ClientPatterns {
		if config.ValidClientPattern(p) != nil {
			continue
		}
		out = append(out, regexp.MustCompile("(?i)"+p))
	}
	return out
}

// MatchClient reports whether application_name app hints at an agent.
func (c Config) MatchClient(app string) bool {
	return matchAny(c.clientMatcher(), app)
}

func matchAny(res []*regexp.Regexp, app string) bool {
	if app == "" {
		return false
	}
	for _, re := range res {
		if re.MatchString(app) {
			return true
		}
	}
	return false
}

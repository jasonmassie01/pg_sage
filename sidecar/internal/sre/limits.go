package sre

import (
	"fmt"
	"time"
)

// R1 hard ceilings (Codex §8; mirrored by CHECK constraints in the
// sage.sre_* schema). Configuration may tighten them, never raise them.
const (
	CeilingActive       = 120 * time.Second
	CeilingProbes       = 12
	CeilingModelTurns   = 2
	CeilingInputTokens  = 16000
	CeilingOutputTokens = 4000
)

// Limits bound one investigation and the durable daily model budget.
type Limits struct {
	MaxActive       time.Duration
	MaxProbes       int
	MaxModelTurns   int
	MaxInputTokens  int64
	MaxOutputTokens int64
	LeaseTTL        time.Duration
	QueueExpiry     time.Duration
	// DatabaseDailyTokens and DeploymentDailyTokens are durable per-UTC-day
	// model allocations. Zero means no allocation: model use is refused
	// (deterministic investigation still works). Never "unlimited".
	DatabaseDailyTokens   int64
	DeploymentDailyTokens int64
}

// DefaultLimits are the R1 ceilings with no daily model allocation.
func DefaultLimits() Limits {
	return Limits{MaxActive: CeilingActive, MaxProbes: CeilingProbes,
		MaxModelTurns: CeilingModelTurns, MaxInputTokens: CeilingInputTokens,
		MaxOutputTokens: CeilingOutputTokens, LeaseTTL: 30 * time.Second,
		QueueExpiry: 10 * time.Minute}
}

// Validate rejects zero, negative and above-ceiling values.
func (l Limits) Validate() error {
	checks := []struct {
		name    string
		ok      bool
		problem string
	}{
		{"max active", l.MaxActive > 0 && l.MaxActive <= CeilingActive, "in (0, 120s]"},
		{"max probes", l.MaxProbes > 0 && l.MaxProbes <= CeilingProbes, "in [1, 12]"},
		{"max model turns", l.MaxModelTurns > 0 && l.MaxModelTurns <= CeilingModelTurns,
			"in [1, 2]"},
		{"max input tokens", l.MaxInputTokens > 0 && l.MaxInputTokens <= CeilingInputTokens,
			"in [1, 16000]"},
		{"max output tokens", l.MaxOutputTokens > 0 &&
			l.MaxOutputTokens <= CeilingOutputTokens, "in [1, 4000]"},
		{"lease ttl", l.LeaseTTL > 0 && l.LeaseTTL <= l.MaxActive, "in (0, max active]"},
		{"queue expiry", l.QueueExpiry > 0, "positive"},
		{"database daily tokens", l.DatabaseDailyTokens >= 0, "not negative"},
		{"deployment daily tokens", l.DeploymentDailyTokens >= 0 &&
			l.DatabaseDailyTokens <= l.DeploymentDailyTokens, "at least the database's"},
	}
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("%w: %s must be %s", ErrInvalidRequest, c.name, c.problem)
		}
	}
	return nil
}

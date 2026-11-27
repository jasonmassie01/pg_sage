package config

import "fmt"

// DrillConfig is the fleet-wide restore drill setting (drill-01a). Each
// database's drill (on or off, cadence, kinds, budget, marker) is in its
// standing policy document's drill block.
type DrillConfig struct {
	MaxConcurrentPerFleet int `yaml:"max_concurrent_per_fleet" doc:"Most restore drills the fleet leader runs at once, across every database, 1-10, on top of clone.max_creates_per_hour. Each database runs one drill at a time. Default: 2."`
	// Command is the self-managed restore command (drill_command.go, drill-01c).
	Command DrillCommandConfig `yaml:"command"`
}

// DefaultDrillMaxConcurrentPerFleet is drill.max_concurrent_per_fleet.
const DefaultDrillMaxConcurrentPerFleet = 2

// maxDrillConcurrentPerFleet bounds drill.max_concurrent_per_fleet.
const maxDrillConcurrentPerFleet = 10

func defaultDrillConfig() DrillConfig {
	return DrillConfig{MaxConcurrentPerFleet: DefaultDrillMaxConcurrentPerFleet}
}

// MaxConcurrent is the fleet cap; an unset (zero) section is the default.
func (d DrillConfig) MaxConcurrent() int {
	if d.MaxConcurrentPerFleet <= 0 {
		return DefaultDrillMaxConcurrentPerFleet
	}
	return d.MaxConcurrentPerFleet
}

func (d DrillConfig) validate() error {
	if d.MaxConcurrentPerFleet < 1 || d.MaxConcurrentPerFleet > maxDrillConcurrentPerFleet {
		return fmt.Errorf("drill.max_concurrent_per_fleet must be 1-%d, got %d",
			maxDrillConcurrentPerFleet, d.MaxConcurrentPerFleet)
	}
	return nil
}

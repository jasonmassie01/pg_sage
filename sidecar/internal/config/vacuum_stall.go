package config

import (
	"fmt"
	"time"
)

// The VACUUM watchdog's stall window (vacuum-05). An executor VACUUM runs
// with statement_timeout 0; its watchdog cancels it when a heap phase
// reports no progress for this long, so a VACUUM stuck behind a buffer pin
// or a lock does not hold its session until vacuum_timeout_seconds. It is
// never disabled.
const (
	DefaultVacuumStallMinutes = 15
	MinVacuumStallMinutes     = 5
	MaxVacuumStallMinutes     = 240
)

// VacuumStall is how long a heap phase may make no progress; unset (0) is
// the default.
func (c *SafetyConfig) VacuumStall() time.Duration {
	minutes := c.VacuumStallMinutes
	if minutes <= 0 {
		minutes = DefaultVacuumStallMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// validateVacuumStall accepts 0 (the default) or 5-240 minutes.
func (c *SafetyConfig) validateVacuumStall() error {
	m := c.VacuumStallMinutes
	if m == 0 || (m >= MinVacuumStallMinutes && m <= MaxVacuumStallMinutes) {
		return nil
	}
	return fmt.Errorf("safety.vacuum_stall_minutes must be %d-%d minutes (0 uses the "+
		"default %d); got %d", MinVacuumStallMinutes, MaxVacuumStallMinutes,
		DefaultVacuumStallMinutes, m)
}

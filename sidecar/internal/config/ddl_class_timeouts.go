package config

import (
	"fmt"
	"time"
)

// Per-class statement timeouts (executor-05). A CONCURRENTLY index build,
// a REINDEX CONCURRENTLY and a VACUUM can run for hours on a large table
// without blocking writes, so one 300 s ddl_timeout_seconds either killed
// them or had to be raised for every other DDL too. Each class defaults to
// four hours and is bounded: never unlimited, never above one day.
const (
	DefaultClassTimeoutSeconds = 14400
	MinClassTimeoutSeconds     = 60
	MaxClassTimeoutSeconds     = 86400
)

// IndexBuildTimeout bounds a CREATE INDEX CONCURRENTLY; unset (0) is the
// default.
func (c *SafetyConfig) IndexBuildTimeout() time.Duration {
	return classTimeout(c.IndexBuildTimeoutSeconds)
}

// ReindexTimeout bounds a REINDEX CONCURRENTLY; unset (0) is the default.
func (c *SafetyConfig) ReindexTimeout() time.Duration {
	return classTimeout(c.ReindexTimeoutSeconds)
}

// VacuumTimeout bounds a plain VACUUM; unset (0) is the default.
func (c *SafetyConfig) VacuumTimeout() time.Duration {
	return classTimeout(c.VacuumTimeoutSeconds)
}

func classTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = DefaultClassTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// validateClassTimeouts accepts 0 (the default) or 60-86400 seconds for
// each class timeout, then the VACUUM watchdog's stall window.
func (c *SafetyConfig) validateClassTimeouts() error {
	for _, field := range []struct {
		key     string
		seconds int
	}{
		{"safety.index_build_timeout_seconds", c.IndexBuildTimeoutSeconds},
		{"safety.reindex_timeout_seconds", c.ReindexTimeoutSeconds},
		{"safety.vacuum_timeout_seconds", c.VacuumTimeoutSeconds},
	} {
		if field.seconds == 0 {
			continue
		}
		if field.seconds < MinClassTimeoutSeconds || field.seconds > MaxClassTimeoutSeconds {
			return fmt.Errorf("%s must be %d-%d seconds (0 uses the default %d); got %d",
				field.key, MinClassTimeoutSeconds, MaxClassTimeoutSeconds,
				DefaultClassTimeoutSeconds, field.seconds)
		}
	}
	return c.validateVacuumStall()
}

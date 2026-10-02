package config

import (
	"fmt"
	"time"
)

// SREDetectorsConfig holds the thresholds of the Sage SRE reactive
// detector (M6): the deterministic trigger for checkpoint storms,
// temp-file explosions and LWLock contention. It samples the database
// once per trigger poll while sre.automatic_start is on and opens one
// incident and investigation per episode. Defaults are conservative.
type SREDetectorsConfig struct {
	WindowSeconds       int `yaml:"window_seconds" doc:"Seconds over which checkpoint and temp-file growth is measured, 60-3600. Keep it at least twice sre.trigger_interval_seconds. Default: 300."`
	CheckpointRequested int `yaml:"checkpoint_requested" doc:"Requested checkpoints within the window that are a checkpoint storm (they must also outnumber timed ones), 1-1000. Default: 3."`
	TempFileMB          int `yaml:"temp_file_mb" doc:"MiB of temp files this database writes within the window that are a temp-file explosion, 1-1048576. Default: 1024."`
	LWLockWaiters       int `yaml:"lwlock_waiters" doc:"Backends waiting on one modeled LWLock class that count as contention, 1-10000. Default: 8."`
	LWLockPolls         int `yaml:"lwlock_polls" doc:"Consecutive trigger polls lwlock_waiters must hold before an LWLock contention episode opens, 1-100. Default: 3."`
	CooldownMinutes     int `yaml:"cooldown_minutes" doc:"Minutes after an episode starts before a new episode of the same family can open, 1-1440. Default: 30."`
}

func defaultSREDetectorsConfig() SREDetectorsConfig {
	return SREDetectorsConfig{WindowSeconds: 300, CheckpointRequested: 3, TempFileMB: 1024,
		LWLockWaiters: 8, LWLockPolls: 3, CooldownMinutes: 30}
}

// Window is how far back counter growth is measured.
func (d SREDetectorsConfig) Window() time.Duration {
	return time.Duration(d.WindowSeconds) * time.Second
}

// TempBytes is the temp-file growth threshold in bytes.
func (d SREDetectorsConfig) TempBytes() int64 { return int64(d.TempFileMB) << 20 }

// Cooldown spaces two episodes of one family.
func (d SREDetectorsConfig) Cooldown() time.Duration {
	return time.Duration(d.CooldownMinutes) * time.Minute
}

func (d SREDetectorsConfig) validate() error {
	for _, c := range []rangeCheck{
		{"window_seconds", d.WindowSeconds, 60, 3600},
		{"checkpoint_requested", d.CheckpointRequested, 1, 1000},
		{"temp_file_mb", d.TempFileMB, 1, 1 << 20},
		{"lwlock_waiters", d.LWLockWaiters, 1, 10000},
		{"lwlock_polls", d.LWLockPolls, 1, 100},
		{"cooldown_minutes", d.CooldownMinutes, 1, 1440},
	} {
		if c.value < c.lo || c.value > c.hi {
			return fmt.Errorf("sre.detectors.%s must be %d-%d, got %d", c.key, c.lo, c.hi,
				c.value)
		}
	}
	return nil
}

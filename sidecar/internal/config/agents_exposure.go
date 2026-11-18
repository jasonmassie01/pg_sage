package config

import (
	"fmt"
	"time"
)

// AgentsExposureConfig tunes the exposure sweep (fleetpol-02a): between
// analyzer cycles, pg_sage reads the catalog fingerprint this often and,
// when it changed, runs the exposure detectors at once.
type AgentsExposureConfig struct {
	SweepSeconds int `yaml:"sweep_seconds" doc:"Seconds between exposure sweeps: each reads the catalog fingerprint and, when it changed, re-runs the exposure checks (AP-01 to AP-05, AP-15, AP-17) outside the analyzer cycle. 15-600. Default: 60."`
}

// Exposure sweep bounds (fleetpol-02a).
const (
	DefaultExposureSweepSeconds = 60
	MinExposureSweepSeconds     = 15
	MaxExposureSweepSeconds     = 600
)

// SweepInterval is agents.exposure.sweep_seconds as a duration.
func (e AgentsExposureConfig) SweepInterval() time.Duration {
	return time.Duration(e.SweepSeconds) * time.Second
}

func (e AgentsExposureConfig) validate() error {
	if s := e.SweepSeconds; s < MinExposureSweepSeconds || s > MaxExposureSweepSeconds {
		return fmt.Errorf("agents.exposure.sweep_seconds must be %d-%d, got %d",
			MinExposureSweepSeconds, MaxExposureSweepSeconds, s)
	}
	return nil
}

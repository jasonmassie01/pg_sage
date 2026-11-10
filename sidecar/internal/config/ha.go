package config

import (
	"fmt"
	"time"
)

// HAConfig configures each database's one HA monitor (fleet-04): the
// role and failover check every consumer (executor gate, custodian
// proposals, runway advisor, earned autonomy) reads.
type HAConfig struct {
	CheckIntervalSeconds int `yaml:"check_interval_seconds" doc:"Seconds between HA role checks of each database, at most the executor cycle interval. A check older than three intervals reads as an unknown role (actions blocked). 5-600. Default: 60."`
}

// DefaultHACheckIntervalSeconds is the HA check cadence.
const DefaultHACheckIntervalSeconds = 60

// HA check interval bounds.
const (
	minHACheckIntervalSeconds = 5
	maxHACheckIntervalSeconds = 600
)

func defaultHAConfig() HAConfig {
	return HAConfig{CheckIntervalSeconds: DefaultHACheckIntervalSeconds}
}

// CheckInterval is the HA check cadence; an unset (zero-value) section
// is the default, never "no checks".
func (h HAConfig) CheckInterval() time.Duration {
	if h.CheckIntervalSeconds <= 0 {
		return DefaultHACheckIntervalSeconds * time.Second
	}
	return time.Duration(h.CheckIntervalSeconds) * time.Second
}

func (h HAConfig) validate() error {
	if h.CheckIntervalSeconds < minHACheckIntervalSeconds ||
		h.CheckIntervalSeconds > maxHACheckIntervalSeconds {
		return fmt.Errorf("ha.check_interval_seconds must be %d-%d, got %d",
			minHACheckIntervalSeconds, maxHACheckIntervalSeconds, h.CheckIntervalSeconds)
	}
	return nil
}

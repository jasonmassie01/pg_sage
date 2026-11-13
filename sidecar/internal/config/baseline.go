package config

import (
	"fmt"
	"math"
	"time"
)

// BaselineConfig configures the hour-of-week baselines behind the
// workload_anomaly finding (market-02): each cycle's workload is folded
// into one of 168 hour-of-week buckets and scored with a robust z against
// the same bucket in earlier weeks. Detection is deterministic and reads
// only data the collector already has.
type BaselineConfig struct {
	Enabled      bool    `yaml:"enabled" doc:"Score each cycle's workload against the same hour of the week in earlier weeks and raise workload_anomaly when it is far above usual. Default: true."`
	Weeks        int     `yaml:"weeks" doc:"Earlier weeks each hour-of-week bucket is scored against; a bucket with data from fewer than 3 weeks is learning and never fires. sage.baseline_samples keeps weeks + 1 weeks. Range 3-52. Default: 4."`
	Timezone     string  `yaml:"timezone" doc:"IANA time zone the hour-of-week buckets follow (Monday 00:00 is bucket 0), e.g. America/Chicago. An unknown zone fails the config load. Default: UTC."`
	ZWarning     float64 `yaml:"z_warning" doc:"Robust z, (value - bucket median) / (1.4826 x MAD), at or above which two consecutive cycles raise a warning. Above 0. Default: 4."`
	ZCritical    float64 `yaml:"z_critical" doc:"Robust z for a critical anomaly; a statement also needs at least 10% of the interval's DB time. At least z_warning. Default: 6."`
	MaxQueryKeys int     `yaml:"max_query_keys" doc:"Statements per database given a baseline each cycle, the largest interval time first. 0 keeps only the backend, WAL and wait baselines. Range 0-5000. Default: 500."`
	MinDeltaMs   float64 `yaml:"min_delta_ms" doc:"Smallest rise over the bucket median, in ms, a statement's interval mean needs before it can be an anomaly, so quiet buckets do not alarm. 0 or more. Default: 5."`
}

// Baseline defaults (market-02).
const (
	DefaultBaselineWeeks        = 4
	DefaultBaselineTimezone     = "UTC"
	DefaultBaselineZWarning     = 4.0
	DefaultBaselineZCritical    = 6.0
	DefaultBaselineMaxQueryKeys = 500
	DefaultBaselineMinDeltaMs   = 5.0
)

// Baseline bounds.
const (
	minBaselineWeeks      = 3
	maxBaselineWeeks      = 52
	maxBaselineQueryKeys  = 5000
	maxBaselineZ          = 1000
	maxBaselineMinDeltaMs = 3_600_000
)

// DefaultBaseline is the shipped baseline section.
func DefaultBaseline() BaselineConfig {
	return BaselineConfig{Enabled: true, Weeks: DefaultBaselineWeeks,
		Timezone: DefaultBaselineTimezone, ZWarning: DefaultBaselineZWarning,
		ZCritical: DefaultBaselineZCritical, MaxQueryKeys: DefaultBaselineMaxQueryKeys,
		MinDeltaMs: DefaultBaselineMinDeltaMs}
}

// Location is the bucket time zone; UTC when unset or unknown (validate
// refuses an unknown zone at load, so only a literal config reaches it).
func (b BaselineConfig) Location() *time.Location {
	if b.Timezone == "" || b.Timezone == "UTC" {
		return time.UTC
	}
	loc, err := time.LoadLocation(b.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// validate refuses an unknown time zone and values outside their ranges,
// whether or not the section is enabled, so enabling it later cannot
// meet a bad value.
func (b BaselineConfig) validate() error {
	if _, err := time.LoadLocation(b.Timezone); err != nil {
		return fmt.Errorf("%s %q is not a known IANA time zone: %w",
			"baseline.timezone", b.Timezone, err)
	}
	if b.Weeks < minBaselineWeeks || b.Weeks > maxBaselineWeeks {
		return fmt.Errorf("baseline.weeks must be %d-%d, got %d",
			minBaselineWeeks, maxBaselineWeeks, b.Weeks)
	}
	if !(b.ZWarning > 0 && b.ZWarning <= maxBaselineZ) {
		return fmt.Errorf("baseline.z_warning must be above 0 and at most %d, got %v",
			maxBaselineZ, b.ZWarning)
	}
	if !(b.ZCritical >= b.ZWarning && b.ZCritical <= maxBaselineZ) {
		return fmt.Errorf("baseline.z_critical must be at least baseline.z_warning (%v) "+
			"and at most %d, got %v", b.ZWarning, maxBaselineZ, b.ZCritical)
	}
	if b.MaxQueryKeys < 0 || b.MaxQueryKeys > maxBaselineQueryKeys {
		return fmt.Errorf("baseline.max_query_keys must be 0-%d, got %d",
			maxBaselineQueryKeys, b.MaxQueryKeys)
	}
	if math.IsNaN(b.MinDeltaMs) || b.MinDeltaMs < 0 || b.MinDeltaMs > maxBaselineMinDeltaMs {
		return fmt.Errorf("baseline.min_delta_ms must be 0-%d, got %v",
			maxBaselineMinDeltaMs, b.MinDeltaMs)
	}
	return nil
}

package config

import "fmt"

// maxCheckpointRequestedPerHour is one requested checkpoint a second, far
// past any real workload; a larger threshold could never warn.
const maxCheckpointRequestedPerHour = 3600

// validate checks the forecaster keys that have a range (forecast-03,
// forecast-04).
func (f ForecasterConfig) validate() error {
	if f.CacheMinBlocksPerDay < 1 {
		return fmt.Errorf("forecaster.cache_min_blocks_per_day must be at least 1 "+
			"block access a day, got %d", f.CacheMinBlocksPerDay)
	}
	v := f.CheckpointRequestedPerHour
	if !(v > 0 && v <= maxCheckpointRequestedPerHour) { // also rejects NaN
		return fmt.Errorf("forecaster.checkpoint_requested_per_hour must be "+
			"above 0 and at most %d requested checkpoints per hour, got %v",
			maxCheckpointRequestedPerHour, v)
	}
	return nil
}

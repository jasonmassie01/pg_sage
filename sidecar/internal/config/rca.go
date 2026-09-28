package config

import (
	"fmt"
	"time"
)

// Bounds for rca.lock_chain_interval_seconds. 0 disables the fast path.
const (
	minRCALockChainIntervalSeconds = 10
	maxRCALockChainIntervalSeconds = 3600
)

// LockChainInterval is the lock-chain fast-path period; zero means the
// fast path is disabled.
func (r *RCAConfig) LockChainInterval() time.Duration {
	if r.LockChainIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(r.LockChainIntervalSeconds) * time.Second
}

func (r *RCAConfig) validate() error {
	s := r.LockChainIntervalSeconds
	if s == 0 {
		return nil
	}
	if s < minRCALockChainIntervalSeconds || s > maxRCALockChainIntervalSeconds {
		return fmt.Errorf("rca.lock_chain_interval_seconds must be 0 "+
			"(disabled) or %d-%d, got %d", minRCALockChainIntervalSeconds,
			maxRCALockChainIntervalSeconds, s)
	}
	return nil
}

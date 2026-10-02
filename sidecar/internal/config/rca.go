package config

import (
	"fmt"
	"time"
)

// Bounds for rca.lock_chain_interval_seconds. 0 disables the fast path.
const (
	minRCALockChainIntervalSeconds = 10
	maxRCALockChainIntervalSeconds = 3600
	maxRCAStaleAfterHours          = 8760
)

// LockChainInterval is the lock-chain fast-path period; zero means the
// fast path is disabled.
func (r *RCAConfig) LockChainInterval() time.Duration {
	if r.LockChainIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(r.LockChainIntervalSeconds) * time.Second
}

// StaleAfter is how long an open incident may go without being
// re-detected before it resolves as stale. A config built without the
// key (zero) gets the default, never "resolve immediately".
func (r *RCAConfig) StaleAfter() time.Duration {
	if r.StaleAfterHours <= 0 {
		return DefaultRCAStaleAfterHours * time.Hour
	}
	return time.Duration(r.StaleAfterHours) * time.Hour
}

func (r *RCAConfig) validate() error {
	if err := r.validateStaleAfter(); err != nil {
		return err
	}
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

func (r *RCAConfig) validateStaleAfter() error {
	h := r.StaleAfterHours
	if h < 1 || h > maxRCAStaleAfterHours {
		return fmt.Errorf("rca.stale_after_hours must be 1-%d, got %d",
			maxRCAStaleAfterHours, h)
	}
	if h*60 < r.DedupWindowMinutes {
		return fmt.Errorf("rca.stale_after_hours (%d h) must cover "+
			"rca.dedup_window_minutes (%d)", h, r.DedupWindowMinutes)
	}
	return nil
}

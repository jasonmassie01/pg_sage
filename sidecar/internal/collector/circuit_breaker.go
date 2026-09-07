package collector

import (
	"context"
	"sync"
)

// CircuitBreaker prevents collection when the database is under heavy load.
type CircuitBreaker struct {
	mu               sync.Mutex
	consecutiveSkips int
	isDormant        bool
	cpuCeilingPct    int
	maxSkips         int
	successCount     int
}

// NewCircuitBreaker creates a breaker that trips when load_ratio exceeds
// cpuCeilingPct and enters dormant mode after maxSkips consecutive skips.
func NewCircuitBreaker(cpuCeilingPct, maxSkips int) *CircuitBreaker {
	return &CircuitBreaker{
		cpuCeilingPct: cpuCeilingPct,
		maxSkips:      maxSkips,
	}
}

// ShouldSkip checks active_backends / max_connections against the ceiling.
// Returns true if the current cycle should be skipped.
func (cb *CircuitBreaker) ShouldSkip(ctx context.Context, pool catalogQuerier) bool {
	var loadRatio float64
	err := pool.QueryRow(ctx, loadRatioSQL).Scan(&loadRatio)
	if err != nil {
		// If we can't even query load, skip to be safe.
		cb.mu.Lock()
		cb.consecutiveSkips++
		if cb.consecutiveSkips >= cb.maxSkips {
			cb.isDormant = true
		}
		cb.successCount = 0
		cb.mu.Unlock()
		return true
	}

	threshold := float64(cb.cpuCeilingPct) / 100.0

	cb.mu.Lock()
	defer cb.mu.Unlock()

	if loadRatio > threshold {
		cb.consecutiveSkips++
		cb.successCount = 0
		if cb.consecutiveSkips >= cb.maxSkips {
			cb.isDormant = true
		}
		return true
	}

	cb.consecutiveSkips = 0
	return false
}

// RecordSuccess records a successful collection cycle.
// After 3 consecutive successes while dormant, exits dormant mode.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveSkips = 0
	cb.successCount++

	if cb.isDormant && cb.successCount >= 3 {
		cb.isDormant = false
		cb.successCount = 0
	}
}

// IsDormant returns true if the breaker has entered dormant mode.
func (cb *CircuitBreaker) IsDormant() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.isDormant
}

package collector

import (
	"context"
	"fmt"
	"sync"
)

// Why the breaker skipped a cycle.
const (
	// SkipReasonLoad: active client sessions over max_connections
	// exceeded safety.cpu_ceiling_pct.
	SkipReasonLoad = "load"
	// SkipReasonLoadQueryError: the load query itself failed (a
	// permission, pooler or connection fault). It still counts toward
	// dormancy: the full cycle would fail the same way.
	SkipReasonLoadQueryError = "load_query_error"
)

// Decision is the breaker's verdict on one cycle.
type Decision struct {
	Skip bool
	// Reason is SkipReasonLoad or SkipReasonLoadQueryError when Skip.
	Reason string
	// Ratio is the measured load ratio (0 when the query failed);
	// Ceiling is the configured one, both as fractions.
	Ratio   float64
	Ceiling float64
	// Err is the load query's error (SkipReasonLoadQueryError only).
	Err error
}

// cause is the operator-facing explanation of a skip.
func (d Decision) cause() string {
	if d.Reason == SkipReasonLoadQueryError {
		return fmt.Sprintf("load query failed: %v", d.Err)
	}
	ratio, ceiling := fmt.Sprintf("%.2f", d.Ratio), fmt.Sprintf("%.2f", d.Ceiling)
	if ratio == ceiling { // just over the ceiling: show the digits that differ
		ratio = fmt.Sprintf("%.6g", d.Ratio)
	}
	return fmt.Sprintf("load ratio %s > ceiling %s "+
		"(active sessions / max_connections)", ratio, ceiling)
}

// CircuitBreaker prevents collection when the database is under heavy load.
type CircuitBreaker struct {
	mu               sync.Mutex
	consecutiveSkips int
	isDormant        bool
	cpuCeilingPct    int
	maxSkips         int
	successCount     int
	lastReason       string
	lastRatio        float64
	skipsTotal       map[string]int64
}

// NewCircuitBreaker creates a breaker that trips when load_ratio exceeds
// cpuCeilingPct and enters dormant mode after maxSkips consecutive skips.
func NewCircuitBreaker(cpuCeilingPct, maxSkips int) *CircuitBreaker {
	return &CircuitBreaker{
		cpuCeilingPct: cpuCeilingPct,
		maxSkips:      maxSkips,
		skipsTotal:    map[string]int64{},
	}
}

// ShouldSkip checks active_backends / max_connections against the ceiling
// and says whether, and why, the current cycle should be skipped.
func (cb *CircuitBreaker) ShouldSkip(ctx context.Context, pool catalogQuerier) Decision {
	d := Decision{Ceiling: float64(cb.cpuCeilingPct) / 100.0}
	if err := pool.QueryRow(ctx, loadRatioSQL).Scan(&d.Ratio); err != nil {
		// If we can't even query load, skip to be safe.
		d.Ratio = 0
		d.Skip, d.Reason = true, SkipReasonLoadQueryError
		d.Err = err
	} else if d.Ratio > d.Ceiling {
		d.Skip, d.Reason = true, SkipReasonLoad
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()
	if d.Reason != SkipReasonLoadQueryError {
		cb.lastRatio = d.Ratio
	}
	if !d.Skip {
		cb.consecutiveSkips = 0
		if !cb.isDormant { // a dormant breaker keeps why it went dormant
			cb.lastReason = ""
		}
		return d
	}
	cb.consecutiveSkips++
	cb.successCount = 0
	cb.lastReason = d.Reason
	cb.skipsTotal[d.Reason]++
	if cb.consecutiveSkips >= cb.maxSkips {
		cb.isDormant = true
	}
	return d
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
		cb.lastReason = ""
	}
}

// IsDormant returns true if the breaker has entered dormant mode.
func (cb *CircuitBreaker) IsDormant() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.isDormant
}

// breakerView is a consistent copy of the breaker's state.
type breakerView struct {
	state            BreakerState
	reason           string
	consecutiveSkips int
	lastRatio        float64
	ceiling          float64
	skipsTotal       map[string]int64
}

func (cb *CircuitBreaker) view() breakerView {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	v := breakerView{
		state:            BreakerClosed,
		reason:           cb.lastReason,
		consecutiveSkips: cb.consecutiveSkips,
		lastRatio:        cb.lastRatio,
		ceiling:          float64(cb.cpuCeilingPct) / 100.0,
		skipsTotal: map[string]int64{
			SkipReasonLoad:           cb.skipsTotal[SkipReasonLoad],
			SkipReasonLoadQueryError: cb.skipsTotal[SkipReasonLoadQueryError],
		},
	}
	switch {
	case cb.isDormant:
		v.state = BreakerDormant
	case cb.consecutiveSkips > 0:
		v.state = BreakerSkipping
	}
	return v
}

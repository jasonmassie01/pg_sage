package llm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Circuit breaker. Closed admits every request. failureThreshold counted
// failures in a row (or one auth failure) open it. Open refuses requests
// for the cooldown; after it, the next request is admitted as the single
// half-open probe and every other request is still refused. The probe's
// success closes the breaker, its failure reopens it for another
// cooldown, and a probe that ends without a verdict (a caller rejection,
// a cancellation, a local budget refusal) hands the probe to the next
// request. Reads (IsCircuitOpen, status) never change the state.

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

func (s breakerState) String() string {
	switch s {
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half_open"
	}
	return "closed"
}

// failureThreshold is the number of counted failures in a row that opens
// a closed breaker.
const failureThreshold = 3

// IsCircuitOpen reports whether the breaker refuses requests now: open
// within its cooldown, or half-open with the probe in flight. It does not
// change the breaker.
func (c *Client) IsCircuitOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case breakerHalfOpen:
		return true
	case breakerOpen:
		return time.Since(c.openedAt) < c.cooldown
	}
	return false
}

// admit lets one request through the breaker. probe reports that the
// request is the half-open probe; the caller must pass it to endProbe.
// force (an operator's connection test) skips the remaining cooldown.
func (c *Client) admit(force bool) (probe bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case breakerClosed:
		return false, nil
	case breakerHalfOpen:
		return false, c.openErrorLocked()
	}
	if !force && time.Since(c.openedAt) < c.cooldown {
		return false, c.openErrorLocked()
	}
	c.state = breakerHalfOpen
	return true, nil
}

// endProbe returns an undecided probe: the breaker goes back to open with
// its original opening time, so the next request can probe again.
func (c *Client) endProbe(probe bool) {
	if !probe {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == breakerHalfOpen {
		c.state = breakerOpen
	}
}

func (c *Client) openErrorLocked() error {
	if c.lastClass == classNone {
		return ErrCircuitOpen
	}
	return fmt.Errorf("%w (last error: %s)", ErrCircuitOpen, c.lastClass)
}

// recordProviderFailure counts one failed request of the given class.
func (c *Client) recordProviderFailure(class errClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.lastClass, c.lastErrorAt = class, now
	c.failures++
	switch {
	case c.state == breakerHalfOpen:
		c.openLocked(now, "the half-open probe failed")
	case c.state == breakerOpen:
		// A request admitted before the breaker opened: already open.
	case class == classAuth:
		c.openLocked(now, "an authentication failure")
	case c.failures >= failureThreshold:
		c.openLocked(now, fmt.Sprintf("%d consecutive failures", c.failures))
	}
}

func (c *Client) openLocked(now time.Time, why string) {
	c.state, c.openedAt = breakerOpen, now
	c.logFn("llm", "circuit breaker opened after %s (last error: %s); "+
		"retrying in %s", why, c.lastClass, c.cooldown)
}

// noteRejection records an uncounted provider error for the status.
func (c *Client) noteRejection(class errClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastClass, c.lastErrorAt = class, time.Now()
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures, c.lastSuccessAt = 0, time.Now()
	if c.state != breakerClosed {
		c.state = breakerClosed
		c.logFn("llm", "circuit breaker closed (provider answered)")
	}
}

// noteFailure feeds a request's error to the breaker, once per request.
// Errors of a request the client was reconfigured away from are not the
// new provider's, and a tool-path 429 (countRate false) is the provider
// asking the caller to back off, not a fault.
func (c *Client) noteFailure(generation uint64, err error, countRate bool) {
	var pf *providerFailure
	if !errors.As(err, &pf) || !c.generationCurrent(generation) {
		return
	}
	switch {
	case pf.class == classCanceled:
	case pf.class == classCaller, pf.class == classRate && !countRate:
		c.noteRejection(pf.class)
	default:
		c.recordProviderFailure(pf.class)
	}
}

// resetBreakerOnProviderChange closes the breaker when the provider
// identity (endpoint, key, auth mode or model) changed: the failures were
// the old provider's.
func (c *Client) resetBreakerOnProviderChange(prev, next config.LLMConfig) {
	if prev.Endpoint == next.Endpoint && prev.APIKey == next.APIKey &&
		prev.Auth == next.Auth && prev.Model == next.Model {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != breakerClosed || c.failures > 0 {
		c.logFn("llm", "circuit breaker reset: provider settings changed")
	}
	c.state, c.failures = breakerClosed, 0
}

type probeKey struct{}

// withForcedProbe marks ctx as an operator's connection test, admitted as
// the half-open probe even inside the cooldown.
func withForcedProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, probeKey{}, true)
}

func forcedProbe(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	forced, _ := ctx.Value(probeKey{}).(bool)
	return forced
}

// breakerStatus is the breaker part of ClientStatus.
type breakerStatus struct {
	state         string
	lastClass     string
	lastErrorAt   *time.Time
	lastSuccessAt *time.Time
	failures      int
}

func (c *Client) breakerSnapshot() breakerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return breakerStatus{
		state:         c.state.String(),
		lastClass:     string(c.lastClass),
		lastErrorAt:   timePtr(c.lastErrorAt),
		lastSuccessAt: timePtr(c.lastSuccessAt),
		failures:      c.failures,
	}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

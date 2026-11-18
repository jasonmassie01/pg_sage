package codehost

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Bounds of a rate-limit cool-down.
const (
	defaultRetryAfter = time.Minute
	minRetryAfter     = time.Second
	maxRetryAfter     = time.Hour
)

// limited reports a rate-limit answer: 429, or GitHub's 403 with an
// exhausted quota or a secondary-limit Retry-After.
func limited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	return resp.Header.Get("Retry-After") != "" ||
		resp.Header.Get("X-RateLimit-Remaining") == "0"
}

// startCoolDown blocks the host until the answer's retry time and returns
// the rate-limit error.
func (r *rest) startCoolDown(h http.Header) error {
	now := r.now()
	wait := retryAfter(h, now)
	r.mu.Lock()
	if until := now.Add(wait); until.After(r.blockedUntil) {
		r.blockedUntil = until
	}
	r.mu.Unlock()
	return &RateLimitError{Host: r.name(), RetryAfter: wait}
}

// coolingDown refuses a request while a cool-down runs, without sending it.
func (r *rest) coolingDown() error {
	now := r.now()
	r.mu.Lock()
	until := r.blockedUntil
	r.mu.Unlock()
	if now.Before(until) {
		return &RateLimitError{Host: r.name(), RetryAfter: until.Sub(now)}
	}
	return nil
}

// retryAfter reads Retry-After (seconds or an HTTP date), then GitHub's
// X-RateLimit-Reset (epoch seconds), else one minute; it is kept within
// one second and one hour.
func retryAfter(h http.Header, now time.Time) time.Duration {
	wait := defaultRetryAfter
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			wait = time.Duration(secs) * time.Second
		} else if at, err := http.ParseTime(v); err == nil {
			wait = at.Sub(now)
		}
	} else if v := h.Get("X-RateLimit-Reset"); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			wait = time.Unix(epoch, 0).Sub(now)
		}
	}
	return min(max(wait, minRetryAfter), maxRetryAfter)
}

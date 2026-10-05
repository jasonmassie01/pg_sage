package specialist

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Limits bound what one identity (and all of them) may do.
type Limits struct {
	WritesPerMinute    int // opens and remediation requests
	ReadsPerMinute     int // status, result and stream calls
	MaxOpenPerIdentity int // live investigations one identity opened
	MaxOpenTotal       int // live investigations all identities opened
}

// DefaultLimits are the configuration defaults.
func DefaultLimits() Limits {
	return Limits{WritesPerMinute: 30, ReadsPerMinute: 240, MaxOpenPerIdentity: 3,
		MaxOpenTotal: 10}
}

// Validate checks the bounds (rates 1-10000, live investigations 1-100,
// per identity at most the total).
func (l Limits) Validate() error {
	switch {
	case l.WritesPerMinute < 1 || l.WritesPerMinute > 10000:
		return invalidf("writes per minute must be 1-10000, got %d", l.WritesPerMinute)
	case l.ReadsPerMinute < 1 || l.ReadsPerMinute > 10000:
		return invalidf("reads per minute must be 1-10000, got %d", l.ReadsPerMinute)
	case l.MaxOpenPerIdentity < 1 || l.MaxOpenTotal < 1 || l.MaxOpenTotal > 100:
		return invalidf("live investigation bounds must be 1-100")
	case l.MaxOpenPerIdentity > l.MaxOpenTotal:
		return invalidf("per-identity bound %d exceeds the total %d",
			l.MaxOpenPerIdentity, l.MaxOpenTotal)
	}
	return nil
}

// maxLimiterKeys bounds the per-identity buckets held in memory.
const maxLimiterKeys = 10000

type bucket struct {
	tokens float64
	at     time.Time
}

// rateLimiter is a token bucket per key: burst = perMinute, refilled at
// perMinute per minute.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	now     func() time.Time
	buckets map[string]*bucket
}

func newRateLimiter(perMinute int, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{rate: float64(perMinute) / 60, burst: float64(perMinute), now: now,
		buckets: map[string]*bucket{}}
}

// Allow takes one token of key, or reports how long until one is free.
func (r *rateLimiter) Allow(key string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[key]
	if !ok {
		r.evict(now)
		b = &bucket{tokens: r.burst, at: now}
		r.buckets[key] = b
	}
	b.tokens = math.Min(r.burst, b.tokens+now.Sub(b.at).Seconds()*r.rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / r.rate * float64(time.Second))
	return false, max(wait, time.Millisecond)
}

// evict makes room for one more key: full buckets go first (a fresh one
// is identical), then the least recently used.
func (r *rateLimiter) evict(now time.Time) {
	if len(r.buckets) < maxLimiterKeys {
		return
	}
	for k, b := range r.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*r.rate >= r.burst {
			delete(r.buckets, k)
		}
	}
	if len(r.buckets) < maxLimiterKeys {
		return
	}
	keys := make([]string, 0, len(r.buckets))
	for k := range r.buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return r.buckets[keys[i]].at.Before(r.buckets[keys[j]].at)
	})
	for _, k := range keys[:len(keys)-maxLimiterKeys+1] {
		delete(r.buckets, k)
	}
}

func (r *rateLimiter) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}

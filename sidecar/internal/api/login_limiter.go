package api

import (
	"strings"
	"sync"
	"time"
)

// loginRateLimiter bounds failed login attempts per email. Attempts are
// reserved atomically before authentication, so concurrent requests
// for one account cannot exceed loginMaxAttempts, and every insertion
// path enforces loginMaxEntries (SURF-16 / G6-B06).
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

var loginLimiter = newLoginRateLimiter()

const (
	loginMaxAttempts = 5
	loginWindow      = 15 * time.Minute
	loginMaxEntries  = 10000
	loginCleanupFreq = 5 * time.Minute
)

func newLoginRateLimiter() *loginRateLimiter {
	l := &loginRateLimiter{
		attempts: make(map[string][]time.Time),
		stop:     make(chan struct{}),
	}
	go l.cleanupLoop()
	return l
}

// cleanupLoop periodically purges expired entries. It exits when
// Stop is called.
func (l *loginRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(loginCleanupFreq)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.purgeExpired()
		}
	}
}

// Stop halts the cleanup goroutine. Idempotent.
func (l *loginRateLimiter) Stop() {
	l.stopOnce.Do(func() {
		close(l.stop)
	})
}

// ShutdownLoginLimiter stops the package-level limiter's cleanup
// goroutine during graceful shutdown.
func ShutdownLoginLimiter() {
	loginLimiter.Stop()
}

func normalizeLoginEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (l *loginRateLimiter) purgeExpired() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-loginWindow)
	for email, attempts := range l.attempts {
		if valid := pruneAttempts(attempts, cutoff); len(valid) == 0 {
			delete(l.attempts, email)
		} else {
			l.attempts[email] = valid
		}
	}
}

func pruneAttempts(attempts []time.Time, cutoff time.Time) []time.Time {
	valid := attempts[:0]
	for _, t := range attempts {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	return valid
}

// reserve records an attempt for email and reports whether it is
// within the limit. The attempt counts whether or not the password
// later verifies; a successful login calls reset.
func (l *loginRateLimiter) reserve(email string) bool {
	key := normalizeLoginEmail(email)
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	valid := pruneAttempts(l.attempts[key], now.Add(-loginWindow))
	if len(valid) >= loginMaxAttempts {
		l.attempts[key] = valid
		return false
	}
	if _, exists := l.attempts[key]; !exists &&
		len(l.attempts) >= loginMaxEntries {
		l.evictOneLocked(now.Add(-loginWindow))
	}
	l.attempts[key] = append(valid, now)
	return true
}

// evictOneLocked removes one entry, preferring one whose newest
// attempt has expired, otherwise the entry with the oldest newest
// attempt. Caller must hold mu.
func (l *loginRateLimiter) evictOneLocked(cutoff time.Time) {
	var oldestEmail string
	var oldestLast time.Time
	for email, attempts := range l.attempts {
		if len(attempts) == 0 {
			delete(l.attempts, email)
			return
		}
		last := attempts[len(attempts)-1]
		if last.Before(cutoff) {
			delete(l.attempts, email)
			return
		}
		if oldestEmail == "" || last.Before(oldestLast) {
			oldestEmail = email
			oldestLast = last
		}
	}
	if oldestEmail != "" {
		delete(l.attempts, oldestEmail)
	}
}

// reset clears attempts for the email after a successful login.
func (l *loginRateLimiter) reset(email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, normalizeLoginEmail(email))
}

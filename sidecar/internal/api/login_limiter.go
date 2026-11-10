package api

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
)

// loginRateLimiter bounds login attempts per key (an email, or a client
// address for the failure budget). Attempts are reserved atomically
// before authentication, so concurrent requests for one key cannot exceed
// the limit, and every insertion path enforces loginMaxEntries without
// ever evicting a locked key (SURF-16 / G6-B06, apiauth-06).
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int // attempts per loginWindow; 0 means loginMaxAttempts
	// saturated counts new keys refused because every tracked key was
	// locked, so none could be evicted.
	saturated atomic.Uint64
	stop      chan struct{}
	stopOnce  sync.Once
}

// loginLimiter bounds attempts per email; loginFailureBudget bounds failed
// logins per client address (an IPv6 /64) across all emails.
var (
	loginLimiter       = newLoginRateLimiter(loginMaxAttempts)
	loginFailureBudget = newLoginRateLimiter(loginFailureBudgetLimit)
)

const (
	loginMaxAttempts        = 5
	loginFailureBudgetLimit = 20
	loginWindow             = 15 * time.Minute
	loginMaxEntries         = 10000
	loginCleanupFreq        = 5 * time.Minute
)

func newLoginRateLimiter(limit int) *loginRateLimiter {
	l := &loginRateLimiter{
		attempts: make(map[string][]time.Time),
		limit:    limit,
		stop:     make(chan struct{}),
	}
	go l.cleanupLoop()
	return l
}

func (l *loginRateLimiter) maxAttempts() int {
	if l.limit <= 0 {
		return loginMaxAttempts
	}
	return l.limit
}

// LoginLimiterSaturatedTotal is how many login attempts were refused
// because a limiter was full of locked keys (pg_sage_login_limiter_
// saturated_total). A rising value means a credential-spraying attack.
func LoginLimiterSaturatedTotal() uint64 {
	return loginLimiter.saturated.Load() + loginFailureBudget.saturated.Load()
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

// ShutdownLoginLimiter stops the package-level limiters' cleanup
// goroutines during graceful shutdown.
func ShutdownLoginLimiter() {
	loginLimiter.Stop()
	loginFailureBudget.Stop()
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

// reserve records an attempt for key and reports whether it is within
// the limit. The attempt counts whether or not the password later
// verifies; a successful login calls reset (per email) or refund (budget).
// A new key arriving when the limiter is full and every key is locked is
// refused: failing closed beats handing a locked account fresh guesses.
func (l *loginRateLimiter) reserve(key string) bool {
	key = auth.NormalizeEmail(key)
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	valid := pruneAttempts(l.attempts[key], now.Add(-loginWindow))
	if len(valid) >= l.maxAttempts() {
		l.attempts[key] = valid
		return false
	}
	if _, exists := l.attempts[key]; !exists &&
		len(l.attempts) >= loginMaxEntries &&
		!l.evictOneLocked(now.Add(-loginWindow)) {
		l.saturated.Add(1)
		return false
	}
	l.attempts[key] = append(valid, now)
	return true
}

// evictOneLocked removes one unlocked key and reports whether it could:
// an expired key if any, otherwise the key with the fewest live attempts
// (oldest newest attempt first), so eviction hands back as few guesses as
// possible. Locked keys are never evicted. Caller must hold mu.
func (l *loginRateLimiter) evictOneLocked(cutoff time.Time) bool {
	limit := l.maxAttempts()
	var victim string
	var victimLive int
	var victimLast time.Time
	for key, attempts := range l.attempts {
		live := liveAttempts(attempts, cutoff)
		if live == 0 {
			delete(l.attempts, key)
			return true
		}
		if live >= limit {
			continue
		}
		last := attempts[len(attempts)-1]
		if victim == "" || live < victimLive ||
			(live == victimLive && last.Before(victimLast)) {
			victim, victimLive, victimLast = key, live, last
		}
	}
	if victim == "" {
		return false
	}
	delete(l.attempts, victim)
	return true
}

// liveAttempts counts the attempts after cutoff; attempts are in order.
func liveAttempts(attempts []time.Time, cutoff time.Time) int {
	n := 0
	for i := len(attempts) - 1; i >= 0 && attempts[i].After(cutoff); i-- {
		n++
	}
	return n
}

// reset clears attempts for the email after a successful login.
func (l *loginRateLimiter) reset(email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, auth.NormalizeEmail(email))
}

// refund gives back one reserved attempt for a request that turned out not
// to be a failed login. Earlier failures stay counted.
func (l *loginRateLimiter) refund(key string) {
	key = auth.NormalizeEmail(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	attempts, ok := l.attempts[key]
	if !ok {
		return
	}
	if len(attempts) <= 1 {
		delete(l.attempts, key)
		return
	}
	l.attempts[key] = attempts[:len(attempts)-1]
}

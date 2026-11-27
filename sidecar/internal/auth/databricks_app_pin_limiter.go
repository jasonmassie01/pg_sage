package auth

import (
	"sync"
	"time"
)

// pinAuditWindow is how long one changed identity (user, stored pin, seen
// pin) is recorded only once. pinAuditMaxKeys bounds the memory a flood of
// distinct changed identities can take.
const (
	pinAuditWindow  = time.Hour
	pinAuditMaxKeys = 1024
)

type pinAuditKey struct {
	userID       int
	stored, seen string
}

// pinAuditLimiter remembers when each changed identity was last recorded.
// It is process-local: a restart records each change once more.
type pinAuditLimiter struct {
	mu   sync.Mutex
	seen map[pinAuditKey]time.Time
	now  func() time.Time
}

var databricksPinAudits = newPinAuditLimiter(time.Now)

func newPinAuditLimiter(now func() time.Time) *pinAuditLimiter {
	return &pinAuditLimiter{seen: make(map[pinAuditKey]time.Time), now: now}
}

// allow reports whether k should be recorded now, and if so remembers it.
func (l *pinAuditLimiter) allow(k pinAuditKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if last, ok := l.seen[k]; ok && now.Sub(last) < pinAuditWindow {
		return false
	}
	if len(l.seen) >= pinAuditMaxKeys {
		l.evict(now)
	}
	l.seen[k] = now
	return true
}

// evict drops expired entries, then everything if the map is still full:
// forgetting early only means a change is recorded again.
func (l *pinAuditLimiter) evict(now time.Time) {
	for k, at := range l.seen {
		if now.Sub(at) >= pinAuditWindow {
			delete(l.seen, k)
		}
	}
	if len(l.seen) >= pinAuditMaxKeys {
		clear(l.seen)
	}
}

// forget drops a user's entries, so a change after a pin reset is recorded.
func (l *pinAuditLimiter) forget(userID int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.seen {
		if k.userID == userID {
			delete(l.seen, k)
		}
	}
}

// release forgets k, so the next sighting is recorded (its write failed).
func (l *pinAuditLimiter) release(k pinAuditKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.seen, k)
}

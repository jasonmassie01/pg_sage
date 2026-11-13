package budget

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// Where a day's actual cancels came from (sage.budget_shadow.cancel_source).
const (
	// CancelSourceNone: pg_sage reads no server log for this database, so
	// an enforced timeout's cancels are unknown.
	CancelSourceNone = "none"
	// CancelSourceLog: counted from the server log through logwatch.
	CancelSourceLog = "log"
)

// EntrySource is a parsed server-log subscription
// (logwatch.EntrySubscriber).
type EntrySource interface {
	Drain() []logwatch.LogEntry
	TakeEvicted() int
}

// IsStatementTimeoutCancel reports a statement cancelled by a timeout as
// logwatch classifies log_statement_timeout (logwatch/patterns.go):
// SQLSTATE 57014 at ERROR or above, not a cancel "due to user request" and
// not an autovacuum cancel. A 57014 without English text (lc_messages)
// counts too: counting a possible client cancel errs toward reverting,
// the safe direction.
func IsStatementTimeoutCancel(e logwatch.LogEntry) bool {
	if e.SQLState != "57014" {
		return false
	}
	switch e.ErrorLevel {
	case "ERROR", "FATAL", "PANIC":
	default:
		return false
	}
	lower := strings.ToLower(e.Message)
	return !strings.Contains(lower, "due to user request") &&
		!strings.Contains(lower, "canceling autovacuum task")
}

// RoleCancels is one role's statement-timeout cancels on one UTC day.
type RoleCancels struct {
	Role  string
	Day   time.Time
	Count int64
}

// CancelBatch is the cancels counted since the last Take, and the days on
// which the subscription dropped entries (their counts are lower bounds).
type CancelBatch struct {
	Counts     []RoleCancels
	LowerBound []time.Time
}

type cancelKey struct {
	role string
	day  time.Time
}

// CancelCounter counts one database's statement-timeout cancels per role
// and UTC day from its log subscription. It is safe for concurrent use; a
// nil counter is "no log source".
type CancelCounter struct {
	database string
	src      EntrySource
	active   func() bool
	mu       sync.Mutex
	counts   map[cancelKey]int64
	lower    map[time.Time]bool
}

// NewCancelCounter counts database's cancels from src; active reports
// whether the log watcher is reading the server log.
func NewCancelCounter(database string, src EntrySource, active func() bool) *CancelCounter {
	return &CancelCounter{database: database, src: src, active: active,
		counts: map[cancelKey]int64{}, lower: map[time.Time]bool{}}
}

// Active reports a working log source: a subscription on a watcher that
// reads the server log.
func (c *CancelCounter) Active() bool {
	return c != nil && c.src != nil && c.active != nil && c.active()
}

// Poll drains the subscription once and counts its cancels; entries the
// subscription evicted unread mark now's day a lower bound.
func (c *CancelCounter) Poll(now time.Time) {
	if c == nil || c.src == nil {
		return
	}
	entries := c.src.Drain()
	evicted := c.src.TakeEvicted()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range entries {
		if e.User == "" || c.database != "" && e.Database != c.database ||
			!IsStatementTimeoutCancel(e) {
			continue
		}
		c.counts[cancelKey{role: e.User, day: utcDay(e.Timestamp)}]++
	}
	if evicted > 0 {
		c.lower[utcDay(now)] = true
	}
}

// Run polls every interval until ctx ends, so the subscription's bounded
// buffer is drained between analyzer cycles.
func (c *CancelCounter) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.Poll(now)
		}
	}
}

// Take returns the cancels counted since the last Take and starts again.
func (c *CancelCounter) Take() CancelBatch {
	if c == nil {
		return CancelBatch{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var b CancelBatch
	for k, n := range c.counts {
		b.Counts = append(b.Counts, RoleCancels{Role: k.role, Day: k.day, Count: n})
	}
	for d := range c.lower {
		b.LowerBound = append(b.LowerBound, d)
	}
	c.counts, c.lower = map[cancelKey]int64{}, map[time.Time]bool{}
	return b
}

// Restore adds back a batch whose write failed, so the next cycle writes
// it.
func (c *CancelCounter) Restore(b CancelBatch) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rc := range b.Counts {
		c.counts[cancelKey{role: rc.Role, day: rc.Day}] += rc.Count
	}
	for _, d := range b.LowerBound {
		c.lower[d] = true
	}
}

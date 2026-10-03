package retention

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// Retry delays of the background conversion of a plain history table.
const (
	firstConvertRetry = time.Hour
	maxConvertRetry   = 24 * time.Hour
)

// backoff schedules conversion attempts per table: the first is due at
// once, each failure doubles the wait (1 h up to a day), a success resets.
type backoff struct {
	next  map[string]time.Time
	delay map[string]time.Duration
}

func (b *backoff) due(table string, now time.Time) bool {
	return !now.Before(b.next[table])
}

// failed records a failed attempt at now and returns the wait until the
// next one.
func (b *backoff) failed(table string, now time.Time) time.Duration {
	if b.next == nil {
		b.next, b.delay = map[string]time.Time{}, map[string]time.Duration{}
	}
	d := min(max(2*b.delay[table], firstConvertRetry), maxConvertRetry)
	b.delay[table], b.next[table] = d, now.Add(d)
	return d
}

func (b *backoff) succeeded(table string) {
	delete(b.next, table)
	delete(b.delay, table)
}

// conversions is a Cleaner's background conversion state (shared by the
// copies target makes).
type conversions struct {
	mu      sync.Mutex
	backoff backoff
	running bool
	wg      sync.WaitGroup
}

// ConvertOutcome is what ConvertHistory did for one history table.
type ConvertOutcome struct {
	Table     string
	Attempted bool
	Result    partition.Result
	Err       error
	NextTry   time.Duration // after a failure
}

// ConvertHistory partitions by day each history table that is still a
// plain table (an upgrade that bootstrap left to retention: too large to
// convert at startup, or busy), when its backoff allows, and removes what
// an unfinished conversion left behind on every call. A failure is logged
// once per attempt at WARN; pg_sage keeps running on the plain table, which
// retention still bounds with paced deletes.
func (c *Cleaner) ConvertHistory(ctx context.Context, now time.Time) []ConvertOutcome {
	out := make([]ConvertOutcome, 0, len(partition.HistoryTables()))
	for _, t := range partition.HistoryTables() {
		o := ConvertOutcome{Table: t.Name}
		if c.convertible(ctx, t, now) {
			c.attempt(ctx, t, now, &o)
		}
		out = append(out, o)
	}
	return out
}

// convertible reports whether t is a plain table due for an attempt; on
// a plain table it first removes leftovers of an unfinished conversion
// (its CHECK refuses every row once the clock passes the cut).
func (c *Cleaner) convertible(ctx context.Context, t partition.Table, now time.Time) bool {
	kind, err := c.relkind(ctx, t.Name)
	if err != nil {
		c.logFn("ERROR", "retention: reading sage.%s failed: %v", t.Name, err)
		return false
	}
	if kind != "r" {
		return false
	}
	if err := partition.Cleanup(ctx, c.pool, t); err != nil {
		c.logFn("ERROR", "retention: removing an unfinished conversion of sage.%s failed "+
			"(retried next run): %v", t.Name, err)
	}
	c.conv.mu.Lock()
	defer c.conv.mu.Unlock()
	return c.conv.backoff.due(t.Name, now)
}

func (c *Cleaner) attempt(ctx context.Context, t partition.Table, now time.Time,
	o *ConvertOutcome) {
	o.Attempted = true
	o.Result, o.Err = partition.Convert(ctx, c.pool, t)
	if errors.Is(o.Err, partition.ErrBusy) {
		o.Attempted = false // another process is at it; not a failure
		return
	}
	c.conv.mu.Lock()
	if o.Err != nil {
		o.NextTry = c.conv.backoff.failed(t.Name, now)
	} else {
		c.conv.backoff.succeeded(t.Name)
	}
	c.conv.mu.Unlock()
	if o.Err != nil {
		c.logFn("WARN", "retention: sage.%s is still one plain table: partitioning it by day "+
			"failed: %v. pg_sage keeps working on it and deletes expired rows in paced "+
			"batches. Next attempt in %s. %s", t.Name, o.Err, o.NextTry, partition.Hint(o.Err))
		return
	}
	if _, err := partition.Ensure(ctx, c.pool, t, now, 3); err != nil {
		c.logFn("ERROR", "retention: creating the daily partitions of sage.%s failed: %v",
			t.Name, err)
	}
	c.logFn("INFO", "retention: sage.%s is now partitioned by UTC day (validated in %s "+
		"without blocking writers; exclusive lock held %s)", t.Name,
		o.Result.Validated.Round(time.Millisecond), o.Result.LockHeld.Round(time.Millisecond))
}

// convertInBackground starts the due conversions on their own goroutine,
// so a long VALIDATE never holds the caller's cycle. Checking what is due
// is two catalog reads; nothing starts while an attempt is running.
func (c *Cleaner) convertInBackground(ctx context.Context, now time.Time) {
	if c.conv == nil {
		return
	}
	c.conv.mu.Lock()
	if c.conv.running {
		c.conv.mu.Unlock()
		return
	}
	c.conv.running = true
	c.conv.wg.Add(1)
	c.conv.mu.Unlock()
	var due []partition.Table
	for _, t := range partition.HistoryTables() {
		if c.convertible(ctx, t, now) {
			due = append(due, t)
		}
	}
	if len(due) == 0 {
		c.conv.mu.Lock()
		c.conv.running = false
		c.conv.mu.Unlock()
		c.conv.wg.Done()
		return
	}
	go func() {
		defer func() {
			c.conv.mu.Lock()
			c.conv.running = false
			c.conv.mu.Unlock()
			c.conv.wg.Done()
		}()
		for _, t := range due {
			var o ConvertOutcome
			c.attempt(ctx, t, now, &o)
		}
	}()
}

// waitConversions waits for a background conversion to end (tests).
func (c *Cleaner) waitConversions() {
	if c.conv != nil {
		c.conv.wg.Wait()
	}
}

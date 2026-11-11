package collector

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

// collection-07: the queries step samples a queryid that leaves the top-N
// while something depends on it. A verification whose target query got
// faster (less total time) would otherwise lose its samples and revert as
// insufficient_samples: the better the index, the likelier the revert.
// Pins come from a durable PinSource (the open verifications in
// sage.verification, so they survive a restart) and from in-memory
// watches (callers without a durable row). Pins are per collector, so per
// database, in fleet mode too.

// PinSource lists the queryids the collector samples whatever their rank,
// oldest first.
type PinSource interface {
	PinnedQueryIDs(ctx context.Context) ([]int64, error)
}

// PinSourceFunc adapts a function to a PinSource.
type PinSourceFunc func(context.Context) ([]int64, error)

// PinnedQueryIDs calls f.
func (f PinSourceFunc) PinnedQueryIDs(ctx context.Context) ([]int64, error) {
	return f(ctx)
}

// pinSourceTimeout bounds one read of the PinSource.
const pinSourceTimeout = 5 * time.Second

// watchEntry is one in-memory watch: when it was first registered (its
// age, for the cap) and until when it holds.
type watchEntry struct {
	since, until time.Time
}

// pinRegistry is a collector's pin state; its zero value is ready.
type pinRegistry struct {
	mu      sync.Mutex
	source  PinSource
	watches map[int64]watchEntry
	failing bool // the source's last read failed (one WARN per streak)
}

// WithPinSource sets the durable source of pinned queryids. A nil source
// pins nothing; the collector calls it once per cycle.
func (c *Collector) WithPinSource(src PinSource) {
	c.pins.mu.Lock()
	defer c.pins.mu.Unlock()
	c.pins.source = src
}

// Watch samples queryID every cycle, whatever its rank, until until. A
// queryid of 0 (unattributable) or a watch already over is ignored; a
// re-watch extends the watch, never shortens it, and keeps its age.
func (c *Collector) Watch(queryID int64, until time.Time) {
	now := c.clock()
	if queryID == 0 || !now.Before(until) {
		return
	}
	c.pins.mu.Lock()
	defer c.pins.mu.Unlock()
	if c.pins.watches == nil {
		c.pins.watches = map[int64]watchEntry{}
	}
	entry, ok := c.pins.watches[queryID]
	if !ok {
		entry.since = now
	}
	if until.After(entry.until) {
		entry.until = until
	}
	c.pins.watches[queryID] = entry
}

// pinnedQueryIDs is this cycle's pins: the source's (in its order), then
// the live watches (oldest first), each once, at most limit of them. A
// failing source pins nothing and never fails the queries step.
func (c *Collector) pinnedQueryIDs(ctx context.Context, limit int) []int64 {
	pins := make([]int64, 0, limit)
	if limit <= 0 {
		return pins
	}
	for _, id := range slices.Concat(c.sourcePins(ctx), c.activeWatches()) {
		if id != 0 && !slices.Contains(pins, id) {
			pins = append(pins, id)
		}
		if len(pins) == limit {
			break
		}
	}
	return pins
}

// sourcePins reads the PinSource; an error is logged once per failure
// streak and pins nothing.
func (c *Collector) sourcePins(ctx context.Context) []int64 {
	c.pins.mu.Lock()
	src := c.pins.source
	c.pins.mu.Unlock()
	if src == nil {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, pinSourceTimeout)
	defer cancel()
	ids, err := src.PinnedQueryIDs(readCtx)
	c.pins.mu.Lock()
	defer c.pins.mu.Unlock()
	if err != nil {
		if !c.pins.failing {
			c.logFn("WARN", "collector: pinned queryids unavailable, sampling the "+
				"ranked queries only until the source recovers: %v", err)
		}
		c.pins.failing = true
		return nil
	}
	c.pins.failing = false
	return ids
}

// activeWatches drops the expired watches and returns the live ones'
// queryids, oldest first.
func (c *Collector) activeWatches() []int64 {
	now := c.clock()
	c.pins.mu.Lock()
	defer c.pins.mu.Unlock()
	ids := make([]int64, 0, len(c.pins.watches))
	for id, entry := range c.pins.watches {
		if !now.Before(entry.until) {
			delete(c.pins.watches, id)
			continue
		}
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b int64) int {
		if ea, eb := c.pins.watches[a], c.pins.watches[b]; !ea.since.Equal(eb.since) {
			return ea.since.Compare(eb.since)
		}
		return cmp.Compare(a, b)
	})
	return ids
}

// Admission bounds of the ranked read (collection-07). Internal constants,
// not configuration: they share the 2N rows the read always had.
const (
	// admissionArmMax caps the mean and io arms; each is N/10 otherwise.
	admissionArmMax = 50
	// pinCapMax caps the pins of one cycle.
	pinCapMax = 64
)

// queryAdmission sizes the arms of the ranked read for max_queries N.
type queryAdmission struct {
	limit  int // N
	perArm int // K: the mean arm's and the io arm's rows
	pinCap int // P: at most this many pins, so the total arm keeps N rows
}

// admissionFor is K = N/10 within [1, 50] and P = min(64, N - 2K); an N
// below 2 is too small to share and keeps the total ranking alone.
func admissionFor(limit int) queryAdmission {
	if limit < 2 {
		return queryAdmission{limit: limit}
	}
	perArm := min(admissionArmMax, max(1, limit/10))
	return queryAdmission{limit: limit, perArm: perArm,
		pinCap: min(pinCapMax, max(0, limit-2*perArm))}
}

// total is the total arm's rows: what remains of candidateHeadroom x N.
func (a queryAdmission) total(pins int) int {
	return candidateHeadroom*a.limit - 2*a.perArm - pins
}

// candidates is the read's row bound: every arm's rows, candidateHeadroom
// x N as before collection-07, so the text read stays as bounded.
func (a queryAdmission) candidates(pins int) int {
	return a.total(pins) + 2*a.perArm + pins
}

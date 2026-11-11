package collector

import (
	"context"
	"errors"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// ErrCycleSkipped is a cycle the circuit breaker skipped (load too high,
// or the load query failed): it collected nothing.
var ErrCycleSkipped = errors.New("collector cycle skipped")

// trackedCycle runs a cycle as one run of the collector loop: its busy
// time for the self-budget and, when it stored no snapshot, a failed run
// (observability-06).
func (c *Collector) trackedCycle(ctx context.Context, ticker intervalResetter) {
	done := c.loopTracker().TrackErr("collector")
	done(c.cycle(ctx, ticker))
}

// loopTracker is the process-wide loop tracker unless a test replaced it.
func (c *Collector) loopTracker() *selfbudget.Loops {
	if c.loops != nil {
		return c.loops
	}
	return selfbudget.Process()
}

package pressure

import (
	"context"
	"fmt"
	"time"
)

// walRate is the WAL bytes per second from the oldest position seen in
// the WAL window to pos. With none (the first read set, after a gap
// longer than the window, or after the position went back, as on a
// failover) it measures over WALFirstSpan instead.
func (c *Checker) walRate(ctx context.Context, pos Reading) Reading {
	if pos.State != StateKnown {
		reason := pos.Reason
		if reason == "" {
			reason = "WAL position not read"
		}
		return Unknown(reason)
	}
	oldest, ok := c.recordWAL(pos)
	if !ok {
		if err := c.o.Sleep(ctx, c.o.WALFirstSpan); err != nil {
			return Unknown("WAL rate not measured: " + err.Error())
		}
		next := c.o.Source.WALPosition(ctx)
		if next.State != StateKnown {
			return Unknown("WAL rate not measured: " + next.Reason)
		}
		if oldest, ok = c.recordWAL(next); !ok {
			return Unknown("WAL rate not measured: no earlier sample to compare (the " +
				"position went back or did not move in time)")
		}
		pos = next
	}
	dt := pos.At.Sub(oldest.at).Seconds()
	if dt <= 0 {
		return Unknown("no time passed between WAL samples")
	}
	return Known((pos.Value-oldest.pos)/dt, pos.At)
}

// recordWAL adds pos to the window and returns the oldest earlier sample
// still in it; false when there is none.
func (c *Checker) recordWAL(pos Reading) (walSample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.wal); n > 0 && pos.At.Before(c.wal[n-1].at) {
		// A concurrent read set recorded a later sample first: compare
		// against the window without reordering it.
		return c.wal[0], c.wal[0].at.Before(pos.At) && c.wal[0].pos <= pos.Value
	}
	if n := len(c.wal); n > 0 && pos.Value < c.wal[n-1].pos {
		c.wal = nil // the position went back: a new timeline
	}
	cutoff := pos.At.Add(-c.o.WALWindow)
	keep := c.wal[:0]
	for _, s := range c.wal {
		if !s.at.Before(cutoff) && !s.at.After(pos.At) {
			keep = append(keep, s)
		}
	}
	c.wal = keep
	var oldest walSample
	ok := len(c.wal) > 0 && c.wal[0].at.Before(pos.At)
	if ok {
		oldest = c.wal[0]
	}
	c.wal = append(c.wal, walSample{at: pos.At, pos: pos.Value})
	if len(c.wal) > maxWALSamples {
		c.wal = append(c.wal[:1], c.wal[len(c.wal)-maxWALSamples+1:]...)
	}
	return oldest, ok
}

// catchUp is how fast replica lag fell between the last two readings at
// least a second apart, in seconds of lag per second; false until two
// such readings exist or while lag is not known.
func (c *Checker) catchUp(r Reading) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.State != StateKnown {
		c.lag, c.rateOK = nil, false
		return 0, false
	}
	if c.lag == nil {
		c.lag = &lagSample{at: r.At, lag: r.Value}
		return 0, false
	}
	dt := r.At.Sub(c.lag.at)
	if dt < minLagInterval {
		return c.rate, c.rateOK
	}
	c.rate, c.rateOK = (c.lag.lag-r.Value)/dt.Seconds(), true
	c.lag = &lagSample{at: r.At, lag: r.Value}
	return c.rate, c.rateOK
}

// replayLag is a lower bound on the slowest replica's lag: the time from
// the oldest WAL sample in the window already past its replay position
// (bytes) to at; false when no sample is past it.
func (c *Checker) replayLag(replay Reading, at time.Time) (float64, bool) {
	if replay.State != StateKnown {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.wal {
		if s.pos > replay.Value {
			return max(0, at.Sub(s.at).Seconds()), true
		}
	}
	return 0, false
}

// boundLag raises a known lag reading to the WAL history's lower bound
// when that is larger: replay_lag only moves when a standby replies, so
// a paused or stuck replay can read far less than it is.
func (c *Checker) boundLag(lag, replay Reading) Reading {
	if lag.State != StateKnown {
		return lag
	}
	est, ok := c.replayLag(replay, lag.At)
	if !ok || est <= lag.Value {
		return lag
	}
	lag.Reason = fmt.Sprintf("estimated from the WAL position history: the primary "+
		"passed the slowest replica's replay position %s s ago; pg_stat_replication "+
		"reports %s s", number(est), number(lag.Value))
	lag.Value = est
	return lag
}

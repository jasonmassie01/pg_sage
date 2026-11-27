package lakebaseobs

import (
	"context"
	"errors"
	"time"
)

// cursorStore is the part of Store a poll's collection writes through.
type cursorStore interface {
	LoadCursor(context.Context, string, Stream) (Cursor, error)
	CommitBatch(context.Context, string, Stream, Cursor, Cursor, []Event, time.Duration) error
}

// narrowKey names one stream's pass whose read width ErrBudget narrowed.
type narrowKey struct {
	stream Stream
	pass   pass
}

// streamPoll is one stream's state during a poll. After the first error the
// stream reads nothing more this poll.
type streamPoll struct {
	stream Stream
	cursor Cursor
	loaded bool
	err    error
	reads  readCounters
}

// readCounters are one poll's warehouse reads for a stream.
type readCounters struct {
	statements, rows, bytes int64
	elapsed                 time.Duration
}

func (c *readCounters) add(b Batch, elapsed time.Duration) {
	c.statements++
	c.rows += int64(len(b.Events))
	c.bytes += int64(b.Bytes)
	c.elapsed += elapsed
}

// collect spends at most QueriesPerPoll reads: first forward for every
// stream, then one short reconcile read per stream, then deep-sweep rounds
// with what is left. A stream always keeps one forward read in reserve
// while earlier streams catch up, so no stream misses a poll.
func (r *Runtime) collect(ctx context.Context, cs cursorStore, now time.Time) []error {
	polls := r.loadStreams(ctx, cs, now)
	budget := r.options.QueriesPerPoll
	served := r.forward(ctx, cs, polls, now, &budget)
	r.reconcile(ctx, cs, polls, shortPass, now, &budget)
	for progressed := true; progressed && budget > 0; {
		progressed = r.reconcile(ctx, cs, polls, deepPass, now, &budget)
	}
	// Start the next poll at the first stream this one could not serve; when
	// every stream was served, rotate by one so catch-up surplus moves around.
	step := served % len(polls)
	if step == 0 {
		step = 1
	}
	r.next = (r.next + step) % len(polls)
	return r.publish(polls, now)
}

// loadStreams loads cursors in this poll's rotated priority order, so one
// permanently saturated source cannot starve the others.
func (r *Runtime) loadStreams(
	ctx context.Context, cs cursorStore, now time.Time,
) []*streamPoll {
	n := len(r.options.Streams)
	polls := make([]*streamPoll, 0, n)
	for i := 0; i < n; i++ {
		sp := &streamPoll{stream: r.options.Streams[(r.next+i)%n]}
		c, err := cs.LoadCursor(ctx, r.binding, sp.stream)
		if err != nil {
			sp.err = err
		} else {
			sp.cursor, sp.loaded = expireHistory(c, now), true
			hint, ok := r.narrowed[narrowKey{sp.stream, forwardPass}]
			if ok && hint < forwardWidth(sp.cursor, r.options) {
				sp.cursor.Width = hint
			}
		}
		polls = append(polls, sp)
	}
	return polls
}

// reserveFloor is the budget level a read may not spend below: later
// streams keep one read each, but a stream always gets one attempt.
func reserveFloor(budget, reserve int) int {
	if budget > reserve {
		return reserve
	}
	return budget - 1
}

// forward reports how many streams, in priority order, had budget left for
// their forward turn.
func (r *Runtime) forward(
	ctx context.Context, cs cursorStore, polls []*streamPoll, now time.Time, budget *int,
) int {
	served := 0
	for i, sp := range polls {
		if *budget > 0 {
			served = i + 1
		}
		reserve := len(polls) - i - 1
		for n := 0; sp.err == nil && *budget > 0; n++ {
			w, ok := nextWindow(sp.cursor, forwardPass, now, r.options)
			if !ok {
				break
			}
			// Catch-up reads take only full windows from budget no later
			// stream needs; a partial tail waits for the next poll.
			partial := w.To.Sub(w.From) < forwardWidth(sp.cursor, r.options)
			if n > 0 && (*budget <= reserve || partial) {
				break
			}
			r.step(ctx, cs, sp, forwardPass, w, now, budget, reserveFloor(*budget, reserve))
		}
	}
	return served
}

// reconcile gives each stream at most one read of pass p and reports
// whether any stream committed one.
func (r *Runtime) reconcile(
	ctx context.Context, cs cursorStore, polls []*streamPoll, p pass, now time.Time, budget *int,
) bool {
	progressed := false
	for i, sp := range polls {
		if sp.err != nil || *budget <= 0 {
			continue
		}
		w, ok := nextWindow(sp.cursor, p, now, r.options)
		if !ok {
			continue
		}
		w = r.narrow(sp.stream, p, w)
		before := sp.cursor.Revision
		r.step(ctx, cs, sp, p, w, now, budget, reserveFloor(*budget, len(polls)-i-1))
		progressed = progressed || sp.cursor.Revision != before
	}
	return progressed
}

// step reads w, halving it on ErrBudget, then commits the batch with the
// advanced cursor through the store's compare-and-swap.
func (r *Runtime) step(
	ctx context.Context, cs cursorStore, sp *streamPoll, p pass, w Window, now time.Time,
	budget *int, floor int,
) {
	b, w, err := r.read(ctx, sp, p, w, budget, floor)
	if err == nil {
		err = r.checkBatch(sp.stream, w, b)
	}
	if err != nil {
		sp.err = err
		return
	}
	after := advance(sp.cursor, p, w, now, r.options)
	if p == forwardPass {
		after = adapt(after, w, len(b.Events), r.options)
	}
	for _, e := range b.Events {
		after.LastEvent = later(after.LastEvent, e.OccurredAt)
	}
	if err = r.commit(ctx, cs, sp, after, b.Events); err != nil {
		sp.err = err
		return
	}
	after.Revision = sp.cursor.Revision + 1
	sp.cursor = after
	if p == forwardPass {
		delete(r.narrowed, narrowKey{sp.stream, p})
	} else {
		r.widen(sp, p, w, len(b.Events))
	}
}

// narrow caps a reconcile read at the width an earlier ErrBudget left, so a
// dense stretch of history is split across polls instead of retried whole.
func (r *Runtime) narrow(s Stream, p pass, w Window) Window {
	if hint, ok := r.narrowed[narrowKey{s, p}]; ok && w.To.Sub(w.From) > hint {
		w.To = w.From.Add(hint)
	}
	return w
}

// widen doubles a narrowed reconcile width after a read projected under
// MaxRows/4, and forgets it once it reaches the pass's own width.
func (r *Runtime) widen(sp *streamPoll, p pass, w Window, rows int) {
	key := narrowKey{sp.stream, p}
	hint, ok := r.narrowed[key]
	span := w.To.Sub(w.From)
	quarter := float64(r.options.MaxRows) / 4 * float64(span)
	if !ok || span <= 0 || float64(rows)*float64(hint) >= quarter {
		return
	}
	limit := SourceRetention
	if p == shortPass {
		limit = r.options.ShortLookback
	}
	if hint *= 2; hint >= reconcileWidth(sp.cursor, r.options, limit) {
		delete(r.narrowed, key)
		return
	}
	r.narrowed[key] = hint
}

// read spends one budget unit per attempt. Every ErrBudget halving is
// remembered per stream and pass, so a read stopped by the budget floor
// resumes narrower on the next poll instead of retrying the same window.
func (r *Runtime) read(
	ctx context.Context, sp *streamPoll, p pass, w Window, budget *int, floor int,
) (Batch, Window, error) {
	for {
		if *budget <= floor {
			return Batch{}, w, ErrBudget
		}
		*budget--
		started := time.Now()
		b, err := r.reader.ReadWindow(ctx, sp.stream, r.scope, w)
		sp.reads.add(b, time.Since(started))
		if !errors.Is(err, ErrBudget) {
			return b, w, err
		}
		if w.To.Sub(w.From) <= time.Millisecond {
			return Batch{}, w, ErrBudget
		}
		w.To = w.From.Add(w.To.Sub(w.From) / 2)
		if p == forwardPass {
			sp.cursor.Width, sp.cursor.Sparse = w.To.Sub(w.From), 0
		}
		r.narrowed[narrowKey{sp.stream, p}] = w.To.Sub(w.From)
	}
}

func (r *Runtime) checkBatch(stream Stream, w Window, b Batch) error {
	hash := r.scope.Hash()
	for _, e := range b.Events {
		inside := !e.OccurredAt.Before(w.From) && e.OccurredAt.Before(w.To)
		if e.ScopeHash != hash || e.Stream != stream || !inside {
			return ErrScope
		}
	}
	return nil
}

// publish records each stream's outcome in Status and returns the errors.
func (r *Runtime) publish(polls []*streamPoll, now time.Time) []error {
	var errs []error
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sp := range polls {
		st := r.statuses[sp.stream]
		st.Statements += sp.reads.statements
		st.Rows += sp.reads.rows
		st.Bytes += sp.reads.bytes
		st.ReadMillis += sp.reads.elapsed.Milliseconds()
		st.LastError = ""
		if sp.err != nil {
			st.LastError = safeError(sp.err)
			errs = append(errs, sp.err)
		}
		if sp.loaded {
			st.Cursor = sp.cursor
			st.ForwardLagSeconds = forwardLag(sp.cursor, now)
			st.ReconcileProgress = deepProgress(sp.cursor)
		}
		r.statuses[sp.stream] = st
	}
	return errs
}

func forwardLag(c Cursor, now time.Time) *int64 {
	if c.ForwardThrough.IsZero() {
		return nil
	}
	lag := int64(now.Sub(c.ForwardThrough) / time.Second)
	return &lag
}

// deepProgress is the share of the current deep sweep read, from the
// horizon start (target minus SourceRetention) to its pinned target.
func deepProgress(c Cursor) float64 {
	if c.ReconcileTarget.IsZero() {
		return 0
	}
	if deepDone(c) {
		return 1
	}
	start := c.ReconcileTarget.Add(-SourceRetention)
	done := c.ReconcileThrough.Sub(start).Seconds() / SourceRetention.Seconds()
	return min(max(done, 0), 1)
}

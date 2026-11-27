package lakebaseobs

import "time"

// pass is one kind of read a poll makes for a stream. Forward reads follow
// wall-clock time behind SettleLag; the short pass re-reads the ShortLookback
// behind the forward edge for late rows; the deep pass sweeps the whole
// source horizon, starting at most once per deepInterval.
type pass int

const (
	forwardPass pass = iota
	shortPass
	deepPass
)

const (
	// sparseReads consecutive reads projected under MaxRows/4 double the width.
	sparseReads = 3
	// sparseFactor widens reconcile reads of a stream that is sparse at Window.
	sparseFactor = 4
	deepInterval = 24 * time.Hour
)

func forwardWidth(c Cursor, o Options) time.Duration {
	if c.Width <= 0 || c.Width > o.Window {
		return o.Window
	}
	return c.Width
}

func reconcileWidth(c Cursor, o Options, limit time.Duration) time.Duration {
	w := forwardWidth(c, o)
	if w == o.Window && c.Sparse >= sparseReads {
		w *= sparseFactor
	}
	return min(w, limit)
}

func shortDone(c Cursor) bool {
	return c.ShortTarget.IsZero() || !c.ShortThrough.Before(c.ShortTarget)
}

func deepDone(c Cursor) bool {
	return c.ReconcileTarget.IsZero() || !c.ReconcileThrough.Before(c.ReconcileTarget)
}

// deepDue also restarts when the clock moved behind the last start.
func deepDue(c Cursor, now time.Time) bool {
	return c.DeepStartedAt.IsZero() || c.DeepStartedAt.After(now) ||
		now.Sub(c.DeepStartedAt) >= deepInterval
}

// shortTop is the upper edge a new short sweep pins: what forward has read.
func shortTop(c Cursor, now time.Time, o Options) time.Time {
	return earlier(c.ForwardThrough, now.Add(-o.SettleLag))
}

// nextWindow plans the next read of one pass. It reports false when the pass
// has nothing to read for this stream at now.
func nextWindow(c Cursor, p pass, now time.Time, o Options) (Window, bool) {
	var from, to time.Time
	switch p {
	case forwardPass:
		end := now.Add(-o.SettleLag)
		from = c.ForwardThrough
		if from.IsZero() || from.After(now) {
			from = end.Add(-o.Window)
		}
		to = earlier(from.Add(forwardWidth(c, o)), end)
	case shortPass:
		from, to = c.ShortThrough, c.ShortTarget
		if shortDone(c) {
			to = shortTop(c, now, o)
			from = to.Add(-o.ShortLookback)
		}
		to = earlier(from.Add(reconcileWidth(c, o, o.ShortLookback)), to)
	case deepPass:
		from, to = c.ReconcileThrough, c.ReconcileTarget
		if deepDone(c) {
			if !deepDue(c, now) {
				return Window{}, false
			}
			from, to = now.Add(-SourceRetention), c.ForwardThrough
		}
		to = earlier(from.Add(reconcileWidth(c, o, SourceRetention)), to)
	}
	from = later(from, now.Add(-SourceRetention))
	if !to.After(from) {
		return Window{}, false
	}
	return Window{from, to}, true
}

// advance records a committed read of w in the pass's cursor fields.
func advance(c Cursor, p pass, w Window, now time.Time, o Options) Cursor {
	switch p {
	case forwardPass:
		if w.To.After(c.ForwardThrough) || c.ForwardThrough.After(now) {
			c.ForwardThrough = w.To
		}
	case shortPass:
		if shortDone(c) {
			c.ShortTarget = shortTop(c, now, o)
		}
		c.ShortThrough = w.To
	case deepPass:
		if deepDone(c) {
			c.ReconcileTarget, c.DeepStartedAt = c.ForwardThrough, now
		}
		c.ReconcileThrough = w.To
	}
	c.LastPoll = now
	return c
}

// adapt tunes the forward width from a successful read of w holding rows.
// Rows are projected to the full width so a short tail window cannot pass
// as sparse evidence for a busy stream, nor a busy one as sparse.
func adapt(c Cursor, w Window, rows int, o Options) Cursor {
	width := forwardWidth(c, o)
	c.Width = width
	span := w.To.Sub(w.From)
	if span <= 0 || float64(rows)*float64(width) >= float64(o.MaxRows)/4*float64(span) {
		c.Sparse = 0
		return c
	}
	c.Sparse = min(c.Sparse+1, sparseReads)
	if c.Sparse >= sparseReads && width < o.Window {
		c.Width, c.Sparse = min(2*width, o.Window), 0
	}
	return c
}

// expireHistory counts each cursor edge the source horizon has passed and
// moves it to the oldest readable instant. A zero cursor has no gap.
func expireHistory(c Cursor, now time.Time) Cursor {
	oldest := now.Add(-SourceRetention)
	if !c.LastPoll.IsZero() && c.LastPoll.Before(oldest) {
		c.GapCount++
		c.LastGap = "source retention exceeded during outage"
	}
	if !c.ForwardThrough.IsZero() && c.ForwardThrough.Before(oldest) {
		c.ForwardThrough = oldest
		c.GapCount++
		c.LastGap = "unread history expired before reconciliation"
	}
	if !deepDone(c) && c.ReconcileThrough.Before(oldest) {
		c.ReconcileThrough = oldest
		c.GapCount++
		c.LastGap = "unread history expired before reconciliation"
	}
	return c
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

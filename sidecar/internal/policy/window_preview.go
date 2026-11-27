package policy

import "time"

// PreviewHorizon is how far ahead a window preview looks: eight days, so
// every day-of-week pattern opens inside it at least once.
const PreviewHorizon = 8 * 24 * time.Hour

// WindowPreview says when a window is open, seen from one instant. Every
// zero time means "not within the horizon". Times are in the window's time
// zone, or the instant's own location when the window names none.
type WindowPreview struct {
	// OpenNow reports whether the instant is inside the window.
	OpenNow bool
	// ClosesAt is when the window open at the instant closes.
	ClosesAt time.Time
	// NextOpen and NextClose bound the next opening that starts after the
	// instant (after the current opening closes, when one is open now).
	NextOpen  time.Time
	NextClose time.Time
}

// Preview reports the window's state at from and its next opening within
// horizon. Transitions are found at whole minutes, the window grammar's
// resolution; the scan is bounded by horizon.
func (w Window) Preview(from time.Time, horizon time.Duration) WindowPreview {
	if w.location != nil {
		from = from.In(w.location)
	}
	end := from.Add(horizon)
	preview := WindowPreview{OpenNow: w.Contains(from)}
	searchFrom := from
	if preview.OpenNow {
		preview.ClosesAt = w.nextTransition(from, end, false)
		if preview.ClosesAt.IsZero() {
			return preview
		}
		searchFrom = preview.ClosesAt
	}
	preview.NextOpen = w.nextTransition(searchFrom, end, true)
	if !preview.NextOpen.IsZero() {
		preview.NextClose = w.nextTransition(preview.NextOpen, end, false)
	}
	return preview
}

// nextTransition returns the first whole minute after at, and no later
// than end, whose openness equals open; zero when there is none.
func (w Window) nextTransition(at, end time.Time, open bool) time.Time {
	for t := at.Truncate(time.Minute).Add(time.Minute); !t.After(end); t = t.Add(time.Minute) {
		if w.Contains(t) == open {
			return t
		}
	}
	return time.Time{}
}

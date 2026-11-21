package notify

import (
	"fmt"
	"strings"
	"time"
)

// Per-channel quiet hours (ux-09b). A channel may hold its own window,
// HH:MM to HH:MM in a timezone, set on the channel form. Inside it the
// channel's Ask and Digest deliveries wait until the window ends; a Page
// goes out at once. A channel without its own window keeps the global
// alerting.quiet_hours_* pair, which holds non-critical finding alerts
// only (alerting-06). A digest, otherwise sent at once, waits in the outbox
// while its channel is quiet; an interactive card sent at once (its
// buttons need the live token) is not held.

// QuietWindow is one quiet window; the zero value holds nothing.
type QuietWindow struct {
	start, end int // minute of day
	loc        *time.Location
	set        bool
}

// ParseQuietHours reads a channel's quiet hours. All three empty is no
// window. start and end are both needed and must differ; tz is an IANA
// zone name, UTC when empty. Each error names the input it refuses.
func ParseQuietHours(start, end, tz string) (QuietWindow, error) {
	start, end, tz = strings.TrimSpace(start), strings.TrimSpace(end), strings.TrimSpace(tz)
	loc := time.UTC
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return QuietWindow{}, fmt.Errorf("unknown quiet hours timezone %q: use an IANA "+
				"zone such as America/Chicago", tz)
		}
		loc = l
	}
	if start == "" && end == "" {
		return QuietWindow{}, nil
	}
	if start == "" || end == "" {
		return QuietWindow{}, fmt.Errorf("quiet hours need both a start and an end "+
			"(got %q to %q)", start, end)
	}
	s, okS := parseClock(start)
	e, okE := parseClock(end)
	switch {
	case !okS:
		return QuietWindow{}, fmt.Errorf("quiet hours start %q is not HH:MM", start)
	case !okE:
		return QuietWindow{}, fmt.Errorf("quiet hours end %q is not HH:MM", end)
	case s == e:
		return QuietWindow{}, fmt.Errorf("quiet hours start %q and end %q are equal: "+
			"leave both empty for no quiet hours", start, end)
	}
	return QuietWindow{start: s, end: e, loc: loc, set: true}, nil
}

// Set reports whether w is a window.
func (w QuietWindow) Set() bool { return w.set }

// Until is the end of the window that contains t, or t outside it.
func (w QuietWindow) Until(t time.Time) time.Time {
	if !w.set {
		return t
	}
	return quietWindowEnd(t, w.start, w.end, w.loc)
}

// quietWindowEnd returns the end of the start-end window (minutes of day
// in loc, possibly crossing midnight) that contains t, or t outside it.
// The end is a local wall time, so a DST night is held for its real
// length.
func quietWindowEnd(t time.Time, start, end int, loc *time.Location) time.Time {
	local := t.In(loc)
	minute := local.Hour()*60 + local.Minute()
	inside := minute >= start && minute < end
	if start > end {
		inside = minute >= start || minute < end
	}
	if !inside {
		return t
	}
	at := time.Date(local.Year(), local.Month(), local.Day(), end/60, end%60, 0, 0, loc)
	if !at.After(local) {
		at = time.Date(local.Year(), local.Month(), local.Day()+1, end/60, end%60, 0, 0, loc)
	}
	return at.UTC()
}

// channelQuiet is ch's own window; a stored window that no longer parses
// is logged and holds nothing (delivery wins over silence).
func (d *Dispatcher) channelQuiet(ch Channel) QuietWindow {
	w, err := ParseQuietHours(ch.QuietStart, ch.QuietEnd, ch.QuietTZ)
	if err != nil {
		d.logFn("WARN", "channel %d: ignoring its quiet hours: %v", ch.ID, err)
		return QuietWindow{}
	}
	return w
}

// quietHold is when ch may deliver evt queued for at: a Page (and every
// never-muted message) at once, anything else after ch's own window.
func (d *Dispatcher) quietHold(ch Channel, evt Event, at time.Time) time.Time {
	if ClassOf(evt) == ClassPage {
		return at
	}
	return d.channelQuiet(ch).Until(at)
}

// heldByQuiet reports whether a message sent at once (the digest) waits in
// the outbox for the end of ch's own quiet hours instead. A card with live
// buttons is never held: the outbox cannot keep its token.
func (d *Dispatcher) heldByQuiet(ch Channel, evt Event) bool {
	if carriesSyncData(evt) {
		return false
	}
	now := d.clock()
	return d.quietHold(ch, evt, now).After(now)
}

// policyFor is the alert policy for ch: the global one, with ch's own
// quiet window in place of the global pair when it has one.
func (d *Dispatcher) policyFor(ch Channel) AlertPolicy {
	p := d.alertPolicy()
	if w := d.channelQuiet(ch); w.set {
		p.quietStart, p.quietEnd, p.loc = w.start, w.end, w.loc
	}
	return p
}

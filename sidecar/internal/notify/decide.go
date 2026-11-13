package notify

import (
	"fmt"
	"time"
)

// throttledEvents are the finding alerts the alerting policy governs.
// Other events (incidents, actions, approvals) are delivered through the
// outbox without cooldown or quiet hours, as before (alerting-06).
var throttledEvents = map[string]bool{
	"finding_critical": true,
	"finding_opened":   true,
	"finding_resolved": true,
}

// Throttled reports whether events of type typ follow the alerting
// policy (cooldown, quiet hours, reminders).
func Throttled(typ string) bool { return throttledEvents[typ] }

// Decision is the policy's verdict for one alert on one channel. At is
// when to deliver: now, or later for quiet hours or a cooldown.
type Decision struct {
	Suppressed bool
	At         time.Time
	Reminder   bool
	// CancelPending drops a trigger still waiting in the outbox: the
	// alert resolved before it was ever delivered.
	CancelPending bool
	Reason        string
}

func suppressed(reason string) Decision { return Decision{Suppressed: true, Reason: reason} }

// Decide classifies evt against the channel's state st (nil: none).
func (p AlertPolicy) Decide(st *NotifyState, evt Event, now time.Time) Decision {
	if evt.Resolve {
		return p.decideResolve(st, evt, now)
	}
	if st == nil {
		return p.at(evt.Severity, now)
	}
	if !st.Open {
		// A new episode waits out the cooldown since the last delivery:
		// flap protection, deferred rather than dropped.
		return p.at(evt.Severity, maxTime(now, st.LastSentAt.Add(p.cooldownFor(evt.Severity))))
	}
	if severityRank(evt.Severity) > severityRank(st.Severity) {
		// An escalation supersedes a lower trigger still waiting in the
		// outbox (quiet hours): the channel gets the new severity only.
		d := p.at(evt.Severity, now)
		d.CancelPending = true
		return d
	}
	return suppressed("already alerted; still open")
}

func (p AlertPolicy) decideResolve(st *NotifyState, evt Event, now time.Time) Decision {
	switch {
	case st == nil && evt.Type == "finding_resolved":
		return suppressed("never alerted on this channel")
	case st == nil:
		// Paged before per-channel state existed: still close it.
		return p.at(evt.Severity, now)
	case !st.Open:
		return suppressed("already resolved")
	case st.LastSentAt.After(now):
		return Decision{Suppressed: true, CancelPending: true, At: now,
			Reason: "resolved before the deferred alert was delivered"}
	}
	return p.at(evt.Severity, now)
}

// DecideReminder is due when the alert is open, its severity has a
// repeat interval and that interval has passed since the last delivery.
func (p AlertPolicy) DecideReminder(st *NotifyState, now time.Time) Decision {
	if st == nil || !st.Open {
		return suppressed("not open")
	}
	every := p.repeatFor(st.Severity)
	if every <= 0 {
		return suppressed("no repeat interval")
	}
	if now.Sub(st.LastSentAt) < every {
		return suppressed("repeat interval not reached")
	}
	d := p.at(st.Severity, now)
	d.Reminder = true
	return d
}

// at schedules a delivery of severity at t, moved to the end of quiet
// hours unless it is critical.
func (p AlertPolicy) at(severity string, t time.Time) Decision {
	if severity != "critical" {
		t = p.quietUntil(t)
	}
	return Decision{At: t}
}

// Apply returns the state after a delivery decided by d. A suppressed
// decision leaves st unchanged, except a cancelled pending alert closes.
func (p AlertPolicy) Apply(st *NotifyState, evt Event, d Decision) *NotifyState {
	if d.Suppressed {
		if d.CancelPending && st != nil {
			next := *st
			next.Open, next.ResolvedAt = false, d.At
			return &next
		}
		return st
	}
	next := NotifyState{}
	if st != nil {
		next = *st
	}
	if evt.Resolve {
		next.Open, next.ResolvedAt = false, d.At
		return &next
	}
	if !next.Open {
		next.FirstSentAt, next.Reminders = d.At, 0
	}
	if d.Reminder {
		next.Reminders++
	}
	next.Open, next.LastSentAt = true, d.At
	if evt.Severity != "" {
		next.Severity = evt.Severity
	}
	return &next
}

// reminderEvent is the open alert's last trigger, headed as a reminder.
func reminderEvent(st NotifyState, now time.Time) (Event, error) {
	evt, err := decodeEvent(st.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("reminder for %s: %w", st.Key.DedupKey, err)
	}
	openFor := now.Sub(st.FirstSentAt).Round(time.Minute)
	evt.Subject = fmt.Sprintf("Still open for %s: %s", openFor, evt.Subject)
	if evt.Data == nil {
		evt.Data = map[string]any{}
	}
	evt.Data["reminder"] = true
	evt.Data["open_for_seconds"] = int64(openFor.Seconds())
	return evt, nil
}

// severityRank orders severities, higher is more severe.
func severityRank(sev string) int { return ValidSeverities[sev] }

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

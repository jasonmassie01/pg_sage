package notify

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// StateKey is the identity of one alert on one channel: the database it
// concerns, its dedup key and the channel (alerting-06). Two databases
// emitting the same dedup key never share state.
type StateKey struct {
	Database  string
	DedupKey  string
	ChannelID int
}

// NotifyState is what a channel was last told about one alert
// (sage.notification_state). LastSentAt is when the latest delivery was
// scheduled, which is in the future while it waits out quiet hours.
type NotifyState struct {
	Key         StateKey
	Open        bool
	Severity    string
	FirstSentAt time.Time
	LastSentAt  time.Time
	ResolvedAt  time.Time
	Reminders   int
	// Payload is the last trigger event, re-sent as a reminder.
	Payload []byte
}

// AlertPolicy is cooldown, quiet hours and repeat intervals for finding
// alerts, built from the alerting.* settings.
type AlertPolicy struct {
	cooldown   map[string]time.Duration
	repeat     map[string]time.Duration
	quietStart int // minute of day; -1 disables quiet hours
	quietEnd   int
	loc        *time.Location
}

// NewAlertPolicy builds a policy with the alerting.* semantics: the
// per-severity cooldown floors (critical 5m, warning 30m, info 6h) raised
// by cooldownMinutes, repeat intervals with their defaults (0 = never),
// and quiet hours HH:MM in tz. Invalid values are reported and disabled.
func NewAlertPolicy(
	cooldownMinutes int, repeatMinutes map[string]int, quietStart, quietEnd, tz string,
) (AlertPolicy, []string) {
	p := AlertPolicy{cooldown: map[string]time.Duration{}, repeat: map[string]time.Duration{},
		quietStart: -1, quietEnd: -1, loc: time.UTC}
	var warns []string
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			p.loc = loc
		} else {
			warns = append(warns, fmt.Sprintf("invalid alerting timezone %q, using UTC", tz))
		}
	}
	for _, sev := range []string{"critical", "warning", "info"} {
		p.cooldown[sev] = time.Duration(
			config.AlertCooldownFloorMinutes(sev, cooldownMinutes)) * time.Minute
	}
	for sev, m := range config.ResolveAlertRepeatMinutes(repeatMinutes) {
		if m > 0 {
			p.repeat[sev] = time.Duration(m) * time.Minute
		}
	}
	start, okS := parseClock(quietStart)
	end, okE := parseClock(quietEnd)
	switch {
	case quietStart == "" && quietEnd == "":
	case !okS || !okE || start == end:
		warns = append(warns, fmt.Sprintf(
			"invalid quiet hours %q-%q, quiet hours disabled", quietStart, quietEnd))
	default:
		p.quietStart, p.quietEnd = start, end
	}
	return p, warns
}

// parseClock reads HH:MM (or HH) like the YAML stack's quiet hours.
func parseClock(s string) (int, bool) {
	m := config.QuietHourMinute(s)
	return m, m >= 0
}

func (p AlertPolicy) cooldownFor(sev string) time.Duration {
	if d, ok := p.cooldown[sev]; ok {
		return d
	}
	return p.cooldown["info"]
}

// repeatFor is the reminder interval, 0 for never, never below the cooldown.
func (p AlertPolicy) repeatFor(sev string) time.Duration {
	d, ok := p.repeat[sev]
	if !ok {
		d = p.repeat["info"]
	}
	if d <= 0 {
		return 0
	}
	return max(d, p.cooldownFor(sev))
}

// quietUntil returns the end of the quiet window that contains t, or t
// itself outside quiet hours. The window may cross midnight.
func (p AlertPolicy) quietUntil(t time.Time) time.Time {
	if p.quietStart < 0 || p.loc == nil {
		return t
	}
	return quietWindowEnd(t, p.quietStart, p.quietEnd, p.loc)
}

// reminderCutoffs maps each severity that reminds to the latest last
// delivery whose reminder is due at now. Severities that never remind are
// left out, so their open alerts cannot fill the bounded reminder scan.
func (p AlertPolicy) reminderCutoffs(now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for _, sev := range []string{"critical", "warning", "info"} {
		if d := p.repeatFor(sev); d > 0 {
			out[sev] = now.Add(-d)
		}
	}
	return out
}

// String describes the policy for logs: cooldowns, repeat intervals and
// the quiet window.
func (p AlertPolicy) String() string {
	quiet := "none"
	if p.quietStart >= 0 {
		quiet = fmt.Sprintf("%02d:%02d-%02d:%02d %s", p.quietStart/60, p.quietStart%60,
			p.quietEnd/60, p.quietEnd%60, p.loc)
	}
	return fmt.Sprintf("cooldown critical=%s warning=%s info=%s; repeat critical=%s "+
		"warning=%s info=%s; quiet %s", p.cooldownFor("critical"), p.cooldownFor("warning"),
		p.cooldownFor("info"), p.repeatFor("critical"), p.repeatFor("warning"),
		p.repeatFor("info"), quiet)
}

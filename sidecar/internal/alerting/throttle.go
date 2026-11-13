package alerting

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Decision is the throttle's verdict for one alert.
type Decision int

const (
	// DecisionSend: deliver now.
	DecisionSend Decision = iota
	// DecisionThrottled: an equal-or-worse alert went out within the
	// cooldown; drop this one.
	DecisionThrottled
	// DecisionDefer: quiet hours; keep the alert pending and deliver it
	// once quiet hours end (G7-B19).
	DecisionDefer
	// DecisionRemind: the finding is still open and its repeat interval
	// has passed since the last delivery; send a reminder (alerting-07).
	DecisionRemind
)

// Throttle tracks deduplication, cooldown and reminders for alerts.
//
// A key's first alert of an episode (one finding row) is sent, then the
// same episode is throttled until its severity's repeat interval passes
// and a reminder is due; repeat 0 never reminds. An escalation is sent at
// once. A new episode of the key (resolved, then reopened as a new row)
// waits out the cooldown, which is flap protection (alerting-07).
type Throttle struct {
	mu         sync.Mutex
	sent       map[string]sentRecord
	cooldown   map[string]time.Duration
	repeat     map[string]time.Duration
	retain     time.Duration // longest retainFor over the severities
	quietStart int           // minute of day 0-1439, -1 = disabled
	quietEnd   int
	timezone   *time.Location
	now        func() time.Time
	warnings   []string
}

// sentRecord is the last delivery of a key. episode is the finding id
// (0 = unknown, matches any); first is the episode's first delivery,
// reminders the deliveries since; seen is the last decision or delivery.
type sentRecord struct {
	at        time.Time
	first     time.Time
	seen      time.Time
	severity  string
	episode   int64
	reminders int
}

// severityRank returns a numeric rank (lower = more severe).
func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

// NewThrottle creates a Throttle with severity-based cooldowns. Invalid
// timezone or quiet-hour values are reported by Warnings.
func NewThrottle(
	cooldownMinutes int,
	quietStart, quietEnd string,
	tz string,
) *Throttle {
	t := &Throttle{sent: make(map[string]sentRecord), timezone: time.UTC,
		now: time.Now}
	if tz != "" {
		if parsed, err := time.LoadLocation(tz); err == nil {
			t.timezone = parsed
		} else {
			t.warnings = append(t.warnings, fmt.Sprintf(
				"invalid alerting timezone %q, using UTC: %v", tz, err))
		}
	}
	t.cooldown = make(map[string]time.Duration, 3)
	for _, sev := range []string{"critical", "warning", "info"} {
		t.cooldown[sev] = time.Duration(
			config.AlertCooldownFloorMinutes(sev, cooldownMinutes)) * time.Minute
	}
	t.setRepeat(nil)
	t.quietStart = t.parseMinute("quiet_hours_start", quietStart)
	t.quietEnd = t.parseMinute("quiet_hours_end", quietEnd)
	return t
}

// Reconfigure applies new cooldown, repeat, quiet-hour and timezone
// settings to a running throttle and returns their warnings; invalid
// values are disabled as at construction. Sent records are kept, so an
// alert that is open stays throttled (alerting-06).
func (t *Throttle) Reconfigure(
	cooldownMinutes int, repeat map[string]int, quietStart, quietEnd, tz string,
) []string {
	next := NewThrottle(cooldownMinutes, quietStart, quietEnd, tz)
	next.setRepeat(repeat)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cooldown, t.repeat, t.retain = next.cooldown, next.repeat, next.retain
	t.quietStart, t.quietEnd, t.timezone = next.quietStart, next.quietEnd, next.timezone
	t.warnings = next.warnings
	return next.warnings
}

// Warnings returns configuration problems found at construction.
func (t *Throttle) Warnings() []string { return t.warnings }

// setRepeat sets the repeat intervals in minutes; a missing severity
// takes its default (config.ResolveAlertRepeatMinutes) and a value of 0
// or less never reminds. retain becomes the longest interval, and at
// least the info cooldown.
func (t *Throttle) setRepeat(minutes map[string]int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.repeat = make(map[string]time.Duration, 3)
	t.retain = t.cooldownFor("info")
	for sev, m := range config.ResolveAlertRepeatMinutes(minutes) {
		if m <= 0 {
			t.repeat[sev] = 0
			continue
		}
		d := time.Duration(m) * time.Minute
		t.repeat[sev] = d
		t.retain = maxDuration(t.retain, d)
	}
}

// repeatFor is severity's reminder interval, 0 for never; never shorter
// than its cooldown. An unknown severity uses info's.
func (t *Throttle) repeatFor(severity string) time.Duration {
	d, ok := t.repeat[severity]
	if !ok {
		d = t.repeat["info"]
	}
	if d <= 0 {
		return 0
	}
	return maxDuration(d, t.cooldownFor(severity))
}

// retainFor is how long a record of severity survives unobserved: its
// repeat interval, and at least the info cooldown so a reopen inside any
// cooldown is still throttled.
func (t *Throttle) retainFor(severity string) time.Duration {
	return maxDuration(t.repeatFor(severity), t.cooldownFor("info"))
}

// SeedWindow is how far back a restart must read sage.alert_log for
// findings no longer open: the retention plus an hour of slack.
func (t *Throttle) SeedWindow() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.retain + time.Hour
}

// Decide classifies an alert for key without an episode identity.
func (t *Throttle) Decide(key, severity string) Decision {
	d, _ := t.DecideEpisode(key, severity, 0)
	return d
}

// DecideEpisode classifies an alert of episode (the finding id, 0 when
// unknown) and, for a reminder, returns how long ago the episode was
// first delivered. Critical alerts bypass quiet hours; other alerts that
// would go out are deferred, never dropped, during quiet hours.
func (t *Throttle) DecideEpisode(
	key, severity string, episode int64,
) (Decision, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	d, openFor := t.classify(key, severity, episode, now)
	if d != DecisionThrottled && severity != "critical" && t.isQuietHours(now) {
		return DecisionDefer, 0
	}
	return d, openFor
}

func (t *Throttle) classify(
	key, severity string, episode int64, now time.Time,
) (Decision, time.Duration) {
	rec, exists := t.sent[key]
	if !exists {
		return DecisionSend, 0
	}
	rec.seen = now
	t.sent[key] = rec
	// Escalation: lower rank = more severe.
	if severityRank(severity) < severityRank(rec.severity) {
		return DecisionSend, 0
	}
	if !sameEpisode(rec.episode, episode) {
		if now.Sub(rec.at) >= t.cooldownFor(severity) {
			return DecisionSend, 0
		}
		return DecisionThrottled, 0
	}
	if every := t.repeatFor(severity); every > 0 && now.Sub(rec.at) >= every {
		return DecisionRemind, now.Sub(rec.first)
	}
	return DecisionThrottled, 0
}

func sameEpisode(a, b int64) bool { return a == 0 || b == 0 || a == b }

// endedEpisode marks a record whose finding resolved: no finding id
// matches it, so the same row reopened is alerted as new.
const endedEpisode int64 = -1

// endEpisode records that episode (a finding id) of key resolved. A row
// an action resolved can reopen with the same id when the condition
// persists; it is then alerted again once the cooldown has passed, not
// left silent until the next reminder. Another episode's record is kept.
func (t *Throttle) endEpisode(key string, episode int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.sent[key]
	if !ok || episode == 0 || !sameEpisode(rec.episode, episode) {
		return
	}
	rec.episode = endedEpisode
	t.sent[key] = rec
}

// ShouldAlert returns true if the key should fire an alert or a
// reminder now.
func (t *Throttle) ShouldAlert(key, severity string) bool {
	d := t.Decide(key, severity)
	return d == DecisionSend || d == DecisionRemind
}

func (t *Throttle) cooldownFor(severity string) time.Duration {
	if cd, ok := t.cooldown[severity]; ok {
		return cd
	}
	return t.cooldown["info"]
}

// Record marks a key as sent now, with no episode identity.
func (t *Throttle) Record(key, severity string) {
	t.RecordAt(key, severity, time.Time{})
}

// RecordEpisode marks episode (the finding id) of key as delivered now.
// A delivery of the same episode keeps its first delivery time.
func (t *Throttle) RecordEpisode(key, severity string, episode int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.put(key, severity, episode, time.Time{}, now, now)
}

// RecordAt marks a key as sent at ts (zero = now), with no episode.
func (t *Throttle) RecordAt(key, severity string, ts time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if ts.IsZero() {
		ts = now
	}
	t.put(key, severity, 0, time.Time{}, ts, now)
}

// restore seeds a key from sage.alert_log after a restart with its
// episode's first and last delivery.
func (t *Throttle) restore(key, severity string, episode int64, first, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.put(key, severity, episode, first, at, t.now())
}

// put stores a delivery at at; a zero first keeps the record's first
// delivery when the episode matches, else starts at at. Records not
// observed within the retention are evicted first, so the map stays
// bounded while a still-open finding keeps its record (G7-B32).
func (t *Throttle) put(key, severity string, episode int64, first, at, now time.Time) {
	for k, rec := range t.sent {
		if now.Sub(rec.seen) >= t.retainFor(rec.severity) {
			delete(t.sent, k)
		}
	}
	rec, exists := t.sent[key]
	next := sentRecord{at: at, first: first, seen: now, severity: severity,
		episode: episode}
	if exists && sameEpisode(rec.episode, episode) {
		next.reminders = rec.reminders + 1
		if first.IsZero() {
			next.first = rec.first
		}
		if episode == 0 {
			next.episode = rec.episode
		}
	}
	if next.first.IsZero() {
		next.first = at
	}
	t.sent[key] = next
}

// IsQuietHours reports whether now falls in the quiet window.
func (t *Throttle) IsQuietHours(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.isQuietHours(now)
}

func (t *Throttle) isQuietHours(now time.Time) bool {
	if t.quietStart < 0 || t.quietEnd < 0 {
		return false
	}
	local := now.In(t.timezone)
	m := local.Hour()*60 + local.Minute()
	if t.quietStart <= t.quietEnd {
		return m >= t.quietStart && m < t.quietEnd
	}
	// Wraps midnight: e.g. 22:30 - 06:00.
	return m >= t.quietStart || m < t.quietEnd
}

// Reset clears the sent map (for testing).
func (t *Throttle) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = make(map[string]sentRecord)
}

// parseMinute converts "HH" or "HH:MM" to minute of day; -1 when empty
// or invalid (invalid values are recorded as warnings).
func (t *Throttle) parseMinute(field, s string) int {
	m := config.QuietHourMinute(s)
	if m < 0 && strings.TrimSpace(s) != "" {
		t.warnings = append(t.warnings, fmt.Sprintf(
			"invalid alerting %s %q (want HH:MM); quiet hours disabled", field, s))
	}
	return m
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// FormatDedupKey builds a dedup key from category and object.
func FormatDedupKey(category, objectIdentifier string) string {
	return fmt.Sprintf("%s:%s", category, objectIdentifier)
}

package alerting

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
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
)

// Throttle tracks deduplication and cooldown for alerts.
type Throttle struct {
	mu         sync.Mutex
	sent       map[string]sentRecord
	cooldown   map[string]time.Duration
	quietStart int // minute of day 0-1439, -1 = disabled
	quietEnd   int
	timezone   *time.Location
	now        func() time.Time
	warnings   []string
}

type sentRecord struct {
	at       time.Time
	severity string
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
	floor := time.Duration(cooldownMinutes) * time.Minute
	t.cooldown = map[string]time.Duration{
		"critical": maxDuration(5*time.Minute, floor),
		"warning":  maxDuration(30*time.Minute, floor),
		"info":     maxDuration(6*time.Hour, floor),
	}
	t.quietStart = t.parseMinute("quiet_hours_start", quietStart)
	t.quietEnd = t.parseMinute("quiet_hours_end", quietEnd)
	return t
}

// Warnings returns configuration problems found at construction.
func (t *Throttle) Warnings() []string { return t.warnings }

// Decide classifies an alert. Critical alerts bypass quiet hours;
// other severities are deferred, never dropped, during quiet hours.
func (t *Throttle) Decide(key, severity string) Decision {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if severity != "critical" && t.isQuietHours(now) {
		return DecisionDefer
	}
	rec, exists := t.sent[key]
	if !exists {
		return DecisionSend
	}
	// Allow escalation: lower rank = more severe.
	if severityRank(severity) < severityRank(rec.severity) {
		return DecisionSend
	}
	if now.Sub(rec.at) >= t.cooldownFor(severity) {
		return DecisionSend
	}
	return DecisionThrottled
}

// ShouldAlert returns true if the key should fire an alert now.
func (t *Throttle) ShouldAlert(key, severity string) bool {
	return t.Decide(key, severity) == DecisionSend
}

func (t *Throttle) cooldownFor(severity string) time.Duration {
	if cd, ok := t.cooldown[severity]; ok {
		return cd
	}
	return t.cooldown["info"]
}

// Record marks a key as sent now and evicts entries whose cooldown has
// fully expired, so the map stays bounded (G7-B32).
func (t *Throttle) Record(key, severity string) {
	t.RecordAt(key, severity, time.Time{})
}

// RecordAt marks a key as sent at ts (zero = now); used to seed the
// throttle from sage.alert_log after a restart.
func (t *Throttle) RecordAt(key, severity string, ts time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if ts.IsZero() {
		ts = now
	}
	maxCD := t.cooldownFor("info")
	for k, rec := range t.sent {
		if now.Sub(rec.at) >= maxCD {
			delete(t.sent, k)
		}
	}
	t.sent[key] = sentRecord{at: ts, severity: severity}
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
	m := parseMinuteOfDay(s)
	if m < 0 && strings.TrimSpace(s) != "" {
		t.warnings = append(t.warnings, fmt.Sprintf(
			"invalid alerting %s %q (want HH:MM); quiet hours disabled", field, s))
	}
	return m
}

func parseMinuteOfDay(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	parts := strings.SplitN(s, ":", 2)
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return -1
	}
	minute := 0
	if len(parts) == 2 {
		minute, err = strconv.Atoi(parts[1])
		if err != nil || minute < 0 || minute > 59 {
			return -1
		}
	}
	return h*60 + minute
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

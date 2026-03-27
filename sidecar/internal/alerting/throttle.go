package alerting

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Throttle tracks deduplication and cooldown for alerts.
type Throttle struct {
	mu         sync.Mutex
	sent       map[string]sentRecord
	cooldown   map[string]time.Duration
	quietStart int // hour 0-23, -1 = disabled
	quietEnd   int
	timezone   *time.Location
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

// NewThrottle creates a Throttle with severity-based cooldowns.
func NewThrottle(
	cooldownMinutes int,
	quietStart, quietEnd string,
	tz string,
) *Throttle {
	loc := time.UTC
	if tz != "" {
		if parsed, err := time.LoadLocation(tz); err == nil {
			loc = parsed
		}
	}

	floor := time.Duration(cooldownMinutes) * time.Minute
	cooldowns := map[string]time.Duration{
		"critical": maxDuration(5*time.Minute, floor),
		"warning":  maxDuration(30*time.Minute, floor),
		"info":     maxDuration(6*time.Hour, floor),
	}

	qs := parseHour(quietStart)
	qe := parseHour(quietEnd)

	return &Throttle{
		sent:       make(map[string]sentRecord),
		cooldown:   cooldowns,
		quietStart: qs,
		quietEnd:   qe,
		timezone:   loc,
	}
}

// ShouldAlert returns true if the key should fire an alert.
func (t *Throttle) ShouldAlert(key, severity string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.isQuietHours(time.Now()) {
		return false
	}

	rec, exists := t.sent[key]
	if !exists {
		return true
	}

	// Allow escalation: lower rank = more severe.
	if severityRank(severity) < severityRank(rec.severity) {
		return true
	}

	cd, ok := t.cooldown[severity]
	if !ok {
		cd = t.cooldown["info"]
	}
	return time.Since(rec.at) >= cd
}

// Record marks a key as sent now.
func (t *Throttle) Record(key, severity string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent[key] = sentRecord{at: time.Now(), severity: severity}
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
	h := now.In(t.timezone).Hour()
	if t.quietStart <= t.quietEnd {
		return h >= t.quietStart && h < t.quietEnd
	}
	// Wraps midnight: e.g. 22:00 - 06:00.
	return h >= t.quietStart || h < t.quietEnd
}

// Reset clears the sent map (for testing).
func (t *Throttle) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = make(map[string]sentRecord)
}

// parseHour extracts the hour from "HH:MM" format. Returns -1 on
// empty or invalid input.
func parseHour(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	parts := strings.SplitN(s, ":", 2)
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return -1
	}
	return h
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

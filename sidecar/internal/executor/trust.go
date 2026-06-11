package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// ShouldExecute determines whether a finding should be auto-remediated
// based on trust level, risk tier, ramp age, and maintenance window.
func ShouldExecute(
	f analyzer.Finding,
	cfg *config.Config,
	rampStart time.Time,
	isReplica bool,
	emergencyStop bool,
) bool {
	if emergencyStop || isReplica {
		return false
	}

	rampAge := time.Since(rampStart)

	switch cfg.Trust.Level {
	case "observation":
		return false

	case "advisory":
		// Advisory: only SAFE actions after 8-day ramp.
		return f.ActionRisk == "safe" &&
			cfg.Trust.Tier3Safe &&
			rampAge >= 8*24*time.Hour

	case "autonomous":
		switch f.ActionRisk {
		case "safe":
			return cfg.Trust.Tier3Safe &&
				rampAge >= 8*24*time.Hour
		case "moderate":
			return cfg.Trust.Tier3Moderate &&
				rampAge >= 31*24*time.Hour &&
				inMaintenanceWindow(cfg.Trust.MaintenanceWindow)
		case "high_risk":
			return false
		default:
			return false
		}

	default:
		return false
	}
}

// inMaintenanceWindow parses a maintenance-window expression and returns
// true if the current time falls inside it. Supports:
//   - "* * * * *" → always in window
//   - "0 2 * * *" → 02:00-03:00 daily (cron minute hour)
//   - "30 * * * *" → at minute 30 of every hour (1h window)
//   - "always" → always in window
//   - "HH:MM-HH:MM" → daily time range, wrapping midnight if start > end
//     (e.g. "22:00-02:00"). This human-friendly form was previously
//     parsed as garbage and treated as never-in-window (S4).
func inMaintenanceWindow(cronExpr string) bool {
	return inMaintenanceWindowAt(cronExpr, time.Now())
}

func inMaintenanceWindowAt(cronExpr string, now time.Time) bool {
	s := strings.TrimSpace(cronExpr)
	if s == "" {
		return false
	}

	// Friendly preset names (nights, weekends, off-hours, business-hours,
	// ...) expand to a canonical form so users who have never touched cron
	// can express a maintenance window.
	if canon, ok := maintenancePresets[strings.ToLower(s)]; ok {
		s = canon
	}
	switch strings.ToLower(s) {
	case "never", "off", "none", "disabled":
		return false
	case "always", "anytime", "24x7", "24/7":
		return true
	}

	// "HH:MM-HH:MM" daily time range.
	if r, ok := parseTimeRange(s); ok {
		return r.contains(now)
	}

	// Friendly "<days> [HH:MM-HH:MM]" — e.g. "weekdays 01:00-05:00",
	// "weekends", "Sat-Sun 02:00-06:00", "Mon,Wed,Fri 22:00-04:00".
	if in, ok := parseFriendlyWindowAt(s, now); ok {
		return in
	}

	parts := strings.Fields(s)
	if len(parts) < 2 {
		return false
	}

	// Parse minute field: "*" means any minute.
	minuteWild := parts[0] == "*"
	var minute int
	if !minuteWild {
		var err error
		minute, err = strconv.Atoi(parts[0])
		if err != nil {
			return false
		}
	}

	// Parse hour field: "*" means any hour.
	hourWild := parts[1] == "*"
	var hour int
	if !hourWild {
		var err error
		hour, err = strconv.Atoi(parts[1])
		if err != nil {
			return false
		}
	}

	// Both wildcards → always in window.
	if minuteWild && hourWild {
		return true
	}

	if hourWild {
		// Any hour, specific minute: 1-hour window starting at :minute.
		windowStart := time.Date(
			now.Year(), now.Month(), now.Day(),
			now.Hour(), minute, 0, 0, now.Location(),
		)
		windowEnd := windowStart.Add(1 * time.Hour)
		return !now.Before(windowStart) && now.Before(windowEnd)
	}

	// Specific hour (minute may be wild or specific).
	if minuteWild {
		minute = 0
	}
	windowStart := time.Date(
		now.Year(), now.Month(), now.Day(),
		hour, minute, 0, 0, now.Location(),
	)
	windowEnd := windowStart.Add(1 * time.Hour)

	return !now.Before(windowStart) && now.Before(windowEnd)
}

// maintenancePresets map friendly names to a canonical maintenance-window
// expression so a non-cron user can pick a sensible window by name.
var maintenancePresets = map[string]string{
	"nights":         "daily 22:00-06:00",
	"nightly":        "daily 22:00-06:00",
	"overnight":      "daily 22:00-06:00",
	"weeknights":     "weekdays 22:00-06:00",
	"off-hours":      "daily 20:00-08:00",
	"off-peak":       "daily 20:00-08:00",
	"business-hours": "weekdays 09:00-17:00",
	"weekends":       "weekends",
	"weekend":        "weekends",
	"weekdays":       "weekdays",
	"weekday":        "weekdays",
}

// parseFriendlyWindowAt handles "<days> [HH:MM-HH:MM]" expressions, where
// days is a name (weekdays, weekends, daily), a list (Sat,Sun), or a range
// (Mon-Fri). matched=false means the string is not this form, so the
// caller falls through to cron parsing.
func parseFriendlyWindowAt(s string, now time.Time) (inWindow, matched bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 2 {
		return false, false
	}
	dayMatch, ok := matchesDaySpec(fields[0], now.Weekday())
	if !ok {
		return false, false
	}
	if len(fields) == 1 {
		return dayMatch, true // all day on those days
	}
	r, ok := parseTimeRange(fields[1])
	if !ok {
		return false, false
	}
	return dayMatch && r.contains(now), true
}

// matchesDaySpec reports whether weekday wd is covered by a day spec like
// "daily", "weekdays", "weekends", "mon-fri", "sat,sun", or "mon,wed,fri".
// ok=false means the token is not a recognized day spec.
func matchesDaySpec(spec string, wd time.Weekday) (match, ok bool) {
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "daily", "everyday", "every-day", "all", "all-days":
		return true, true
	case "weekday", "weekdays":
		return wd >= time.Monday && wd <= time.Friday, true
	case "weekend", "weekends":
		return wd == time.Saturday || wd == time.Sunday, true
	}
	var set [7]bool
	any := false
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dash := strings.IndexByte(part, '-'); dash > 0 {
			a, oka := dayNum(part[:dash])
			b, okb := dayNum(part[dash+1:])
			if !oka || !okb {
				return false, false
			}
			for d := a; ; d = (d + 1) % 7 { // inclusive, wraps (fri-mon)
				set[d] = true
				if d == b {
					break
				}
			}
			any = true
		} else {
			d, okd := dayNum(part)
			if !okd {
				return false, false
			}
			set[d] = true
			any = true
		}
	}
	if !any {
		return false, false
	}
	return set[int(wd)], true
}

func dayNum(s string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sun", "sunday":
		return 0, true
	case "mon", "monday":
		return 1, true
	case "tue", "tues", "tuesday":
		return 2, true
	case "wed", "weds", "wednesday":
		return 3, true
	case "thu", "thur", "thurs", "thursday":
		return 4, true
	case "fri", "friday":
		return 5, true
	case "sat", "saturday":
		return 6, true
	}
	return 0, false
}

// timeRange is a daily window expressed in minutes since midnight.
type timeRange struct{ startMin, endMin int }

func (r timeRange) contains(now time.Time) bool {
	cur := now.Hour()*60 + now.Minute()
	if r.startMin == r.endMin {
		return false // zero-length window
	}
	if r.startMin < r.endMin {
		return cur >= r.startMin && cur < r.endMin
	}
	// Wraps midnight, e.g. 22:00-02:00.
	return cur >= r.startMin || cur < r.endMin
}

// parseTimeRange parses "HH:MM-HH:MM". Returns ok=false if the string
// isn't in that form (so the caller falls through to cron parsing).
func parseTimeRange(s string) (timeRange, bool) {
	if strings.ContainsAny(s, " \t") {
		return timeRange{}, false // whitespace ⇒ looks like cron
	}
	dash := strings.IndexByte(s, '-')
	if dash <= 0 || dash >= len(s)-1 {
		return timeRange{}, false
	}
	start, ok1 := parseHHMM(s[:dash])
	end, ok2 := parseHHMM(s[dash+1:])
	if !ok1 || !ok2 {
		return timeRange{}, false
	}
	return timeRange{startMin: start, endMin: end}, true
}

// parseHHMM parses "HH:MM" into minutes-since-midnight.
func parseHHMM(s string) (int, bool) {
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(s[:colon]))
	m, err2 := strconv.Atoi(strings.TrimSpace(s[colon+1:]))
	if err1 != nil || err2 != nil ||
		h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// CheckEmergencyStop queries sage.config for the emergency_stop flag.
// Returns true (stopped) if the value is "true".
//
// It fails CLOSED: only a genuine "no flag set" result (ErrNoRows)
// means "not stopped". Any other error — a transient connection blip,
// statement timeout, lock contention — returns true so that a database
// hiccup can never silently bypass an active emergency stop. The
// kill-switch must not depend on the database being healthy (H7).
func CheckEmergencyStop(ctx context.Context, pool *pgxpool.Pool) bool {
	var value string
	err := pool.QueryRow(ctx,
		"SELECT value FROM sage.config WHERE key = 'emergency_stop'",
	).Scan(&value)
	if err != nil {
		// No row → the flag was never set → not stopped.
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		// Unknown/transient error → fail closed.
		return true
	}
	return value == "true"
}

// SetEmergencyStop upserts the emergency_stop flag in sage.config.
func SetEmergencyStop(
	ctx context.Context, pool *pgxpool.Pool, stopped bool,
) error {
	val := "false"
	if stopped {
		val = "true"
	}

	_, err := pool.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.config (key, value, updated_at, updated_by)
		 VALUES ('emergency_stop', $1, now(), 'executor')
		 ON CONFLICT (key, COALESCE(database_id, 0)) DO UPDATE
		 SET value = $1, updated_at = now(), updated_by = 'executor'`,
		val,
	)
	if err != nil {
		return fmt.Errorf("setting emergency_stop to %s: %w", val, err)
	}
	return nil
}

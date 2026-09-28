package policy

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	// Embedded zone data: a window's time zone resolves identically on
	// every platform, with or without a system tzdata package.
	_ "time/tzdata"
)

// windowGrammar is appended to every parse error so the operator can fix
// the value without reading the source.
const windowGrammar = "expected always, a preset (nights, weeknights, weekdays, " +
	"weekends, business-hours, off-hours), \"[days] HH:MM-HH:MM\" (days: daily, " +
	"weekdays, weekends, Mon-Fri, sat,sun), or a 5-field cron with an optional " +
	"@<duration> (default @1h), optionally followed by an IANA time zone such as " +
	"\"weekdays 01:00-05:00 America/Chicago\""

// Window is a parsed maintenance window. It is the single window engine:
// policy documents and trust.maintenance_window both use it.
type Window struct {
	contains func(time.Time) bool
	// location is the window's time zone. Nil evaluates the instant in its
	// own location, which for the gate is the process-local clock.
	location *time.Location
}

// Contains reports whether at falls inside the window.
func (w Window) Contains(at time.Time) bool {
	if w.contains == nil {
		return false
	}
	if w.location != nil {
		at = at.In(w.location)
	}
	return w.contains(at)
}

// ParseWindow parses a maintenance-window expression:
//
//	always | anytime | 24x7 | 24/7
//	<preset>                      nights, weeknights, off-hours, business-hours, ...
//	<days>                        all day on those days (weekdays, sat,sun, Mon-Fri)
//	[<days>] HH:MM-HH:MM          a range that ends before it starts wraps
//	                              midnight and belongs to the day it starts on
//	<5-field cron> [@<duration>]  each matching minute opens a window of the
//	                              duration (default 1h, whole minutes, 1m-24h)
//
// Any form may end with an IANA time zone ("weekdays 01:00-05:00
// America/Chicago"); without one the instant's own location is used.
func ParseWindow(expression string) (Window, error) {
	window, err := parseWindow(expression)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: %w; %s", expression, err, windowGrammar)
	}
	return window, nil
}

func parseWindow(expression string) (Window, error) {
	fields := strings.Fields(expression)
	if len(fields) == 0 {
		return Window{}, errors.New("is empty")
	}
	location, fields, err := splitZone(fields)
	if err != nil {
		return Window{}, err
	}
	contains, err := parseWindowBody(fields)
	if err != nil {
		return Window{}, err
	}
	return Window{contains: contains, location: location}, nil
}

// splitZone removes a trailing time zone. Every other token of the grammar
// starts with a digit, '*' or '@', or is the only token, so a trailing token
// that starts with a letter can only be a zone.
func splitZone(fields []string) (*time.Location, []string, error) {
	last := fields[len(fields)-1]
	if len(fields) < 2 || !unicode.IsLetter([]rune(last)[0]) {
		return nil, fields, nil
	}
	if last == "Local" {
		return nil, nil, errors.New(`time zone "Local" is ambiguous; name the zone ` +
			`(for example UTC or America/Chicago) or omit it`)
	}
	location, err := time.LoadLocation(last)
	if err != nil {
		return nil, nil, fmt.Errorf("unknown time zone %q", last)
	}
	return location, fields[:len(fields)-1], nil
}

func parseWindowBody(fields []string) (func(time.Time) bool, error) {
	if len(fields) == 1 {
		if contains, ok := keywordWindow(fields[0]); ok {
			return contains, nil
		}
	}
	if strings.HasPrefix(fields[len(fields)-1], "@") || len(fields) == 5 {
		return parseCronWindow(fields)
	}
	return parseFriendlyWindow(fields)
}

func keywordWindow(token string) (func(time.Time) bool, bool) {
	switch strings.ToLower(token) {
	case "always", "anytime", "24x7", "24/7":
		return func(time.Time) bool { return true }, true
	}
	if expansion, ok := windowPresets[strings.ToLower(token)]; ok {
		contains, err := parseFriendlyWindow(strings.Fields(expansion))
		return contains, err == nil
	}
	return nil, false
}

// windowPresets name common windows so an operator never needs cron.
var windowPresets = map[string]string{
	"nights":         "daily 22:00-06:00",
	"nightly":        "daily 22:00-06:00",
	"overnight":      "daily 22:00-06:00",
	"weeknights":     "weekdays 22:00-06:00",
	"off-hours":      "daily 20:00-08:00",
	"off-peak":       "daily 20:00-08:00",
	"business-hours": "weekdays 09:00-17:00",
}

// parseFriendlyWindow handles "<days>", "HH:MM-HH:MM" and
// "<days> HH:MM-HH:MM".
func parseFriendlyWindow(fields []string) (func(time.Time) bool, error) {
	days, active := allDays(), fullDayRange()
	switch {
	case len(fields) == 1 && strings.Contains(fields[0], ":"):
		rangeValue, err := parseClockRange(fields[0])
		if err != nil {
			return nil, err
		}
		active = rangeValue
	case len(fields) == 1 || len(fields) == 2:
		parsed, ok := parseDaySpec(fields[0])
		if !ok {
			return nil, fmt.Errorf("%q is not a day list or preset", fields[0])
		}
		days = parsed
		if len(fields) == 2 {
			rangeValue, err := parseClockRange(fields[1])
			if err != nil {
				return nil, err
			}
			active = rangeValue
		}
	default:
		return nil, errors.New("is not a recognized window")
	}
	return dayWindow(days, active), nil
}

type minuteRange struct {
	start int
	end   int
}

func (r minuteRange) contains(minute int) bool {
	if r.start <= r.end {
		return minute >= r.start && minute < r.end
	}
	return minute >= r.start || minute < r.end
}

func parseClockRange(value string) (minuteRange, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return minuteRange{}, fmt.Errorf("range %q is not HH:MM-HH:MM", value)
	}
	start, err := parseClock(parts[0])
	if err != nil {
		return minuteRange{}, err
	}
	end, err := parseClock(parts[1])
	if err != nil {
		return minuteRange{}, err
	}
	if start == end {
		return minuteRange{}, fmt.Errorf("range %q has zero duration", value)
	}
	return minuteRange{start: start, end: end}, nil
}

func parseClock(value string) (int, error) {
	hourText, minuteText, found := strings.Cut(value, ":")
	hour, hourErr := parseDigits(hourText)
	minute, minuteErr := parseDigits(minuteText)
	if !found || hourErr != nil || minuteErr != nil || hour > 23 || minute > 59 {
		return 0, fmt.Errorf("clock %q is not HH:MM (00:00-23:59)", value)
	}
	return hour*60 + minute, nil
}

func parseDigits(value string) (int, error) {
	if value == "" || len(value) > 2 {
		return 0, errors.New("invalid")
	}
	number := 0
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, errors.New("invalid")
		}
		number = number*10 + int(char-'0')
	}
	return number, nil
}

// dayWindow matches active minutes on days. A range that wraps midnight
// belongs to the day it starts on: "weekdays 22:00-06:00" covers Friday
// night into Saturday morning, not Sunday night into Monday.
func dayWindow(days map[time.Weekday]bool, active minuteRange) func(time.Time) bool {
	return func(at time.Time) bool {
		minute := at.Hour()*60 + at.Minute()
		day := at.Weekday()
		if active.start > active.end && minute < active.end {
			day = (day + 6) % 7
		}
		return days[day] && active.contains(minute)
	}
}

func fullDayRange() minuteRange {
	return minuteRange{start: 0, end: 24 * 60}
}
